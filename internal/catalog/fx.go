package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// fxCacheTTL bounds how long a fetched quote is served before it is
	// refreshed. It is short because the value is a live market rate.
	fxCacheTTL = time.Hour
	// fxRequestTimeout bounds one quote fetch.
	fxRequestTimeout = 8 * time.Second
	// maxRateBytes bounds the accepted quote response.
	maxRateBytes = 1 << 20

	// The quote is USD→CNY; a value outside this band is rejected as a bad or
	// hostile quote rather than displayed.
	minUSDToCNYNumerator = 4
	maxUSDToCNYNumerator = 12
)

// erAPIURL is the single approved quote provider, matching the provider the
// original management UI used. It is fixed here and never read from a request.
const erAPIURL = "https://open.er-api.com/v6/latest/USD"

func fxSources() []source {
	return []source{{Name: "er-api", URL: erAPIURL}}
}

// Rate is the management response body for a USD/CNY quote. Rate is the exact
// decimal string published by the provider; it is empty when no quote is
// available, in which case the UI keeps its manual fallback value.
type Rate struct {
	Base      string    `json:"base"`
	Quote     string    `json:"quote"`
	Rate      string    `json:"rate,omitempty"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	Cached    bool      `json:"cached"`
	Stale     bool      `json:"stale"`
	Error     string    `json:"error,omitempty"`
}

// FXService fetches and caches the live USD/CNY quote for display only. It is
// safe for concurrent use; one instance is intended per management Handler.
type FXService struct {
	http    *http.Client
	sources []source
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	fetchMu sync.Mutex
	cache[Rate]
}

// NewFXService builds a quote service over the fixed, approved provider.
// Redirects are always refused regardless of the supplied client.
func NewFXService(httpClient *http.Client) *FXService {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: fxRequestTimeout}
	}
	return &FXService{
		http:    httpClient,
		sources: fxSources(),
		ttl:     fxCacheTTL,
		timeout: fxRequestTimeout,
		now:     time.Now,
	}
}

func (s *FXService) nowTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *FXService) cacheDuration() time.Duration {
	if s.ttl > 0 {
		return s.ttl
	}
	return fxCacheTTL
}

func (s *FXService) fetchTimeout() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return fxRequestTimeout
}

// Get returns the current USD/CNY quote, serving the cache while fresh. Stale
// is set when a previously fetched quote is served because the refresh failed;
// even then no fallback rate is invented, so the UI can keep its manual value.
func (s *FXService) Get(ctx context.Context, refresh bool) Rate {
	if s == nil {
		return fxUnavailable("exchange rate service is nil", time.Now())
	}
	if !refresh {
		if rate, ok := s.fresh(s.nowTime()); ok {
			rate.Cached = true
			return rate
		}
	}
	arrival := s.nowTime()
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if !refresh {
		if rate, ok := s.fresh(s.nowTime()); ok {
			rate.Cached = true
			return rate
		}
	} else if rate, ok := s.completedSince(arrival); ok {
		rate.Cached = true
		return rate
	}

	previous, hasPrevious := s.previous()
	rate, err := s.fetch(ctx)
	if err != nil {
		if hasPrevious {
			previous.Cached = true
			previous.Stale = true
			previous.Error = err.Error()
			return previous
		}
		return fxUnavailable(err.Error(), s.nowTime())
	}
	now := s.nowTime()
	rate.FetchedAt = now.UTC()
	s.store(rate, rate.FetchedAt, s.cacheDuration())
	return rate
}

func (s *FXService) fetch(ctx context.Context) (Rate, error) {
	client := requestClient(s.http, s.fetchTimeout())
	ctx, cancel := context.WithTimeout(ctx, s.fetchTimeout())
	defer cancel()

	sources := s.sources
	if len(sources) == 0 {
		return Rate{}, errors.New("no exchange rate sources configured")
	}
	failures := make([]string, 0, len(sources))
	for _, src := range sources {
		rate, err := fetchRate(ctx, client, src)
		if err != nil {
			failures = append(failures, src.Name+": "+err.Error())
			continue
		}
		return rate, nil
	}
	return Rate{}, errors.New("exchange rate unavailable: " + strings.Join(failures, "; "))
}

func fetchRate(ctx context.Context, client *http.Client, src source) (Rate, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return Rate{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cliproxyapi-credit-manager/catalog")
	res, err := client.Do(req)
	if err != nil {
		return Rate{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxRateBytes+1))
	if err != nil {
		return Rate{}, err
	}
	if len(body) > maxRateBytes {
		return Rate{}, errors.New("exchange rate response too large")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return Rate{}, errors.New("http " + res.Status)
	}
	value, err := parseERAPI(body)
	if err != nil {
		return Rate{}, err
	}
	return Rate{Base: "USD", Quote: "CNY", Rate: value, Source: src.Name}, nil
}

// parseERAPI reads the USD/CNY rate from the open.er-api.com envelope. The
// decimal value is returned exactly as published after validating that it is a
// sane USD/CNY quote.
func parseERAPI(body []byte) (string, error) {
	var payload struct {
		Result string                 `json:"result"`
		Rates  map[string]json.Number `json:"rates"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", errors.New("malformed exchange rate")
	}
	if payload.Result != "" && !strings.EqualFold(payload.Result, "success") {
		return "", errors.New("exchange rate provider returned " + payload.Result)
	}
	raw, ok := payload.Rates["CNY"]
	if !ok {
		raw, ok = payload.Rates["cny"]
	}
	if !ok {
		return "", errors.New("exchange rate missing CNY")
	}
	value := strings.TrimSpace(raw.String())
	rat, ok := new(big.Rat).SetString(value)
	if !ok || rat.Sign() <= 0 {
		return "", errors.New("malformed exchange rate")
	}
	if rat.Cmp(big.NewRat(minUSDToCNYNumerator, 1)) < 0 || rat.Cmp(big.NewRat(maxUSDToCNYNumerator, 1)) > 0 {
		return "", errors.New("exchange rate out of range")
	}
	return value, nil
}

func fxUnavailable(message string, now time.Time) Rate {
	return Rate{
		Base:      "USD",
		Quote:     "CNY",
		Source:    "unavailable",
		FetchedAt: now.UTC(),
		Error:     message,
	}
}
