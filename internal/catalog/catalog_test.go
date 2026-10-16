package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixture mirrors the real models.dev envelope, including an explicit zero, a
// missing rate, a reasoning rate, a tier array and a context_over_200k variant
// that disagrees with its matching tier.
const fixture = `{
  "models.dev": {},
  "deepinfra": {
    "models": {
      "anthropic/claude-fable-5": {
        "id": "anthropic/claude-fable-5",
        "name": "Claude Fable 5",
        "cost": {"input": 10, "output": 50}
      },
      "tencent/Hy3": {
        "id": "tencent/Hy3",
        "name": "Hy3",
        "cost": {"input": 0.13, "output": 0.53, "cache_read": 0.033}
      },
      "free/model": {
        "id": "free/model",
        "name": "Free",
        "cost": {"input": 0, "output": 0}
      },
      "reasoner": {
        "id": "reasoner",
        "name": "Reasoner",
        "cost": {"input": 0.15, "output": 0.6, "reasoning": 0.6, "cache_read": 0.003, "cache_write": 0.85}
      },
      "tiered": {
        "id": "tiered",
        "name": "Tiered",
        "cost": {"input": 2.5, "output": 7.5, "cache_read": 0.5,
          "tiers": [
            {"input": 5, "output": 15, "cache_read": 1, "tier": {"type": "context", "size": 32000}},
            {"input": 6.25, "output": 18.5, "cache_read": 1.25, "tier": {"type": "context", "size": 128000}}
          ]}
      },
      "c200k": {
        "id": "c200k",
        "name": "C200k",
        "cost": {"input": 2, "output": 12, "cache_read": 0.2,
          "tiers": [{"input": 4.5, "output": 18, "cache_read": 0.4, "tier": {"type": "context", "size": 200000}}],
          "context_over_200k": {"input": 4, "output": 18, "cache_read": 0.4}}
      },
      "namefallback": {"cost": {"input": 1}}
    }
  }
}`

// fakeClock is a strictly increasing clock so a fetch's completion is always
// later than any caller that started before it, exactly like a real clock.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	calls int64
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Nanosecond)
	c.calls++
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) callCount() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// catalogServer returns a service bound to one local test upstream; the fixed
// source list is overridden here, never through any request.
func testService(t *testing.T, handler http.HandlerFunc, clock *fakeClock) (*Service, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	service := NewService(server.Client())
	service.sources = []source{{Name: "test", URL: server.URL}}
	service.ttl = time.Hour
	service.timeout = 2 * time.Second
	if clock != nil {
		service.now = clock.Now
	}
	return service, &calls
}

func findModel(t *testing.T, items []Model, id string) Model {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("model %q missing from %d items", id, len(items))
	return Model{}
}

func wantMicro(t *testing.T, label string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", label, got, want)
	}
}

func wantMissing(t *testing.T, label string, got *int64) {
	t.Helper()
	if got != nil {
		t.Fatalf("%s = %d, want missing", label, *got)
	}
}

