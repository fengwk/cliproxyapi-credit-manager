package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// backendWithoutAnalytics satisfies credit.Backend but hides the optional
// overview capability, proving the handler degrades instead of assuming it.
type backendWithoutAnalytics struct{ credit.Backend }

func overviewRequest(query url.Values) pluginapi.ManagementRequest {
	req := managementRequest(http.MethodGet, AnalyticsOverview, "")
	req.Query = query
	return req
}

// seedOverview records a small history through the shared ledger contract: two
// m1 requests on openai and one m2 request on anthropic, each with one settled
// execution.
func seedOverview(t *testing.T, db credit.Backend) {
	t.Helper()
	ctx := context.Background()
	for _, price := range []credit.Price{
		{Model: "m1", Rates: credit.Rates{Input: 1_000_000, Output: 2_000_000}},
		{Model: "m2", Rates: credit.Rates{Input: 1_000_000}},
	} {
		if err := db.PutPrice(ctx, price); err != nil {
			t.Fatalf("put price: %v", err)
		}
	}
	key, _, err := db.CreateKey(ctx, credit.KeySpec{Label: "alpha", Enabled: true})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	record := func(id, trace, model, provider, auth string, tokens int64) {
		req, err := db.Reserve(ctx, credit.ReserveInput{
			ID: id, TraceID: trace, KeyID: key.ID, RequestedModel: model, InputEstimate: 1,
		})
		if err != nil {
			t.Fatalf("reserve %s: %v", id, err)
		}
		usage := credit.Usage{
			ID: "u-" + id, RequestID: req.ID, TraceID: req.TraceID, KeyID: key.ID,
			Provider: provider, AuthID: auth, Model: model,
			Tokens:    credit.Tokens{Input: tokens, Output: tokens, Total: 2 * tokens},
			Raw:       pluginapi.UsageDetail{InputTokens: tokens, OutputTokens: tokens, TotalTokens: 2 * tokens},
			Quality:   credit.QualityComplete,
			LatencyMS: 1000, TTFTMS: 100,
		}
		if err := db.RecordUsage(ctx, usage); err != nil {
			t.Fatalf("usage %s: %v", id, err)
		}
		if err := db.Complete(ctx, credit.Completion{ID: req.ID, TraceID: req.TraceID, Outcome: credit.OutcomeSucceeded}); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
	}
	record("r1", "t1", "m1", "openai", "auth-1", 10)
	record("r2", "t2", "m1", "openai", "auth-1", 20)
	record("r3", "t3", "m2", "anthropic", "auth-2", 30)
}

type overviewBody struct {
	Summary credit.Summary `json:"summary"`
	Models  []struct {
		Model          string `json:"model"`
		Requests       int64  `json:"requests"`
		CostMicroUSD   int64  `json:"cost_micro_usd"`
		OutputTokens   int64  `json:"output_tokens"`
		LatencySamples int64  `json:"latency_samples"`
	} `json:"models"`
	RetentionFrom time.Time `json:"retention_from"`
}

func decodeOverview(t *testing.T, body []byte) overviewBody {
	t.Helper()
	var out overviewBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode overview %s: %v", body, err)
	}
	return out
}

