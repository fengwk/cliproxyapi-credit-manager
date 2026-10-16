package web

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/catalog"
)

// fakeCatalogSource records the refresh flag it was asked for and returns a
// fixed snapshot, so the endpoint can be exercised without any network access.
type fakeCatalogSource struct {
	refresh  []bool
	snapshot catalog.Snapshot
}

func (f *fakeCatalogSource) Get(_ context.Context, refresh bool) catalog.Snapshot {
	f.refresh = append(f.refresh, refresh)
	return f.snapshot
}

type fakeExchangeSource struct {
	refresh []bool
	rate    catalog.Rate
}

func (f *fakeExchangeSource) Get(_ context.Context, refresh bool) catalog.Rate {
	f.refresh = append(f.refresh, refresh)
	return f.rate
}

func sampleSnapshot() catalog.Snapshot {
	input := int64(3_000_000)
	return catalog.Snapshot{
		Items: []catalog.Model{{
			ID:       "anthropic/claude-fable-5",
			Provider: "deepinfra",
			Name:     "Claude Fable 5",
			Rates:    catalog.Rates{Input: &input},
		}},
		Source:    "models.dev",
		FetchedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func decodeInto[T any](t *testing.T, respBody []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decode %s: %v", respBody, err)
	}
	return out
}

// The catalog endpoint returns the normalized snapshot and forwards refresh=1
// only, never writing any financial state.
func TestModelCatalogEndpointServesSnapshot(t *testing.T) {
	h := testHandler(t)
	fake := &fakeCatalogSource{snapshot: sampleSnapshot()}
	h.catalog = fake

	got := h.Handle(managementRequest("GET", "/api/model-catalog", ""))
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}
	if got.Headers.Get("Cache-Control") != "no-store" {
		t.Fatal("catalog response is cacheable")
	}
	body := decodeInto[map[string]any](t, got.Body)
	if body["source"] != "models.dev" || body["cached"] != false || body["stale"] != false {
		t.Fatalf("body=%s", got.Body)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items=%v", body["items"])
	}
	item := items[0].(map[string]any)
	if item["id"] != "anthropic/claude-fable-5" || item["provider"] != "deepinfra" {
		t.Fatalf("item=%v", item)
	}
	rates := item["rates"].(map[string]any)
	if rates["input"] != float64(3_000_000) {
		t.Fatalf("rates=%v", rates)
	}

	refresh := managementRequest("GET", "/api/model-catalog", "")
	refresh.Query.Set("refresh", "1")
	if got := h.Handle(refresh); got.StatusCode != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", got.StatusCode, got.Body)
	}
	zero := managementRequest("GET", "/api/model-catalog", "")
	zero.Query.Set("refresh", "0")
	h.Handle(zero)

	want := []bool{false, true, false}
	if len(fake.refresh) != len(want) {
		t.Fatalf("refresh flags=%v", fake.refresh)
	}
	for i := range want {
		if fake.refresh[i] != want[i] {
			t.Fatalf("refresh[%d]=%v want %v", i, fake.refresh[i], want[i])
		}
	}
}

// A request cannot select the upstream: url/source/proxy parameters are ignored
// and the fixed service is used unchanged.
func TestModelCatalogEndpointIgnoresRedirectParameters(t *testing.T) {
	h := testHandler(t)
	fake := &fakeCatalogSource{snapshot: sampleSnapshot()}
	h.catalog = fake

	req := managementRequest("GET", "/api/model-catalog", "")
	req.Query.Set("url", "https://evil.example/catalog.json")
	req.Query.Set("source", "evil")
	req.Query.Set("proxy", "http://127.0.0.1:9")
	got := h.Handle(req)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}
	body := decodeInto[map[string]any](t, got.Body)
	if body["source"] != "models.dev" {
		t.Fatalf("redirect parameter changed the source: %s", got.Body)
	}
	if len(fake.refresh) != 1 || fake.refresh[0] {
		t.Fatalf("refresh flags=%v", fake.refresh)
	}
}

