// Package catalog exposes read-only management utilities: a normalized view of
// the public models.dev price catalog and a live USD/CNY quote for display.
//
// Both services are strictly non-financial. They never write prices, rules or
// ledger rows, and a refresh can never reprice recorded usage; a normalized
// catalog is only ever previewed and then explicitly applied through the
// existing management API. Upstream hosts are fixed in this package, so no
// HTTP client can redirect a fetch to an arbitrary destination.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// cacheTTL bounds how long a fetched catalog is served before it is
	// considered stale and refreshed from upstream.
	cacheTTL = 6 * time.Hour
	// requestTimeout bounds one catalog fetch, including the fallback source.
	requestTimeout = 8 * time.Second
	// maxCatalogBytes bounds the accepted upstream response so a hostile or
	// broken upstream cannot exhaust memory.
	maxCatalogBytes = 16 << 20

	// microPerUnit is the number of integer microUSD in one USD. Catalog rates
	// are published as USD per million tokens; they are stored as integer
	// microUSD per million tokens, which is the ledger's money unit.
	microPerUnit = 1_000_000

	// contextOver200kThreshold is the token boundary models.dev uses for the
	// legacy context_over_200k cost variant.
	contextOver200kThreshold = 200_000
)

var errInvalidRate = errors.New("invalid catalog rate")

// source is one fixed upstream. Sources are never derived from a request.
type source struct {
	Name string
	URL  string
}

// defaultSources is the complete, approved upstream set. Any extra source can
// only be introduced by Go code, never by an HTTP caller.
func defaultSources() []source {
	return []source{
		{Name: "models.dev", URL: "https://models.dev/api.json"},
		{Name: "models.opencode.ai", URL: "https://models.opencode.ai/api.json"},
	}
}

// Rates are exact integer microUSD per million tokens. A nil field means the
// upstream catalog published no such rate; it is preserved as missing rather
// than coerced to zero, so an explicit zero and an absent rate stay distinct.
type Rates struct {
	Input      *int64 `json:"input,omitempty"`
	Output     *int64 `json:"output,omitempty"`
	CacheRead  *int64 `json:"cache_read,omitempty"`
	CacheWrite *int64 `json:"cache_write,omitempty"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
}

// ContextTier is a context-length price tier. Threshold is the token count at
// which the tier's rates apply.
type ContextTier struct {
	Threshold int64  `json:"threshold"`
	Label     string `json:"label"`
	Rates     Rates  `json:"rates"`
}

// Model is one normalized catalog entry. ID is the catalog's provider-qualified
// model identifier, preserved exactly; Provider is the catalog provider key.
type Model struct {
	ID           string        `json:"id"`
	Provider     string        `json:"provider"`
	Name         string        `json:"name"`
	Rates        Rates         `json:"rates"`
	ContextTiers []ContextTier `json:"context_tiers,omitempty"`
}

// Snapshot is the management response body. Stale is set only for a cached
// response served because the upstream refresh failed; Error then carries the
// failure while the previously fetched items stay untouched.
type Snapshot struct {
	Items     []Model   `json:"items"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	Cached    bool      `json:"cached"`
	Stale     bool      `json:"stale"`
	Error     string    `json:"error,omitempty"`
}

// Service fetches, normalizes and caches the models.dev catalog. It is safe for
// concurrent use; one Service instance is intended per management Handler so a
// test never observes another instance's cache.
type Service struct {
	http    *http.Client
	sources []source
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	fetchMu sync.Mutex
	cache[Snapshot]
}

// NewService builds a catalog service over the fixed, approved upstreams. A nil
// client is replaced by one bounded by requestTimeout; redirects are always
// refused regardless of the supplied client.
func NewService(httpClient *http.Client) *Service {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Service{
		http:    httpClient,
		sources: defaultSources(),
		ttl:     cacheTTL,
		timeout: requestTimeout,
		now:     time.Now,
	}
}

func (s *Service) nowTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Service) cacheDuration() time.Duration {
	if s.ttl > 0 {
		return s.ttl
	}
	return cacheTTL
}

func (s *Service) fetchTimeout() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return requestTimeout
}

// requestClient returns an http.Client that refuses redirects, so an approved
// upstream cannot 3xx the fetch to a loopback, private or otherwise unapproved
// destination. A caller-supplied client is cloned and forced onto the same
// policy: the redirect decision is never taken from configuration or a request.
func requestClient(base *http.Client, timeout time.Duration) *http.Client {
	if base == nil {
		return &http.Client{Timeout: timeout, CheckRedirect: denyRedirect}
	}
	clone := *base
	clone.CheckRedirect = denyRedirect
	return &clone
}

// denyRedirect aborts any redirect instead of following it. Every error path
// here leaves the fetch failed, which the cache already turns into an explicit
// stale response rather than a silent fallback.
func denyRedirect(*http.Request, []*http.Request) error {
	return errors.New("upstream redirect rejected")
}