// TestHandleAnalyticsServesFullDataset verifies the endpoint returns the whole
// matched set, matches a direct store read, and ignores paging parameters.
func TestHandleAnalyticsServesFullDataset(t *testing.T) {
	h := testHandler(t)
	seedOverview(t, h.db)

	// Paging must not shrink the overview: page 2 of size 1 still reports all
	// three requests and both models.
	query := url.Values{"page": {"2"}, "page_size": {"1"}}
	got := h.HandleAnalytics(context.Background(), overviewRequest(query))
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", got.StatusCode, got.Body)
	}
	if got.Headers.Get("Cache-Control") != "no-store" {
		t.Fatal("overview response is cacheable")
	}
	body := decodeOverview(t, got.Body)
	if body.Summary.Requests != 3 || body.Summary.Executions != 3 {
		t.Fatalf("summary = %+v, want the full dataset", body.Summary)
	}
	if len(body.Models) != 2 || body.Models[0].Model != "m1" || body.Models[1].Model != "m2" {
		t.Fatalf("models = %+v, want m1 then m2", body.Models)
	}
	if body.Models[0].Requests != 2 || body.Models[1].Requests != 1 {
		t.Fatalf("model requests = %+v, want 2 and 1", body.Models)
	}
	if body.Models[0].OutputTokens != 10+20 {
		t.Fatalf("m1 output tokens = %d, want 30", body.Models[0].OutputTokens)
	}
	if body.Models[0].LatencySamples != 2 {
		t.Fatalf("m1 latency samples = %d, want 2", body.Models[0].LatencySamples)
	}
	if body.RetentionFrom.IsZero() {
		t.Fatal("retention_from is missing")
	}

	// The parsed query resolves to the same selection the store reads directly.
	api, ok := h.db.(analyticsOverview)
	if !ok {
		t.Fatal("test handler backend lacks the overview capability")
	}
	direct, err := api.Analytics(context.Background(), credit.Filter{})
	if err != nil {
		t.Fatalf("direct analytics: %v", err)
	}
	if direct.Summary.Requests != body.Summary.Requests || direct.Summary.CostMicroUSD != body.Summary.CostMicroUSD {
		t.Fatalf("direct = %+v, handler = %+v", direct.Summary, body.Summary)
	}
}

// TestHandleAnalyticsParsesUsageFilters proves the overview accepts exactly the
// usage route's selectors.
func TestHandleAnalyticsParsesUsageFilters(t *testing.T) {
	h := testHandler(t)
	seedOverview(t, h.db)
	cases := []struct {
		name     string
		query    url.Values
		requests int64
		models   int
	}{
		{"all", url.Values{}, 3, 2},
		{"by model", url.Values{"model": {"m2"}}, 1, 1},
		{"by provider", url.Values{"provider": {"anthropic"}}, 1, 1},
		{"by auth", url.Values{"auth_id": {"auth-1"}}, 2, 1},
		{"by search", url.Values{"search": {"r3"}}, 1, 1},
		{"by bucket", url.Values{"bucket": {"hour"}}, 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := h.HandleAnalytics(context.Background(), overviewRequest(tc.query))
			if got.StatusCode != http.StatusOK {
				t.Fatalf("status = %d body = %s", got.StatusCode, got.Body)
			}
			body := decodeOverview(t, got.Body)
			if body.Summary.Requests != tc.requests || len(body.Models) != tc.models {
				t.Fatalf("summary = %+v models = %+v, want %d requests / %d models", body.Summary, body.Models, tc.requests, tc.models)
			}
		})
	}
}

// TestHandleAnalyticsBoundary rejects malformed or misplaced calls without
// reaching the ledger.
func TestHandleAnalyticsBoundary(t *testing.T) {
	h := testHandler(t)

	post := managementRequest(http.MethodPost, AnalyticsOverview, "")
	if got := h.HandleAnalytics(context.Background(), post); got.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", got.StatusCode)
	}
	badFrom := overviewRequest(url.Values{"from": {"not-a-date"}})
	if got := h.HandleAnalytics(context.Background(), badFrom); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad from status = %d, want 400", got.StatusCode)
	}
	same := overviewRequest(url.Values{"from": {"2026-01-01T00:00:00Z"}, "to": {"2026-01-01T00:00:00Z"}})
	if got := h.HandleAnalytics(context.Background(), same); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty range status = %d, want 400", got.StatusCode)
	}
	// A public resource spelling must never reach the overview.
	public := overviewRequest(url.Values{})
	public.Path = "/v0/resource/plugins/credit-manager/overview"
	if got := h.HandleAnalytics(context.Background(), public); got.StatusCode != http.StatusNotFound {
		t.Fatalf("public path status = %d, want 404", got.StatusCode)
	}
}

// TestHandleAnalyticsWithoutCapability returns an explicit unavailable status
// for a backend that cannot aggregate the whole dataset.
func TestHandleAnalyticsWithoutCapability(t *testing.T) {
	h := testHandler(t)
	wrapped := New(backendWithoutAnalytics{h.db}, h.captures, h.cfg)
	got := wrapped.HandleAnalytics(context.Background(), overviewRequest(url.Values{}))
	if got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s, want 503", got.StatusCode, got.Body)
	}
}