// The approved upstreams are fixed in code and can never be chosen by a caller.
func TestDefaultSourcesAreFixed(t *testing.T) {
	got := defaultSources()
	want := []source{
		{Name: "models.dev", URL: "https://models.dev/api.json"},
		{Name: "models.opencode.ai", URL: "https://models.opencode.ai/api.json"},
	}
	if len(got) != len(want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("source[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if fx := fxSources(); len(fx) != 1 || fx[0].Name != "er-api" || fx[0].URL != erAPIURL {
		t.Fatalf("fx sources = %v, want the fixed er-api provider", fx)
	}
}

// Prices map to exact integer microUSD with explicit zero kept and unknown
// rates still missing, and provider-qualified ids preserved verbatim.
func TestNormalizeKeepsExactRatesAndIds(t *testing.T) {
	service, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fixture))
	}, nil)

	snapshot := service.Get(context.Background(), false)
	if snapshot.Error != "" || len(snapshot.Items) != 7 || snapshot.Source != "test" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Cached || snapshot.Stale {
		t.Fatalf("fresh snapshot marked cached/stale: %+v", snapshot)
	}
	for i := 1; i < len(snapshot.Items); i++ {
		previous, current := snapshot.Items[i-1], snapshot.Items[i]
		if previous.Provider > current.Provider ||
			(previous.Provider == current.Provider && previous.ID > current.ID) {
			t.Fatalf("items are not deterministically sorted at %d", i)
		}
	}

	fable := findModel(t, snapshot.Items, "anthropic/claude-fable-5")
	if fable.Provider != "deepinfra" || fable.Name != "Claude Fable 5" {
		t.Fatalf("fable = %+v", fable)
	}
	wantMicro(t, "fable input", fable.Rates.Input, 10_000_000)
	wantMicro(t, "fable output", fable.Rates.Output, 50_000_000)
	wantMissing(t, "fable cache_read", fable.Rates.CacheRead)
	wantMissing(t, "fable cache_write", fable.Rates.CacheWrite)

	hy3 := findModel(t, snapshot.Items, "tencent/Hy3")
	wantMicro(t, "hy3 cache_read", hy3.Rates.CacheRead, 33_000) // 0.033 exactly
	wantMicro(t, "hy3 input", hy3.Rates.Input, 130_000)

	free := findModel(t, snapshot.Items, "free/model")
	wantMicro(t, "free input", free.Rates.Input, 0)
	wantMicro(t, "free output", free.Rates.Output, 0)

	reasoner := findModel(t, snapshot.Items, "reasoner")
	wantMicro(t, "reasoner reasoning", reasoner.Rates.Reasoning, 600_000)
	wantMicro(t, "reasoner cache_write", reasoner.Rates.CacheWrite, 850_000)
	wantMicro(t, "reasoner cache_read", reasoner.Rates.CacheRead, 3_000)

	fallback := findModel(t, snapshot.Items, "namefallback")
	if fallback.Name != "namefallback" || fallback.Provider != "deepinfra" {
		t.Fatalf("fallback model = %+v", fallback)
	}

	tiered := findModel(t, snapshot.Items, "tiered")
	if len(tiered.ContextTiers) != 2 {
		t.Fatalf("tiered tiers = %+v", tiered.ContextTiers)
	}
	if tiered.ContextTiers[0].Threshold != 32_000 || tiered.ContextTiers[0].Label != "32K" {
		t.Fatalf("first tier = %+v", tiered.ContextTiers[0])
	}
	wantMicro(t, "tier input", tiered.ContextTiers[1].Rates.Input, 6_250_000)
	if tiered.ContextTiers[1].Threshold != 128_000 || tiered.ContextTiers[1].Label != "128K" {
		t.Fatalf("second tier = %+v", tiered.ContextTiers[1])
	}

	// The explicit tier at 200000 wins over the legacy context_over_200k copy.
	c200k := findModel(t, snapshot.Items, "c200k")
	if len(c200k.ContextTiers) != 1 || c200k.ContextTiers[0].Threshold != 200_000 {
		t.Fatalf("c200k tiers = %+v", c200k.ContextTiers)
	}
	if c200k.ContextTiers[0].Label != "200K" {
		t.Fatalf("c200k label = %q", c200k.ContextTiers[0].Label)
	}
	wantMicro(t, "c200k tier input", c200k.ContextTiers[0].Rates.Input, 4_500_000)
	// The base rates are untouched by the tier.
	wantMicro(t, "c200k input", c200k.Rates.Input, 2_000_000)
}

// A negative, non-numeric or overflowing rate is rejected rather than clamped,
// zeroed or silently dropped.
func TestNormalizeRejectsInvalidRates(t *testing.T) {
	for name, cost := range map[string]string{
		"negative":        `{"input": -1, "output": 5}`,
		"overflow":        `{"input": 99999999999999999999999}`,
		"not-number":      `{"input": "free"}`,
		"tier-zero":       `{"input": 1, "tiers": [{"input": 2, "tier": {"type": "context", "size": 0}}]}`,
		"tier-fractional": `{"input": 1, "tiers": [{"input": 2, "tier": {"type": "context", "size": 1.5}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := fmt.Sprintf(`{"p":{"models":{"m":{"id":"m","name":"m","cost":%s}}}}`, cost)
			service, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}, nil)
			snapshot := service.Get(context.Background(), false)
			if snapshot.Error == "" || len(snapshot.Items) != 0 {
				t.Fatalf("invalid cost was accepted: %+v", snapshot)
			}
		})
	}
}

// The upstream response is bounded and a bad status or malformed payload never
// yields a catalog.
func TestFetchBoundsStatusAndMalformed(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		payload := bytes.Repeat([]byte{'x'}, maxCatalogBytes+1)
		service, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(payload)
		}, nil)
		snapshot := service.Get(context.Background(), false)
		if snapshot.Error == "" || len(snapshot.Items) != 0 {
			t.Fatalf("oversize payload accepted: %+v", snapshot)
		}
	})
	for name, handler := range map[string]http.HandlerFunc{
		"http-error": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		"malformed":  func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{not json")) },
		"empty":      func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) },
		"no-models":  func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"p":{"id":"p"}}`)) },
	} {
		t.Run(name, func(t *testing.T) {
			service, _ := testService(t, handler, nil)
			snapshot := service.Get(context.Background(), false)
			if snapshot.Error == "" || len(snapshot.Items) != 0 || snapshot.Source != "unavailable" {
				t.Fatalf("snapshot = %+v", snapshot)
			}
		})
	}
}

