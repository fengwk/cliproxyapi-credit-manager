package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

func pricingRuleJSON(t *testing.T, rule credit.PricingRule) string {
	t.Helper()
	raw, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("marshal rule: %v", err)
	}
	return string(raw)
}

// TestPricingRuleRoutesRoundTrip exercises the advanced pricing management API
// end to end through the shared dispatcher: upsert, list, preview, delete and
// the safety guards.
func TestPricingRuleRoutesRoundTrip(t *testing.T) {
	h := testHandler(t)
	rule := credit.PricingRule{
		ID: "r1", MatchKind: credit.MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: credit.Price{Model: "m1", Rates: credit.Rates{Input: 1_000_000, Output: 2_000_000}},
		Tiers: []credit.PriceTier{{Kind: "context", Threshold: 1_000, Label: "1K", Rates: credit.Rates{Input: 4_000_000}}},
	}
	put := h.Handle(managementRequest("PUT", "/api/pricing-rules", pricingRuleJSON(t, rule)))
	if put.StatusCode != http.StatusOK {
		t.Fatalf("put=%d %s", put.StatusCode, put.Body)
	}
	var putOut struct {
		Item credit.PricingRule `json:"item"`
	}
	if err := json.Unmarshal(put.Body, &putOut); err != nil {
		t.Fatalf("decode put: %v %s", err, put.Body)
	}
	if putOut.Item.ID != "r1" || putOut.Item.MatchKind != credit.MatchExact || !putOut.Item.Enabled {
		t.Fatalf("stored item = %+v", putOut.Item)
	}
	if len(putOut.Item.Tiers) != 1 || putOut.Item.Tiers[0].Threshold != 1_000 {
		t.Fatalf("stored tiers = %+v", putOut.Item.Tiers)
	}
	list := h.Handle(managementRequest("GET", "/api/pricing-rules", ""))
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list=%d %s", list.StatusCode, list.Body)
	}
	var listOut struct {
		Items []credit.PricingRule `json:"items"`
	}
	if err := json.Unmarshal(list.Body, &listOut); err != nil || len(listOut.Items) != 1 {
		t.Fatalf("list body=%s err=%v", list.Body, err)
	}
	// Preview reports the context tier reached by the prompt size.
	preview := managementRequest("GET", "/api/price-preview", "")
	preview.Query.Set("model", "m1")
	preview.Query.Set("input_tokens", "2000")
	got := h.Handle(preview)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("preview=%d %s", got.StatusCode, got.Body)
	}
	var match credit.PriceMatch
	if err := json.Unmarshal(got.Body, &match); err != nil {
		t.Fatalf("decode preview: %v %s", err, got.Body)
	}
	if match.Rates.Input != 4_000_000 || match.CostMicroUSD != 8_000 {
		t.Fatalf("preview match = %+v", match)
	}
	// A service tier beats the context tier in the preview too.
	preview.Query.Set("service_tier", "priority")
	_ = h.Handle(managementRequest("PUT", "/api/pricing-rules", pricingRuleJSON(t, credit.PricingRule{
		ID: "r1", MatchKind: credit.MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: credit.Price{Model: "m1", Rates: credit.Rates{Input: 1_000_000}},
		Tiers: []credit.PriceTier{
			{Kind: "context", Threshold: 1_000, Rates: credit.Rates{Input: 4_000_000}},
			{Kind: "service", Service: "priority", Rates: credit.Rates{Input: 9_000_000}},
		},
	})))
	got = h.Handle(preview)
	var serviceMatch credit.PriceMatch
	if err := json.Unmarshal(got.Body, &serviceMatch); err != nil {
		t.Fatalf("decode service preview: %v %s", err, got.Body)
	}
	if serviceMatch.Rates.Input != 9_000_000 || serviceMatch.CostMicroUSD != 18_000 {
		t.Fatalf("service preview = %+v", serviceMatch)
	}
	// Delete is exact and its absence is reported.
	del := h.Handle(managementRequest("POST", "/api/delete-price-rule", `{"id":"r1"}`))
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete=%d %s", del.StatusCode, del.Body)
	}
	again := h.Handle(managementRequest("POST", "/api/delete-price-rule", `{"id":"r1"}`))
	if again.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete=%d %s", again.StatusCode, again.Body)
	}
}

// TestPricingRoutesFailClosed covers the guards and the error mapping of the
// advanced pricing routes.
func TestPricingRoutesFailClosed(t *testing.T) {
	h := testHandler(t)
	// Only an exact model is priced; an unpriced model preview is a client error.
	preview := managementRequest("GET", "/api/price-preview", "")
	preview.Query.Set("model", "unknown")
	if got := h.Handle(preview); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("unpriced preview=%d %s", got.StatusCode, got.Body)
	}
	// An invalid rule is rejected without being stored.
	invalid := h.Handle(managementRequest("PUT", "/api/pricing-rules", `{"id":"x","match_kind":"glob","pattern":"[","enabled":true,"price":{"model":"x","rates":{"input":1,"output":0,"cache_read":0,"cache_write":0}}}`))
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid rule=%d %s", invalid.StatusCode, invalid.Body)
	}
	// An unknown field is rejected (the shared decoder is strict).
	strict := h.Handle(managementRequest("PUT", "/api/pricing-rules", `{"id":"x","match_kind":"exact","pattern":"x","enabled":true,"tiers":[],"price":{"model":"x","rates":{"input":1,"output":0,"cache_read":0,"cache_write":0}},"extra":1}`))
	if strict.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field accepted=%d %s", strict.StatusCode, strict.Body)
	}
	// A negative preview size is a client error.
	neg := managementRequest("GET", "/api/price-preview", "")
	neg.Query.Set("model", "m1")
	neg.Query.Set("input_tokens", "-1")
	if got := h.Handle(neg); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative tokens=%d %s", got.StatusCode, got.Body)
	}
	// The guards of the shared handler still apply on the dedicated entry point.
	noAuth := managementRequest("GET", "/api/pricing-rules", "")
	noAuth.Headers.Del("Authorization")
	if got := h.HandlePricing(t.Context(), noAuth); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing auth=%d %s", got.StatusCode, got.Body)
	}
	crossSite := managementRequest("PUT", "/api/pricing-rules", `{}`)
	crossSite.Headers.Set("Sec-Fetch-Site", "cross-site")
	if got := h.HandlePricing(t.Context(), crossSite); got.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site=%d %s", got.StatusCode, got.Body)
	}
	wrongMedia := managementRequest("PUT", "/api/pricing-rules", `{}`)
	wrongMedia.Headers.Set("Content-Type", "text/plain")
	if got := h.HandlePricing(t.Context(), wrongMedia); got.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong media=%d %s", got.StatusCode, got.Body)
	}
	// An unknown pricing route under the dedicated entry point is not found.
	unknown := managementRequest("GET", "/api/price-nope", "")
	if got := h.HandlePricing(t.Context(), unknown); got.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route=%d %s", got.StatusCode, got.Body)
	}
}

// TestPricingRoutesRequireCapableBackend pins the optional-capability contract:
// a backend that cannot manage pricing refuses instead of pretending.
func TestPricingRoutesRequireCapableBackend(t *testing.T) {
	h := testHandler(t)
	h.db = backendWithoutPricing{Backend: h.db}
	if got := h.Handle(managementRequest("GET", "/api/pricing-rules", "")); got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("incapable backend=%d %s", got.StatusCode, got.Body)
	}
}

// backendWithoutPricing satisfies credit.Backend but not the optional advanced
// pricing capability.
type backendWithoutPricing struct{ credit.Backend }
