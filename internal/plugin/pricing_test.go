package plugin

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

// pricingStore is the optional advanced-pricing capability of the live store.
type pricingStore interface {
	PutPricingRule(ctx context.Context, rule credit.PricingRule) error
}

// interceptBody admits one request with a caller-supplied body.
func (h *harness) interceptBody(id, trace, body string) pluginapi.RequestInterceptResponse {
	h.t.Helper()
	req := h.request(id, trace)
	req.Body = []byte(body)
	return h.intercept(req)
}

// ownUsage builds one execution record attributed to this plugin's own frontend
// auth, which is what makes it billable customer usage.
func ownUsage(id, trace, principal string, detail pluginapi.UsageDetail) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		RequestID: id, TraceID: trace, Provider: "openai", Model: testModel,
		APIKey: principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail: detail,
	}
}

// TestInterceptorReadsPricingMetadataWithoutMutating pins that the interceptor
// reads the requested image count from a private body copy, leaves the forwarded
// request byte-identical, and still answers with the unchanged zero-value
// envelope. The frozen count is what the ledger bills.
func TestInterceptorReadsPricingMetadataWithoutMutating(t *testing.T) {
	h := newHarness(t)
	store := h.store()
	ctx := context.Background()
	if err := store.PutPrice(ctx, credit.Price{Model: "img-model", BillingMode: "per_image", PerImageMicroUSD: 50_000}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	body := []byte(`{"model":"img-model","n":3}`)
	original := string(body)
	req := pluginapi.RequestInterceptRequest{
		RequestID: "req-img", TraceID: "trace-img", Model: "img-model", RequestedModel: "img-model",
		Body:     body,
		Metadata: map[string]any{"caller_scope": h.key.Scope, "request_path": "/v1/images"},
	}
	resp := h.intercept(req)
	if resp.Terminate || resp.StatusCode != 0 || len(resp.ResponseBody) != 0 || len(resp.ResponseHeaders) != 0 {
		t.Fatalf("interceptor answer changed: %+v", resp)
	}
	if string(req.Body) != original {
		t.Fatalf("interceptor mutated the forwarded body: %s", req.Body)
	}
	key, err := store.GetKey(ctx, h.key.ID)
	if err != nil || key.HeldMicroUSD != 150_000 {
		t.Fatalf("held=%d err=%v, want the frozen three-image hold 150000", key.HeldMicroUSD, err)
	}
	// The proven successful lifecycle bills the frozen count exactly once.
	h.usage(ownUsage("exec-img", "trace-img", h.key.Principal, pluginapi.UsageDetail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}))
	h.complete("req-img", "trace-img", pluginapi.RequestCompletionSucceeded)
	key, err = store.GetKey(ctx, h.key.ID)
	if err != nil || key.SpentMicroUSD != 150_000 || key.HeldMicroUSD != 0 {
		t.Fatalf("spent=%d held=%d err=%v, want 150000/0", key.SpentMicroUSD, key.HeldMicroUSD, err)
	}
}