// The fallback source is used only when the first fixed source fails.
func TestFetchFallsBackToSecondSource(t *testing.T) {
	service, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}, nil)
	good, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fixture))
	}, nil)
	// Rebuild the fallback list explicitly: first a failing URL, then the good
	// one, proving the second approved source is tried.
	service.sources = append(service.sources, good.sources[0])

	snapshot := service.Get(context.Background(), false)
	if snapshot.Error != "" || len(snapshot.Items) != 7 || snapshot.Source != "test" {
		t.Fatalf("fallback fetch = %+v", snapshot)
	}
}

// Within the TTL the cache is served; an explicit refresh refetches; past the
// TTL the cache is refreshed.
func TestCacheTTLAndForcedRefresh(t *testing.T) {
	clock := newFakeClock()
	service, calls := testService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fixture))
	}, clock)

	first := service.Get(context.Background(), false)
	if first.Cached || first.Error != "" || *calls != 1 {
		t.Fatalf("first = %+v calls=%d", first, *calls)
	}
	second := service.Get(context.Background(), false)
	if !second.Cached || second.Error != "" || *calls != 1 {
		t.Fatalf("second = %+v calls=%d", second, *calls)
	}
	if !second.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("cached response changed fetched_at: %v vs %v", second.FetchedAt, first.FetchedAt)
	}

	forced := service.Get(context.Background(), true)
	if forced.Cached || forced.Error != "" || *calls != 2 {
		t.Fatalf("forced = %+v calls=%d", forced, *calls)
	}

	clock.advance(2 * time.Hour)
	expired := service.Get(context.Background(), false)
	if expired.Cached || expired.Error != "" || *calls != 3 {
		t.Fatalf("expired = %+v calls=%d", expired, *calls)
	}
}

// A failed refresh with a previous catalog returns it explicitly marked cached,
// stale and errored, keeping the original items and fetch time.
func TestStaleOnRefreshFailure(t *testing.T) {
	clock := newFakeClock()
	var healthy atomic.Bool
	healthy.Store(true)
	service, calls := testService(t, func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(fixture))
	}, clock)

	first := service.Get(context.Background(), false)
	if len(first.Items) != 7 {
		t.Fatalf("first = %+v", first)
	}
	healthy.Store(false)
	clock.advance(2 * time.Hour)

	stale := service.Get(context.Background(), false)
	if !stale.Cached || !stale.Stale || stale.Error == "" {
		t.Fatalf("stale flags = %+v", stale)
	}
	if len(stale.Items) != len(first.Items) || !stale.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("stale response changed data: %+v", stale)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want 2", *calls)
	}
}