// Get returns the normalized catalog, serving the per-instance cache while it is
// fresh. refresh=true forces a refetch, except that concurrent refreshes
// coalesce: a caller that started while a refresh was already in flight shares
// that refresh's result instead of issuing a second request.
//
// When an upstream refresh fails and a previous catalog exists, that catalog is
// returned with Cached, Stale and Error set. No synthetic prices or zero rates
// are ever invented.
func (s *Service) Get(ctx context.Context, refresh bool) Snapshot {
	if s == nil {
		return unavailable("model catalog service is nil", time.Now())
	}
	if !refresh {
		if snap, ok := s.fresh(s.nowTime()); ok {
			snap.Cached = true
			return snap
		}
	}
	arrival := s.nowTime()
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if !refresh {
		if snap, ok := s.fresh(s.nowTime()); ok {
			snap.Cached = true
			return snap
		}
	} else if snap, ok := s.completedSince(arrival); ok {
		snap.Cached = true
		return snap
	}

	previous, hasPrevious := s.previous()
	items, name, err := s.fetch(ctx)
	if err != nil {
		if hasPrevious {
			previous.Cached = true
			previous.Stale = true
			previous.Error = err.Error()
			return previous
		}
		return unavailable(err.Error(), s.nowTime())
	}
	now := s.nowTime()
	snap := Snapshot{Items: items, Source: name, FetchedAt: now.UTC()}
	s.store(snap, snap.FetchedAt, s.cacheDuration())
	return snap
}

// fetch tries the fixed sources in order within one bounded budget and returns
// the first normalized catalog. Redirects are refused, so a fixed, approved URL
// can never be bounced to another host.
func (s *Service) fetch(ctx context.Context) ([]Model, string, error) {
	client := requestClient(s.http, s.fetchTimeout())
	ctx, cancel := context.WithTimeout(ctx, s.fetchTimeout())
	defer cancel()

	sources := s.sources
	if len(sources) == 0 {
		return nil, "", errors.New("no model catalog sources configured")
	}
	failures := make([]string, 0, len(sources))
	for _, src := range sources {
		items, err := fetchSource(ctx, client, src)
		if err != nil {
			failures = append(failures, src.Name+": "+err.Error())
			continue
		}
		return items, src.Name, nil
	}
	return nil, "", errors.New("model catalog unavailable: " + strings.Join(failures, "; "))
}

func fetchSource(ctx context.Context, client *http.Client, src source) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cliproxyapi-credit-manager/catalog")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxCatalogBytes {
		return nil, errors.New("catalog too large")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, errors.New("http " + res.Status)
	}
	return normalize(body)
}

func unavailable(message string, now time.Time) Snapshot {
	return Snapshot{
		Items:     []Model{},
		Source:    "unavailable",
		FetchedAt: now.UTC(),
		Error:     message,
	}
}

// upstreamProvider mirrors the models.dev provider envelope. Cost is kept as
// raw JSON so each rate is decoded with json.Number and converted exactly.
type upstreamProvider struct {
	Models map[string]upstreamModel `json:"models"`
}

type upstreamModel struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Cost json.RawMessage `json:"cost"`
}

type rateFields struct {
	Input      *json.Number `json:"input"`
	Output     *json.Number `json:"output"`
	CacheRead  *json.Number `json:"cache_read"`
	CacheWrite *json.Number `json:"cache_write"`
	Reasoning  *json.Number `json:"reasoning"`
}

type costFields struct {
	rateFields
	Tiers           []tierFields `json:"tiers"`
	ContextOver200k *rateFields  `json:"context_over_200k"`
}

type tierFields struct {
	rateFields
	Tier struct {
		Type string       `json:"type"`
		Size *json.Number `json:"size"`
	} `json:"tier"`
}

