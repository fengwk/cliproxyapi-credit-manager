package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

// Exercise the public dispatcher, not just the individual feature helpers:
// every new management route must inherit authorization and CSRF protection.
func TestAlignedManagementRoutesUseSharedGate(t *testing.T) {
	h := testHandler(t)
	h.catalog, h.fx = nil, nil // Routing tests must not contact public services.
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/overview"}, {"GET", "/api/callers"}, {"POST", "/api/callers"},
		{"PUT", "/api/callers"}, {"GET", "/api/audit"}, {"POST", "/api/reveal"},
		{"POST", "/api/delete-key"}, {"POST", "/api/reset-spend"},
		{"GET", "/api/model-catalog"}, {"GET", "/api/exchange-rate"},
		{"GET", "/api/pricing-rules"}, {"PUT", "/api/pricing-rules"},
		{"POST", "/api/delete-price-rule"}, {"GET", "/api/price-preview"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			req := managementRequest(route.method, route.path, `{}`)
			if got := h.Handle(req); got.StatusCode == http.StatusNotFound {
				t.Fatalf("declared feature is unreachable: %d %s", got.StatusCode, got.Body)
			}
			req.Headers.Del("Authorization")
			if got := h.Handle(req); got.StatusCode != http.StatusUnauthorized {
				t.Fatalf("missing authorization accepted: %d %s", got.StatusCode, got.Body)
			}
			if route.method == "GET" {
				return
			}
			req.Headers.Set("Authorization", "Bearer synthetic-manager")
			req.Headers.Set("Sec-Fetch-Site", "cross-site")
			if got := h.Handle(req); got.StatusCode != http.StatusForbidden {
				t.Fatalf("cross-site mutation accepted: %d %s", got.StatusCode, got.Body)
			}
		})
	}
}

// More than one page of own data must feed the model charts, and foreign
// selectors must not expand the customer's report or reveal caller metadata.
func TestLookupModelsAreFullSelectionAndOwnKeyOnly(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	if err := h.db.PutPrice(ctx, credit.Price{Model: "m1", Rates: credit.Rates{Input: 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	caller := h.Handle(managementRequest("POST", "/api/callers",
		`{"id":"operator-only-caller","display_name":"private team","enabled":true}`))
	if caller.StatusCode != http.StatusCreated {
		t.Fatalf("caller=%d %s", caller.StatusCode, caller.Body)
	}
	alpha := createTestKey(t, h.db, "synthetic-model-owner", credit.KeySpec{CallerID: "operator-only-caller"})
	beta := createTestKey(t, h.db, "synthetic-model-foreign", credit.KeySpec{})
	for i := 0; i < 51; i++ {
		seedRequest(t, h.db, alpha, fmt.Sprintf("alpha-%d", i), fmt.Sprintf("trace-alpha-%d", i))
	}
	seedRequest(t, h.db, beta, "foreign-request", "foreign-trace")
	query := url.Values{
		"key_id": {beta.ID}, "auth_id": {"foreign-auth"}, "caller_id": {"foreign-caller"},
		"page_size": {"1"},
	}
	got := lookupCall(t, h, PublicLookupData, "synthetic-model-owner", query)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("lookup=%d %s", got.StatusCode, got.Body)
	}
	var out lookupData
	if err := json.Unmarshal(got.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Total != 51 || len(out.Models) != 1 ||
		out.Models[0].Requests != 51 || out.Models[0].CostMicroUSD != 510 ||
		out.Summary.CostMicroUSD != out.Models[0].CostMicroUSD {
		t.Fatalf("page/model totals disagree: %+v", out)
	}
	for _, secret := range []string{"operator-only-caller", "private team", "foreign-request", beta.ID, "auth-secret"} {
		if strings.Contains(string(got.Body), secret) {
			t.Errorf("customer lookup leaked %q", secret)
		}
	}
	reset := h.Handle(managementRequest("POST", "/api/reset-spend",
		`{"id":"`+alpha.ID+`","total":true,"daily":true,"weekly":true,"monthly":true,"reason":"new allocation"}`))
	if reset.StatusCode != http.StatusOK {
		t.Fatalf("reset=%d %s", reset.StatusCode, reset.Body)
	}
	got = lookupCall(t, h, PublicLookupData, "synthetic-model-owner", nil)
	if err := json.Unmarshal(got.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Key.SpentMicroUSD != 510 || out.Key.QuotaUsedMicroUSD != 0 ||
		out.Key.DailyUsedMicroUSD != 0 || out.Summary.CostMicroUSD != 510 {
		t.Fatalf("reset erased history or customer quota view is stale: %+v", out)
	}
}