// A total upstream failure is a bad gateway; a stale snapshot is still served
// with its previous items explicitly marked stale.
func TestModelCatalogEndpointErrorAndStale(t *testing.T) {
	h := testHandler(t)
	h.catalog = &fakeCatalogSource{snapshot: catalog.Snapshot{
		Items: []catalog.Model{}, Source: "unavailable", Error: "model catalog unavailable: models.dev: http 503",
	}}
	failed := h.Handle(managementRequest("GET", "/api/model-catalog", ""))
	if failed.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", failed.StatusCode, failed.Body)
	}
	if body := decodeInto[map[string]any](t, failed.Body); body["error"] == "" {
		t.Fatalf("failure lost the error: %s", failed.Body)
	}

	stale := sampleSnapshot()
	stale.Cached, stale.Stale, stale.Error = true, true, "model catalog unavailable: models.dev: http 503"
	h.catalog = &fakeCatalogSource{snapshot: stale}
	served := h.Handle(managementRequest("GET", "/api/model-catalog", ""))
	if served.StatusCode != http.StatusOK {
		t.Fatalf("stale status=%d body=%s", served.StatusCode, served.Body)
	}
	body := decodeInto[map[string]any](t, served.Body)
	if body["stale"] != true || body["cached"] != true || body["error"] == "" {
		t.Fatalf("stale body=%s", served.Body)
	}
	if items := body["items"].([]any); len(items) != 1 {
		t.Fatalf("stale items=%v", body["items"])
	}
}

// The catalog endpoint stays behind the management boundary and reports an
// unavailable service instead of panicking.
func TestModelCatalogEndpointBoundaries(t *testing.T) {
	h := testHandler(t)
	h.catalog = &fakeCatalogSource{snapshot: sampleSnapshot()}

	unauthorized := managementRequest("GET", "/api/model-catalog", "")
	unauthorized.Headers.Del("Authorization")
	if got := h.Handle(unauthorized); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}

	h.catalog = nil
	if got := h.Handle(managementRequest("GET", "/api/model-catalog", "")); got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}
}

// The exchange-rate endpoint presents the decimal quote as-is and forwards
// refresh=1.
func TestExchangeRateEndpointServesDecimal(t *testing.T) {
	h := testHandler(t)
	fake := &fakeExchangeSource{rate: catalog.Rate{
		Base: "USD", Quote: "CNY", Rate: "7.1234", Source: "er-api",
		FetchedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}}
	h.fx = fake

	got := h.Handle(managementRequest("GET", "/api/exchange-rate", ""))
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}
	body := decodeInto[map[string]any](t, got.Body)
	if body["rate"] != "7.1234" || body["base"] != "USD" || body["quote"] != "CNY" || body["source"] != "er-api" {
		t.Fatalf("body=%s", got.Body)
	}

	refresh := managementRequest("GET", "/api/exchange-rate", "")
	refresh.Query.Set("refresh", "1")
	h.Handle(refresh)
	if len(fake.refresh) != 2 || fake.refresh[0] || !fake.refresh[1] {
		t.Fatalf("refresh flags=%v", fake.refresh)
	}
}

// When no quote is available the endpoint reports the failure and carries no
// rate, so the UI keeps its manual fallback. A redirect parameter is ignored.
func TestExchangeRateEndpointFailureAndRedirect(t *testing.T) {
	h := testHandler(t)
	h.fx = &fakeExchangeSource{rate: catalog.Rate{
		Base: "USD", Quote: "CNY", Source: "unavailable", Error: "exchange rate unavailable: er-api: http 503",
	}}
	req := managementRequest("GET", "/api/exchange-rate", "")
	req.Query.Set("url", "https://evil.example/rates")
	req.Query.Set("proxy", "http://127.0.0.1:9")
	got := h.Handle(req)
	if got.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}
	body := decodeInto[map[string]any](t, got.Body)
	if body["error"] == "" {
		t.Fatalf("failure lost the error: %s", got.Body)
	}
	if _, ok := body["rate"]; ok && body["rate"] != "" {
		t.Fatalf("failure invented a rate: %s", got.Body)
	}
	if body["source"] != "unavailable" {
		t.Fatalf("redirect parameter changed the source: %s", got.Body)
	}
}

// The exchange-rate endpoint stays behind the management boundary and reports
// an unavailable service instead of panicking.
func TestExchangeRateEndpointBoundaries(t *testing.T) {
	h := testHandler(t)
	h.fx = &fakeExchangeSource{rate: catalog.Rate{Rate: "7.2"}}

	unauthorized := managementRequest("GET", "/api/exchange-rate", "")
	unauthorized.Headers.Del("Authorization")
	if got := h.Handle(unauthorized); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}

	h.fx = nil
	if got := h.Handle(managementRequest("GET", "/api/exchange-rate", "")); got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}
}
