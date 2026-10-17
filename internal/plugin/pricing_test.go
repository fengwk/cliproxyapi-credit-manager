package plugin

import (
	"context"
	"fmt"
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

// TestInterceptorLeavesImageBillingToQueryTime pins that the interceptor no
// longer derives a billable image count from the request body. Admission charges
// nothing, the forwarded body stays byte-identical, and a per-image price yields
// a null execution cost because the host does not report a real image count.
func TestInterceptorLeavesImageBillingToQueryTime(t *testing.T) {
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
	before := h.report(credit.Filter{KeyID: h.key.ID}).Summary
	if before.Executions != 0 || before.CostMicroUSD != 0 {
		t.Fatalf("admission created a priced execution: %+v", before)
	}
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-img", TraceID: "trace-img", Provider: "openai", Model: "img-model",
		APIKey: h.key.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail: pluginapi.UsageDetail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	h.complete("req-img", "trace-img", pluginapi.RequestCompletionSucceeded)
	report := h.report(credit.Filter{KeyID: h.key.ID})
	if report.Summary.CostMicroUSD != 0 || report.Summary.UnpricedExecutions != 1 {
		t.Fatalf("per-image summary = %+v, want one unpriced execution", report.Summary)
	}
	if len(report.Items) != 1 || len(report.Items[0].Executions) != 1 ||
		report.Items[0].Executions[0].CostMicroUSD != nil {
		t.Fatalf("per-image execution must expose null cost: %+v", report.Items)
	}
}

// TestInterceptorAppliesRequestedServiceTier pins that a service_tier in the
// body is frozen at admission and bills the tier rate, with no context-tier
// estimate guess and no effect on admission itself.
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
	settle := func(id, tier string) {
		t.Helper()
		if resp := h.interceptBody(id, "trace-"+id, `{"model":"test-model","tier":"`+tier+`"}`); resp.Terminate {
			t.Fatalf("%s intercept: %+v", tier, resp)
		}
		h.usage(ownUsage("exec-"+id, "trace-"+id, h.key.Principal, pluginapi.UsageDetail{InputTokens: 1, TotalTokens: 1}))
		h.complete(id, "trace-"+id, pluginapi.RequestCompletionSucceeded)
	}
	// "standard" and "priority" have the same length, so only the tier differs.
	settle("req-a", "standard")
	afterBase := h.report(credit.Filter{KeyID: h.key.ID}).Summary.CostMicroUSD
	settle("req-b", "priority")
	afterPriority := h.report(credit.Filter{KeyID: h.key.ID}).Summary.CostMicroUSD
	baseCharge := afterBase
	priorityCharge := afterPriority - baseCharge
	if baseCharge <= 0 || priorityCharge <= baseCharge {
		t.Fatalf("priority charge = %d, base charge = %d; the service tier must raise the billed rate", priorityCharge, baseCharge)
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
	if cost := h.report(credit.Filter{KeyID: h.key.ID}).Summary.CostMicroUSD; cost != 91 {
		t.Fatalf("priority cost = %d, want 91", cost)
	}
	// A late identical replay must not add another execution.
	h.usage(first)
	if cost := h.report(credit.Filter{KeyID: h.key.ID}).Summary.CostMicroUSD; cost != 91 {
		t.Fatalf("replay changed cost: %d", cost)
	}
	// No upstream tier: the flex tier frozen at admission is used.
	if resp := h.interceptBody("req-2", "trace-2", `{"model":"test-model","tier":"flex"}`); resp.Terminate {
		t.Fatalf("intercept: %+v", resp)
	}
	h.usage(ownUsage("exec-2", "trace-2", h.key.Principal, pluginapi.UsageDetail{InputTokens: 1, OutputTokens: 10, TotalTokens: 11}))
	h.complete("req-2", "trace-2", pluginapi.RequestCompletionSucceeded)
	if cost := h.report(credit.Filter{KeyID: h.key.ID}).Summary.CostMicroUSD; cost != 132 {
		t.Fatalf("frozen flex cost = %d, want 132", cost)
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
// bill base rates even though the request's frozen tier is priority.
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
	// The request declares priority, so priority is frozen on the snapshot.
	if resp := h.interceptBody("req-1", "trace-1", `{"model":"test-model","tier":"priority"}`); resp.Terminate {
		t.Fatalf("intercept: %+v", resp)
	}
	// The upstream reports the default tier: base input 1 + base output 20 = 21,
	// not the frozen priority output rate (91).
	usage := ownUsage("exec-1", "trace-1", h.key.Principal, pluginapi.UsageDetail{InputTokens: 1, OutputTokens: 10, TotalTokens: 11})
	usage.ServiceTier = "default"
	h.usage(usage)
	h.complete("req-1", "trace-1", pluginapi.RequestCompletionSucceeded)
	if cost := h.report(credit.Filter{KeyID: h.key.ID}).Summary.CostMicroUSD; cost != 21 {
		t.Fatalf("cost = %d, want 21 at the base rate, never the frozen priority rate", cost)
	}
}

// TestInterceptorNeverGatesOnImageHints pins that no request-body image hint
// (including values that used to be rejected) can refuse admission: image
// accounting is not a request fact and per-image executions stay unpriced.
func TestInterceptorNeverGatesOnImageHints(t *testing.T) {
	h := newHarness(t)
	store := h.store()
	ctx := context.Background()
	if err := store.PutPrice(ctx, credit.Price{Model: "img-model", BillingMode: "per_image", PerImageMicroUSD: 50_000}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	for i, body := range []string{
		`{"model":"img-model","n":100}`,
		`{"model":"img-model","n":33}`,
		`{"model":"img-model","n":0}`,
		`{"model":"img-model","n":-1}`,
		`{"model":"img-model","n":2.5}`,
		`{"model":"img-model","n":"3"}`,
		`{"model":"img-model","n":1e30}`,
		`{"model":"img-model","n_images":64}`,
		`{"model":"img-model"}`,
	} {
		raw := []byte(body)
		original := string(raw)
		resp := h.intercept(pluginapi.RequestInterceptRequest{
			RequestID: fmt.Sprintf("req-%d", i), TraceID: fmt.Sprintf("trace-%d", i),
			Model: "img-model", RequestedModel: "img-model",
			Body:     raw,
			Metadata: map[string]any{"caller_scope": h.key.Scope, "request_path": "/v1/images"},
		})
		if resp.Terminate {
			t.Fatalf("body %s: resp = %+v, image hints must not gate admission", body, resp)
		}
		if string(raw) != original {
			t.Fatalf("body %s mutated: %s", body, raw)
		}
	}
	// The same hint on a token request is irrelevant too.
	if resp := h.interceptBody("req-token", "trace-token", `{"model":"test-model","n":100}`); resp.Terminate {
		t.Fatalf("token request with an image hint was refused: %+v", resp)
	}
}