// Concurrent forced refreshes coalesce into a single upstream request instead
// of a burst, and every caller observes the same fetch.
func TestConcurrentForcedRefreshIsCoalesced(t *testing.T) {
	clock := newFakeClock()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	service, calls := testService(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write([]byte(fixture))
	}, clock)

	const callers = 8
	results := make([]Snapshot, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	go func() {
		defer wg.Done()
		results[0] = service.Get(context.Background(), true)
	}()
	<-started // the first refresh is now in flight and blocks upstream

	for i := 1; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = service.Get(context.Background(), true)
		}(i)
	}
	// Each coalescing caller records one arrival. Wait until every caller has
	// arrived while the first fetch is still blocked, so overlap is guaranteed.
	deadline := time.Now().Add(5 * time.Second)
	for clock.callCount() < int64(callers) {
		if time.Now().After(deadline) {
			t.Fatalf("callers did not arrive: %d", clock.callCount())
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
	for i, result := range results {
		if result.Error != "" || len(result.Items) != 7 {
			t.Fatalf("caller %d = %+v", i, result)
		}
		if !result.FetchedAt.Equal(results[0].FetchedAt) {
			t.Fatalf("caller %d saw a different fetch", i)
		}
		if i > 0 && !result.Cached {
			t.Fatalf("caller %d did not coalesce", i)
		}
	}
}

// A redirect from an approved upstream is refused, so the fetch can never be
// bounced to a loopback or private destination; the previous catalog is served
// explicitly stale instead.
func TestFetchRefusesRedirect(t *testing.T) {
	var reached atomic.Int32
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte(fixture))
	}))
	t.Cleanup(private.Close)

	var redirect atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect.Load() {
			http.Redirect(w, r, private.URL, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(fixture))
	}))
	t.Cleanup(upstream.Close)

	clock := newFakeClock()
	service := NewService(upstream.Client())
	service.sources = []source{{Name: "test", URL: upstream.URL}}
	service.ttl = time.Hour
	service.now = clock.Now

	if first := service.Get(context.Background(), false); first.Error != "" || len(first.Items) != 7 {
		t.Fatalf("first = %+v", first)
	}
	redirect.Store(true)
	clock.advance(2 * time.Hour)
	stale := service.Get(context.Background(), false)
	if !stale.Cached || !stale.Stale || stale.Error == "" || len(stale.Items) != 7 {
		t.Fatalf("redirect was not refused as stale: %+v", stale)
	}
	if reached.Load() != 0 {
		t.Fatalf("redirect target was contacted %d times", reached.Load())
	}

	// Without a previous catalog the refused redirect is a hard failure, never
	// a silently followed request.
	fresh := NewService(upstream.Client())
	fresh.sources = []source{{Name: "test", URL: upstream.URL}}
	if snapshot := fresh.Get(context.Background(), false); snapshot.Error == "" || len(snapshot.Items) != 0 {
		t.Fatalf("fresh = %+v", snapshot)
	}
	if reached.Load() != 0 {
		t.Fatalf("redirect target was contacted %d times", reached.Load())
	}
}

// A nil service and a service without sources fail closed instead of panicking.
func TestServiceFailsClosed(t *testing.T) {
	var nilService *Service
	if snapshot := nilService.Get(context.Background(), false); snapshot.Error == "" || len(snapshot.Items) != 0 {
		t.Fatalf("nil service = %+v", snapshot)
	}
	service := NewService(nil)
	service.sources = nil
	if snapshot := service.Get(context.Background(), false); snapshot.Error == "" {
		t.Fatalf("source-less service = %+v", snapshot)
	}
}

// A slow upstream is abandoned within the configured timeout instead of
// blocking the management request indefinitely.
func TestFetchTimeoutIsBounded(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // runs before the test server closes
	service, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, nil)
	service.timeout = 20 * time.Millisecond

	started := time.Now()
	snapshot := service.Get(context.Background(), false)
	if snapshot.Error == "" {
		t.Fatalf("timeout was not reported: %+v", snapshot)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("fetch was not bounded: %v", elapsed)
	}
}

// An explicit zero rate is emitted while an unknown rate stays absent, so the
// wire format never invents a zero price.
func TestRatesWireFormatKeepsZeroAndMissingDistinct(t *testing.T) {
	zero := int64(0)
	encoded, err := json.Marshal(Model{
		ID: "m", Provider: "p", Name: "m",
		Rates: Rates{Input: &zero},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"input":0`) {
		t.Fatalf("explicit zero dropped: %s", encoded)
	}
	if strings.Contains(string(encoded), `"output"`) || strings.Contains(string(encoded), `"cache_read"`) {
		t.Fatalf("missing rate invented: %s", encoded)
	}
}

// The upstream is never chosen by a request: the exactMicro conversion accepts
// only non-negative decimals and rejects everything else.
func TestExactMicroRejectsNonDecimals(t *testing.T) {
	for _, raw := range []string{"", " ", "abc", "-1", "NaN", "Inf", "+", "1.2.3"} {
		if _, err := exactMicro(raw); err == nil {
			t.Fatalf("exactMicro(%q) accepted", raw)
		}
	}
	if got, err := exactMicro("0.0000005"); err != nil || got != 1 { // rounds half up
		t.Fatalf("exactMicro = %d %v", got, err)
	}
	if got, err := exactMicro("9223372036854"); err != nil || got != 9_223_372_036_854_000_000 {
		t.Fatalf("exactMicro large = %d %v", got, err)
	}
	if _, err := exactMicro("9223372036855"); err == nil { // 9.223372036855e18 microUSD overflows int64
		t.Fatal("exactMicro accepted an overflowing value")
	}
}
