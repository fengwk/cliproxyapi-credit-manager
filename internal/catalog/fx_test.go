package catalog

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const fxFixture = `{"result":"success","provider":"https://www.exchangerate-api.com","rates":{"USD":1,"CNY":7.1234}}`

// testFXService builds a quote service over one local test provider.
func testFXService(t *testing.T, handler http.HandlerFunc, clock *fakeClock) (*FXService, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	service := NewFXService(server.Client())
	service.sources = []source{{Name: "test", URL: server.URL}}
	service.ttl = time.Hour
	service.timeout = 2 * time.Second
	if clock != nil {
		service.now = clock.Now
	}
	return service, &calls
}

// The decimal rate is presented exactly as published, never rounded through a
// float, and carries the fixed USD/CNY identity.
func TestExchangeRatePresentsExactDecimal(t *testing.T) {
	service, calls := testFXService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fxFixture))
	}, nil)

	rate := service.Get(context.Background(), false)
	if rate.Error != "" || rate.Rate != "7.1234" || rate.Base != "USD" || rate.Quote != "CNY" {
		t.Fatalf("rate = %+v", rate)
	}
	if rate.Source != "test" || rate.Cached || rate.Stale || calls.Load() != 1 {
		t.Fatalf("rate = %+v calls=%d", rate, calls.Load())
	}
}

// A provider error, a missing rate or an out-of-range value is rejected rather
// than shown.
func TestParseERAPIRejectsBadQuotes(t *testing.T) {
	for name, body := range map[string]string{
		"provider-error": `{"result":"error","rates":{"CNY":7.1}}`,
		"missing":        `{"result":"success","rates":{"EUR":0.9}}`,
		"zero":           `{"result":"success","rates":{"CNY":0}}`,
		"negative":       `{"result":"success","rates":{"CNY":-7.1}}`,
		"too-low":        `{"result":"success","rates":{"CNY":3.9}}`,
		"too-high":       `{"result":"success","rates":{"CNY":12.1}}`,
		"not-number":     `{"result":"success","rates":{"CNY":"seven"}}`,
		"malformed":      `{`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseERAPI([]byte(body)); err == nil {
				t.Fatalf("parseERAPI(%s) accepted", body)
			}
		})
	}
	if got, err := parseERAPI([]byte(`{"result":"success","rates":{"cny":7.2}}`)); err != nil || got != "7.2" {
		t.Fatalf("lowercase cny = %q %v", got, err)
	}
}

// The cache serves within the TTL, refetches on expiry, and reports a stale
// previous quote when the provider fails. No fallback rate is ever invented.
func TestExchangeRateCacheAndStale(t *testing.T) {
	clock := newFakeClock()
	var healthy atomic.Bool
	healthy.Store(true)
	service, calls := testFXService(t, func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(fxFixture))
	}, clock)

	first := service.Get(context.Background(), false)
	if first.Error != "" || first.Rate != "7.1234" {
		t.Fatalf("first = %+v", first)
	}
	cached := service.Get(context.Background(), false)
	if !cached.Cached || calls.Load() != 1 {
		t.Fatalf("cached = %+v calls=%d", cached, calls.Load())
	}

	healthy.Store(false)
	clock.advance(2 * time.Hour)
	stale := service.Get(context.Background(), false)
	if !stale.Cached || !stale.Stale || stale.Error == "" || stale.Rate != "7.1234" {
		t.Fatalf("stale = %+v", stale)
	}
	if !stale.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("stale fetch time changed: %v vs %v", stale.FetchedAt, first.FetchedAt)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

// Without any previous quote a failed provider yields no rate at all.
func TestExchangeRateFailsClosed(t *testing.T) {
	var nilService *FXService
	if rate := nilService.Get(context.Background(), false); rate.Error == "" || rate.Rate != "" {
		t.Fatalf("nil service = %+v", rate)
	}
	service, _ := testFXService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}, nil)
	rate := service.Get(context.Background(), false)
	if rate.Error == "" || rate.Rate != "" || rate.Source != "unavailable" {
		t.Fatalf("failed service = %+v", rate)
	}
}

// Oversized responses are refused before parsing.
func TestExchangeRateBoundsResponse(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, maxRateBytes+1)
	service, _ := testFXService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}, nil)
	if rate := service.Get(context.Background(), false); rate.Error == "" || rate.Rate != "" {
		t.Fatalf("oversize accepted: %+v", rate)
	}
}

// A redirect from the approved quote provider is refused, so a fixed URL can
// never be bounced to a loopback or private destination.
func TestExchangeRateRefusesRedirect(t *testing.T) {
	var reached atomic.Int32
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte(fxFixture))
	}))
	t.Cleanup(private.Close)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL, http.StatusFound)
	}))
	t.Cleanup(upstream.Close)

	service := NewFXService(upstream.Client())
	service.sources = []source{{Name: "test", URL: upstream.URL}}

	rate := service.Get(context.Background(), false)
	if rate.Error == "" || rate.Rate != "" {
		t.Fatalf("redirect was not refused: %+v", rate)
	}
	if reached.Load() != 0 {
		t.Fatalf("redirect target was contacted %d times", reached.Load())
	}
}

// A slow provider is abandoned within the configured timeout instead of
// blocking the management request.
func TestExchangeRateTimeoutIsBounded(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // runs before the test server closes
	service, _ := testFXService(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, nil)
	service.timeout = 20 * time.Millisecond

	started := time.Now()
	rate := service.Get(context.Background(), false)
	if rate.Error == "" || rate.Rate != "" {
		t.Fatalf("timeout was not reported: %+v", rate)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("fetch was not bounded: %v", elapsed)
	}
}