// TestInterceptorAppliesRequestedServiceTier pins that a service_tier in the
// body raises the reservation without any context-tier estimate guess.
func TestInterceptorAppliesRequestedServiceTier(t *testing.T) {
	h := newHarness(t)
	store := h.store()
	ctx := context.Background()
	if err := store.(pricingStore).PutPricingRule(ctx, credit.PricingRule{
		ID: "svc", MatchKind: credit.MatchExact, Pattern: testModel, Priority: 5, Enabled: true,
		Price: credit.Price{Model: testModel, Rates: credit.Rates{Input: 1_000_000}},
		Tiers: []credit.PriceTier{{Kind: "service", Service: "priority", Rates: credit.Rates{Input: 4_000_000}}},
	}); err != nil {
		t.Fatalf("PutPricingRule: %v", err)
	}
	// "standard" and "priority" have the same length, so only the tier differs.
	if resp := h.interceptBody("req-a", "trace-a", `{"model":"test-model","tier":"standard"}`); resp.Terminate {
		t.Fatalf("base intercept: %+v", resp)
	}
	afterBase, err := store.GetKey(ctx, h.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resp := h.interceptBody("req-b", "trace-b", `{"model":"test-model","tier":"priority"}`); resp.Terminate {
		t.Fatalf("priority intercept: %+v", resp)
	}
	afterPriority, err := store.GetKey(ctx, h.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseHold := afterBase.HeldMicroUSD
	priorityHold := afterPriority.HeldMicroUSD - baseHold
	if baseHold <= 0 || priorityHold <= baseHold {
		t.Fatalf("priority hold = %d, base hold = %d; the service tier must raise the hold", priorityHold, baseHold)
	}
}

// TestUsageServiceTierSelectionOrderAndPersistence pins the host contract: the
// billed tier is the upstream response tier, else the requested tier, else the
// tier frozen at admission, and it is persisted so a replay cannot silently
// reprice the execution.
func TestUsageServiceTierSelectionOrderAndPersistence(t *testing.T) {
	h := newHarness(t)
	store := h.store()
	ctx := context.Background()
	if err := store.(pricingStore).PutPricingRule(ctx, credit.PricingRule{
		ID: "svc", MatchKind: credit.MatchExact, Pattern: testModel, Priority: 5, Enabled: true,
		Price: credit.Price{Model: testModel, Rates: credit.Rates{Input: 1_000_000, Output: 2_000_000}},
		Tiers: []credit.PriceTier{
			{Kind: "service", Service: "priority", Rates: credit.Rates{Output: 9_000_000}},
			{Kind: "service", Service: "flex", Rates: credit.Rates{Output: 4_000_000}},
		},
	}); err != nil {
		t.Fatalf("PutPricingRule: %v", err)
	}
	// The request asks flex; the upstream reports priority, which wins.
	if resp := h.interceptBody("req-1", "trace-1", `{"model":"test-model","tier":"flex"}`); resp.Terminate {
		t.Fatalf("intercept: %+v", resp)
	}
	first := ownUsage("exec-1", "trace-1", h.key.Principal, pluginapi.UsageDetail{InputTokens: 1, OutputTokens: 10, TotalTokens: 11})
	first.ResponseServiceTier = "priority"
	h.usage(first)
	h.complete("req-1", "trace-1", pluginapi.RequestCompletionSucceeded)
	if key, err := store.GetKey(ctx, h.key.ID); err != nil || key.SpentMicroUSD != 91 {
		t.Fatalf("priority spend = %d err=%v, want 91", key.SpentMicroUSD, err)
	}
	// A late identical replay must not charge again.
	h.usage(first)
	if key, err := store.GetKey(ctx, h.key.ID); err != nil || key.SpentMicroUSD != 91 {
		t.Fatalf("replay changed spend: %d err=%v", key.SpentMicroUSD, err)
	}
	// No upstream tier: the flex tier frozen at admission is used.
	if resp := h.interceptBody("req-2", "trace-2", `{"model":"test-model","tier":"flex"}`); resp.Terminate {
		t.Fatalf("intercept: %+v", resp)
	}
	h.usage(ownUsage("exec-2", "trace-2", h.key.Principal, pluginapi.UsageDetail{InputTokens: 1, OutputTokens: 10, TotalTokens: 11}))
	h.complete("req-2", "trace-2", pluginapi.RequestCompletionSucceeded)
	if key, err := store.GetKey(ctx, h.key.ID); err != nil || key.SpentMicroUSD != 132 {
		t.Fatalf("frozen flex spend = %d err=%v, want 132", key.SpentMicroUSD, err)
	}
	report, err := store.Report(ctx, credit.Filter{KeyID: h.key.ID})
	if err != nil {
		t.Fatal(err)
	}
	tiers := map[string]bool{}
	for _, item := range report.Items {
		for _, exec := range item.Executions {
			tiers[exec.ServiceTier] = true
		}
	}
	if !tiers["priority"] || !tiers["flex"] {
		t.Fatalf("persisted tiers = %v, want both priority and flex", tiers)
	}
}

// TestUsageReportedDefaultTierDoesNotInheritFrozen pins the host wiring of the
// reported-tier presence rule: an upstream that reports the default tier must
// bill base rates even though the request reserved a priority tier.
func TestUsageReportedDefaultTierDoesNotInheritFrozen(t *testing.T) {
	h := newHarness(t)
	store := h.store()
	ctx := context.Background()
	if err := store.(pricingStore).PutPricingRule(ctx, credit.PricingRule{
		ID: "svc", MatchKind: credit.MatchExact, Pattern: testModel, Priority: 5, Enabled: true,
		Price: credit.Price{Model: testModel, Rates: credit.Rates{Input: 1_000_000, Output: 2_000_000}},
		Tiers: []credit.PriceTier{{Kind: "service", Service: "priority", Rates: credit.Rates{Output: 9_000_000}}},
	}); err != nil {
		t.Fatalf("PutPricingRule: %v", err)
	}
	// The request reserves priority, so priority is frozen on the snapshot.
	if resp := h.interceptBody("req-1", "trace-1", `{"model":"test-model","tier":"priority"}`); resp.Terminate {
		t.Fatalf("intercept: %+v", resp)
	}
	// The upstream reports the default tier: base input 1 + base output 20 = 21,
	// not the frozen priority output rate (91).
	usage := ownUsage("exec-1", "trace-1", h.key.Principal, pluginapi.UsageDetail{InputTokens: 1, OutputTokens: 10, TotalTokens: 11})
	usage.ServiceTier = "default"
	h.usage(usage)
	h.complete("req-1", "trace-1", pluginapi.RequestCompletionSucceeded)
	if key, err := store.GetKey(ctx, h.key.ID); err != nil || key.SpentMicroUSD != 21 {
		t.Fatalf("spend = %d err = %v, want 21 at the base rate, never the frozen priority rate", key.SpentMicroUSD, err)
	}
}

// TestInterceptorRefusesUnsupportedImageCount pins the admission contract of the
// requested image count: an explicit unsupported value fails closed for a
// per-image price instead of being silently billed for a clamped number, an
// omitted value means one image, and the same hint on a token request is
// irrelevant rather than a rejection. The forwarded body is never modified.
func TestInterceptorRefusesUnsupportedImageCount(t *testing.T) {
	h := newHarness(t)
	store := h.store()
	ctx := context.Background()
	if err := store.PutPrice(ctx, credit.Price{Model: "img-model", BillingMode: "per_image", PerImageMicroUSD: 50_000}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	// A per-image intercept must request the image model explicitly: the harness
	// body helper keeps its own default model.
	imageIntercept := func(id, body string) pluginapi.RequestInterceptResponse {
		return h.intercept(pluginapi.RequestInterceptRequest{
			RequestID: id, TraceID: "trace-" + id, Model: "img-model", RequestedModel: "img-model",
			Body:     []byte(body),
			Metadata: map[string]any{"caller_scope": h.key.Scope, "request_path": "/v1/images"},
		})
	}
	for _, body := range []string{
		`{"model":"img-model","n":100}`,
		`{"model":"img-model","n":33}`,
		`{"model":"img-model","n":0}`,
		`{"model":"img-model","n":-1}`,
		`{"model":"img-model","n":2.5}`,
		`{"model":"img-model","n":"3"}`,
		`{"model":"img-model","n":1e30}`,
		`{"model":"img-model","n_images":64}`,
	} {
		raw := []byte(body)
		original := string(raw)
		resp := h.intercept(pluginapi.RequestInterceptRequest{
			RequestID: "req-bad", TraceID: "trace-bad", Model: "img-model", RequestedModel: "img-model",
			Body:     raw,
			Metadata: map[string]any{"caller_scope": h.key.Scope, "request_path": "/v1/images"},
		})
		if !resp.Terminate || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: resp = %+v, want a fail-closed 400", body, resp)
		}
		if string(raw) != original {
			t.Fatalf("body %s mutated: %s", body, raw)
		}
		if key, err := store.GetKey(ctx, h.key.ID); err != nil || key.HeldMicroUSD != 0 {
			t.Fatalf("body %s: held = %d err = %v, want no reservation", body, key.HeldMicroUSD, err)
		}
	}
	// An omitted count is one image.
	if resp := imageIntercept("req-default", `{"model":"img-model"}`); resp.Terminate {
		t.Fatalf("omitted count: %+v", resp)
	}
	if key, err := store.GetKey(ctx, h.key.ID); err != nil || key.HeldMicroUSD != 50_000 {
		t.Fatalf("omitted count held = %d err = %v, want 50000 for one image", key.HeldMicroUSD, err)
	}
	// A supported count is billed exactly.
	if resp := imageIntercept("req-two", `{"model":"img-model","n":2}`); resp.Terminate {
		t.Fatalf("supported count: %+v", resp)
	}
	if key, err := store.GetKey(ctx, h.key.ID); err != nil || key.HeldMicroUSD != 150_000 {
		t.Fatalf("two-image held = %d err = %v, want 150000", key.HeldMicroUSD, err)
	}
	// A token price ignores the hint: an image hint is not a billing input there,
	// so it must never turn into a rejection.
	if resp := h.interceptBody("req-token", "trace-token", `{"model":"test-model","n":100}`); resp.Terminate {
		t.Fatalf("token request with an irrelevant image hint was refused: %+v", resp)
	}
}