// normalize decodes a models.dev payload into a deterministic, normalized list.
// A payload without a single model is treated as malformed so an empty or
// truncated response never silently replaces a good catalog.
func normalize(body []byte) ([]Model, error) {
	var providers map[string]upstreamProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, errors.New("malformed catalog")
	}
	if len(providers) == 0 {
		return nil, errors.New("malformed catalog")
	}
	items := make([]Model, 0, 1024)
	for provider, entry := range providers {
		for modelKey, upstream := range entry.Models {
			model, err := normalizeModel(provider, modelKey, upstream)
			if err != nil {
				return nil, err
			}
			items = append(items, model)
		}
	}
	if len(items) == 0 {
		return nil, errors.New("malformed catalog")
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Provider != items[j].Provider {
			return items[i].Provider < items[j].Provider
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

func normalizeModel(provider, modelKey string, upstream upstreamModel) (Model, error) {
	id := upstream.ID
	if id == "" {
		id = modelKey
	}
	if id == "" {
		return Model{}, errors.New("malformed catalog")
	}
	name := upstream.Name
	if name == "" {
		name = id
	}
	rates, tiers, err := normalizeCost(upstream.Cost)
	if err != nil {
		return Model{}, err
	}
	return Model{
		ID:           id,
		Provider:     provider,
		Name:         name,
		Rates:        rates,
		ContextTiers: tiers,
	}, nil
}

// normalizeCost maps the models.dev cost object. Input/Output/CacheRead/
// CacheWrite are mapped directly and Reasoning from the reasoning rate. The
// context_over_200k variant is folded into the context tiers, preferring an
// explicit tier at the same threshold.
func normalizeCost(raw json.RawMessage) (Rates, []ContextTier, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return Rates{}, nil, nil
	}
	var cost costFields
	if err := json.Unmarshal(trimmed, &cost); err != nil {
		return Rates{}, nil, errors.New("malformed catalog cost")
	}
	rates, err := normalizeRates(cost.rateFields)
	if err != nil {
		return Rates{}, nil, err
	}

	thresholds := make(map[int64]Rates)
	order := make([]int64, 0, 4)
	for _, tier := range cost.Tiers {
		if !strings.EqualFold(strings.TrimSpace(tier.Tier.Type), "context") {
			continue
		}
		threshold, err := tokenCount(tier.Tier.Size)
		if err != nil {
			return Rates{}, nil, err
		}
		if threshold <= 0 {
			return Rates{}, nil, errInvalidRate
		}
		tierRates, err := normalizeRates(tier.rateFields)
		if err != nil {
			return Rates{}, nil, err
		}
		if _, seen := thresholds[threshold]; !seen {
			order = append(order, threshold)
		}
		thresholds[threshold] = tierRates
	}
	if cost.ContextOver200k != nil {
		if _, seen := thresholds[contextOver200kThreshold]; !seen {
			tierRates, err := normalizeRates(*cost.ContextOver200k)
			if err != nil {
				return Rates{}, nil, err
			}
			order = append(order, contextOver200kThreshold)
			thresholds[contextOver200kThreshold] = tierRates
		}
	}
	if len(order) == 0 {
		return rates, nil, nil
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	tiers := make([]ContextTier, 0, len(order))
	for _, threshold := range order {
		tiers = append(tiers, ContextTier{
			Threshold: threshold,
			Label:     contextLabel(threshold),
			Rates:     thresholds[threshold],
		})
	}
	return rates, tiers, nil
}

func normalizeRates(fields rateFields) (Rates, error) {
	input, err := microUSD(fields.Input)
	if err != nil {
		return Rates{}, err
	}
	output, err := microUSD(fields.Output)
	if err != nil {
		return Rates{}, err
	}
	cacheRead, err := microUSD(fields.CacheRead)
	if err != nil {
		return Rates{}, err
	}
	cacheWrite, err := microUSD(fields.CacheWrite)
	if err != nil {
		return Rates{}, err
	}
	reasoning, err := microUSD(fields.Reasoning)
	if err != nil {
		return Rates{}, err
	}
	return Rates{
		Input:      input,
		Output:     output,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		Reasoning:  reasoning,
	}, nil
}

// microUSD converts one USD-per-million-token price to integer microUSD per
// million tokens. A nil number stays nil; a negative, non-numeric or
// overflowing value is rejected rather than clamped or zeroed.
func microUSD(n *json.Number) (*int64, error) {
	if n == nil {
		return nil, nil
	}
	value, err := exactMicro(n.String())
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// exactMicro rounds a non-negative decimal to the nearest integer microUSD,
// half away from zero, using exact rational arithmetic so a value such as 0.033
// becomes exactly 33000.
func exactMicro(raw string) (int64, error) {
	rat, ok := new(big.Rat).SetString(strings.TrimSpace(raw))
	if !ok || rat.Sign() < 0 {
		return 0, errInvalidRate
	}
	scaled := new(big.Int).Mul(rat.Num(), big.NewInt(microPerUnit))
	quotient, remainder := new(big.Int).QuoRem(scaled, rat.Denom(), new(big.Int))
	remainder.Abs(remainder)
	remainder.Lsh(remainder, 1) // half-point comparison without division
	if remainder.Cmp(rat.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, errInvalidRate
	}
	return quotient.Int64(), nil
}

// tokenCount reads a context threshold. Thresholds are token counts and must be
// exact, non-negative integers.
func tokenCount(n *json.Number) (int64, error) {
	if n == nil {
		return 0, errInvalidRate
	}
	rat, ok := new(big.Rat).SetString(strings.TrimSpace(n.String()))
	if !ok || rat.Sign() < 0 || !rat.IsInt() {
		return 0, errInvalidRate
	}
	if !rat.Num().IsInt64() {
		return 0, errInvalidRate
	}
	return rat.Num().Int64(), nil
}

// contextLabel mirrors the management UI's compact context label.
func contextLabel(tokens int64) string {
	if tokens >= 1000 && tokens%1000 == 0 {
		return strconv.FormatInt(tokens/1000, 10) + "K"
	}
	return strconv.FormatInt(tokens, 10)
}
