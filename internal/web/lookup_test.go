package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/capture"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/config"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// exactAssets is the deliberate public asset set. The serving allow list must
// match it exactly: adding a name here is a security decision, and no other
// embedded file may become reachable through Asset.
var exactAssets = []string{
	"admin-pricing.js",
	"app.js",
	"charts.js",
	"index.html",
	"lookup.css",
	"lookup.html",
	"lookup.js",
	"numbers.js",
	"style.css",
	"vendor/echarts-5.6.0.min.js",
}

// TestAssetAllowListIsExact proves the served set is a fixed, exact path allow
// list: an extension is never enough to serve a file, and no aliasing spelling
// (traversal, "./", a directory suffix, a case change) reaches any asset.
func TestAssetAllowListIsExact(t *testing.T) {
	names := AssetNames()
	if len(names) != len(exactAssets) {
		t.Fatalf("AssetNames = %v, want %v", names, exactAssets)
	}
	for i, want := range exactAssets {
		if names[i] != want {
			t.Fatalf("AssetNames[%d] = %q, want %q (must stay sorted and exact)", i, names[i], want)
		}
	}

	for _, name := range exactAssets {
		data, mime, ok := Asset(name)
		if !ok || len(data) == 0 {
			t.Fatalf("declared asset %s is not served", name)
		}
		if !strings.HasPrefix(mime, "text/") {
			t.Fatalf("asset %s mime = %q", name, mime)
		}
	}
	// Every rejection below shares the same file extension with a real asset,
	// so an extension-driven rule would have served it.
	for _, name := range []string{
		"", " ", "/app.js", "./app.js", "../app.js", "assets/app.js",
		"app.js/", "app.js/../app.js", "index.html/../app.js",
		"secret.txt", "maps.js", "extra.css", "INDEX.HTML", "App.js",
		"vendor/../app.js", "vendor/echarts-5.6.0.min.js.bak", "vendor/app.js",
	} {
		if _, _, ok := Asset(name); ok {
			t.Fatalf("Asset(%q) must be rejected by the exact allow list", name)
		}
	}
}

// TestLookupAssetsAreSelfContainedAndSharedChartsWired verifies the public
// lookup page: only relative, first-party references; no inline credential or
// storage access in the HTML; the real shared chart assets are wired rather than
// a private chart implementation; and the hour/day/month grain controls of the
// original lookup page exist.
func TestLookupAssetsAreSelfContainedAndSharedChartsWired(t *testing.T) {
	html := readAsset(t, "lookup.html")
	js := readAsset(t, "lookup.js")
	css := readAsset(t, "lookup.css")

	refs := regexp.MustCompile(`(?:href|src)="([^"]+)"`).FindAllStringSubmatch(html, -1)
	got := make([]string, 0, len(refs))
	for _, match := range refs {
		got = append(got, match[1])
	}
	want := []string{"./lookup.css", "./vendor/echarts-5.6.0.min.js", "./charts.js", "./numbers.js", "./lookup.js"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lookup.html references = %v, want %v", got, want)
	}
	if strings.Contains(html, "http://") || strings.Contains(html, "https://") {
		t.Error("lookup.html must not reference an external CDN")
	}
	if regexp.MustCompile(`localStorage\s*[.=\[]|sessionStorage\s*[.=\[]|document\.cookie\s*=`).MatchString(html) {
		t.Error("lookup.html must not touch browser storage or cookies inline")
	}
	if !strings.Contains(html, `lang="zh-CN"`) {
		t.Error("lookup page language should be Simplified Chinese")
	}
	if regexp.MustCompile(`sk-[A-Za-z0-9]{8,}`).MatchString(html) || strings.Contains(html, "Bearer") {
		t.Error("lookup.html must not embed key material or credential literals")
	}
	// The page must expose the three trend panels and the hour/day/month grain.
	for _, id := range []string{`id="trend-requests"`, `id="trend-tokens"`, `id="trend-cost"`} {
		if !strings.Contains(html, id) {
			t.Errorf("lookup.html is missing trend container %s", id)
		}
	}
	for _, grain := range []string{`data-grain="hour"`, `data-grain="day"`, `data-grain="month"`} {
		if !strings.Contains(html, grain) {
			t.Errorf("lookup.html is missing grain control %s", grain)
		}
	}

	// The page drives the shared renderer; it does not ship a private chart.
	if !strings.Contains(js, "CMCharts") || !strings.Contains(js, "charts().render(") {
		t.Error("lookup.js must render through the shared CMCharts.render entry point")
	}
	for _, option := range []string{"bucket:", "tokenUnit:", "currency:", "rate:"} {
		if !strings.Contains(js, option) {
			t.Errorf("lookup.js must pass chart option %s", option)
		}
	}

	// The shared renderer reuses the ECharts instance bound to a container, so
	// the container must not be emptied before the shared render call: doing so
	// detaches the instance and leaves a blank chart on every later refresh.
	start := strings.Index(js, "function renderTrend()")
	if start < 0 {
		t.Fatal("lookup.js is missing renderTrend")
	}
	cut := strings.Index(js[start:], "charts().render(")
	if cut < 0 {
		t.Fatal("renderTrend must call the shared renderer")
	}
	if head := js[start : start+cut]; strings.Contains(head, "clear(") {
		t.Error("renderTrend must not clear the chart containers before reusing ECharts")
	}
	// The shared modules are required shipped assets: there is no fallback
	// chart and no degraded numeric path that could fake financial values.
	for _, gone := range []string{"renderTrendFallback", "fallbackBar", "JSON.parse(text)"} {
		if strings.Contains(js, gone) {
			t.Errorf("lookup.js must not keep the degraded implementation %s", gone)
		}
	}
	if !strings.Contains(js, "页面资源加载不完整") || !strings.Contains(js, "el('submit').disabled = true") {
		t.Error("lookup.js must fail closed with a visible error when a shared module is missing")
	}
	// Preferences change the display contract of every panel, not just charts.
	if !strings.Contains(js, "renderAll()") {
		t.Error("lookup.js must re-render the whole page when display preferences change")
	}
	for _, call := range []string{"renderQuotas(", "renderStats(", "renderRecent(", "renderTrend()"} {
		idx := strings.Index(js, "function renderAll()")
		if idx < 0 || !strings.Contains(js[idx:idx+1200], call) {
			t.Errorf("renderAll must redraw %s", call)
		}
	}
	if strings.Contains(js, "charts().formatDateTime") || strings.Contains(js, "charts().normalizeCurrency") ||
		strings.Contains(js, "charts().normalizeTokenUnit") {
		t.Error("lookup.js must only call the exported shared chart API")
	}

	// Amounts and token counts are 64-bit values: the JSON body is parsed with
	// the lossless shared helper instead of response.json(), which would round
	// every integer above 2^53.
	if !strings.Contains(js, "CMNumbers") || !strings.Contains(js, "parseBody(") {
		t.Error("lookup.js must parse responses with the lossless CMNumbers helper")
	}
	if strings.Contains(js, "response.json()") {
		t.Error("lookup.js must not use response.json(): it loses 64-bit precision")
	}
	// The chosen grain is sent to the server as-is (true calendar months), and
	// late responses from a superseded request are dropped.
	for _, want := range []string{"normalizeGrain(state.grain)", "AbortController", "epoch"} {
		if !strings.Contains(js, want) {
			t.Errorf("lookup.js must contain %s", want)
		}
	}
	if strings.Contains(js, "serverBucket") {
		t.Error("the trend grain must not be remapped to a coarser server bucket")
	}
	// Theme switches must recolour already rendered charts, and the page reads
	// the CPA theme key itself instead of depending on another preferences
	// module asset.
	if !strings.Contains(js, "refreshTheme") {
		t.Error("lookup.js must refresh charts when the CPA theme changes")
	}
	if !strings.Contains(js, "'cli-proxy-theme'") {
		t.Error("lookup.js must read the CPA theme key directly")
	}
	if strings.Contains(js, "CMPreferences") || strings.Contains(html, "preferences.js") {
		t.Error("the lookup page must not depend on a separate preferences asset")
	}

	// The backend returns the key's configured model token limits; the page
	// must surface them instead of dropping the field.
	for _, want := range []string{`id="token-limits"`, `id="token-limits-state"`} {
		if !strings.Contains(html, want) {
			t.Errorf("lookup.html is missing model token limit section %s", want)
		}
	}
	if !strings.Contains(js, "model_token_limits") {
		t.Error("lookup.js must render the model token limits returned by the backend")
	}

	// The key is sent as an explicit bearer header and never read from the URL.
	if !strings.Contains(js, "Authorization: 'Bearer '") {
		t.Error("lookup requests must carry an Authorization Bearer header")
	}
	if !strings.Contains(js, "credentials: 'same-origin'") || !strings.Contains(js, "cache: 'no-store'") {
		t.Error("lookup requests must be same-origin and uncached")
	}
	if strings.Contains(js, "location.search") || strings.Contains(js, "key=") {
		t.Error("the customer key must never be read from the query string")
	}

	// Remembering the customer key is an approved, explicit opt-in. It must use
	// the lookup-only storage prefix, distinct from the console's management key.
	if !strings.Contains(js, "'credit-manager.lookup.'") {
		t.Error("remembered key must use the dedicated credit-manager.lookup. prefix")
	}
	if !strings.Contains(js, "localStorage") || !strings.Contains(js, "sessionStorage") {
		t.Error("approved optional remember must be able to persist the key")
	}
	if strings.Contains(js, "credit-manager.management") {
		t.Error("the lookup page must not reuse the console management key storage")
	}

	for _, forbidden := range []string{
		`\.innerHTML\s*=`, `\.outerHTML\s*=`, `insertAdjacentHTML`, `document\.write`, `\beval\s*\(`,
	} {
		if regexp.MustCompile(forbidden).MatchString(js) {
			t.Errorf("lookup.js must not use %s", forbidden)
		}
	}
	if !strings.Contains(js, "document.createTextNode") || !strings.Contains(js, "textContent") {
		t.Error("server data must be written via textContent/createTextNode")
	}

	if !strings.Contains(css, "@media (max-width: 720px)") {
		t.Error("lookup.css needs a narrow-screen breakpoint")
	}
	if regexp.MustCompile(`url\(\s*['"]?https?:`).MatchString(css) || strings.Contains(css, "@import") {
		t.Error("lookup.css must not load external resources")
	}
}

/* ------------------------------------------------------------------ *
 * Handler behavior
 * ------------------------------------------------------------------ */

func createTestKey(t *testing.T, db credit.Backend, material string, spec credit.KeySpec) credit.Key {
	t.Helper()
	spec.Material = material
	spec.Enabled = true
	key, returned, err := db.CreateKey(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if returned != material {
		t.Fatalf("material = %q, want the registered %q", returned, material)
	}
	return key
}

// seedRequest admits and settles one request for the given key so the report
// has an item to serve, and returns its id.
func seedRequest(t *testing.T, db credit.Backend, key credit.Key, id, trace string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Admit(ctx, credit.AdmitInput{ID: id, TraceID: trace, KeyID: key.ID, RequestedModel: "m1"}); err != nil {
		t.Fatalf("Admit(%s): %v", id, err)
	}
	if err := db.RecordUsage(ctx, credit.Usage{
		ID: "exec-" + id, RequestID: id, TraceID: trace, KeyID: key.ID,
		Provider: "openai", AuthID: "auth-secret-id", AuthIndex: "auth-secret-index", Executor: "openaiExecutor",
		Model: "m1", Tokens: credit.Tokens{Input: 10, Total: 10}, Quality: credit.QualityComplete,
	}); err != nil {
		t.Fatalf("RecordUsage(%s): %v", id, err)
	}
	if err := db.Complete(ctx, credit.Completion{ID: id, TraceID: trace, Outcome: credit.OutcomeSucceeded}); err != nil {
		t.Fatalf("Complete(%s): %v", id, err)
	}
}

func lookupCall(t *testing.T, h *Handler, action, material string, query url.Values) pluginapi.ManagementResponse {
	t.Helper()
	headers := http.Header{}
	if material != "" {
		headers.Set("Authorization", "Bearer "+material)
	}
	if query == nil {
		query = url.Values{}
	}
	return h.HandleLookup(action, pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: headers, Query: query,
	})
}

// TestLookupDataRequiresExplicitCustomerBearer rejects every credential shape
// that is not an explicit Bearer customer key, so the host management secret and
// a URL-supplied key can never open the public endpoint.
func TestLookupDataRequiresExplicitCustomerBearer(t *testing.T) {
	h := testHandler(t)
	if key := createTestKey(t, h.db, "synthetic-customer-key", credit.KeySpec{Label: "customer"}); key.ID == "" {
		t.Fatal("customer key was not created")
	}

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"missing", ""},
		{"bare-token", "synthetic-customer-key"},
		{"wrong-scheme", "Basic synthetic-customer-key"},
		{"unknown-key", "Bearer synthetic-not-a-key"},
		{"management-key", "Bearer synthetic-manager"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			if tc.header != "" {
				headers.Set("Authorization", tc.header)
			}
			got := h.HandleLookup(PublicLookupData, pluginapi.ManagementRequest{
				Method: http.MethodGet, Headers: headers, Query: url.Values{},
			})
			if got.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
			}
		})
	}
	// The key is only ever a header: a query parameter alone must not authenticate.
	got := h.HandleLookup(PublicLookupData, pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: http.Header{},
		Query: url.Values{"key": {"synthetic-customer-key"}},
	})
	if got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query key must not authenticate: %d %s", got.StatusCode, got.Body)
	}
	// Only GET is a read.
	post := h.HandleLookup(PublicLookupData, pluginapi.ManagementRequest{
		Method: http.MethodPost, Headers: http.Header{"Authorization": {"Bearer synthetic-customer-key"}}, Query: url.Values{},
	})
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", post.StatusCode)
	}
	// A valid key is served.
	if ok := lookupCall(t, h, PublicLookupData, "synthetic-customer-key", nil); ok.StatusCode != http.StatusOK {
		t.Fatalf("valid key status=%d body=%s", ok.StatusCode, ok.Body)
	}
}

// TestLookupDataIsConfinedAndSanitized proves the public report is forced to the
// authenticated key's own data even when an attacker supplies another key_id,
// and that the response never carries principal, scope, fingerprint or the
// operator-side auth identifiers.
func TestLookupDataIsConfinedAndSanitized(t *testing.T) {
	h := testHandler(t)
	if err := h.db.PutPrice(context.Background(), credit.Price{Model: "m1", Rates: credit.Rates{Input: 1_000_000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	alpha := createTestKey(t, h.db, "synthetic-alpha-key", credit.KeySpec{Label: "alpha"})
	beta := createTestKey(t, h.db, "synthetic-beta-key", credit.KeySpec{Label: "beta"})
	seedRequest(t, h.db, alpha, "req-alpha", "trace-alpha")
	seedRequest(t, h.db, beta, "req-beta", "trace-beta")

	// Attack: ask for beta's key explicitly. The handler must ignore it.
	got := lookupCall(t, h, PublicLookupData, "synthetic-alpha-key", url.Values{"key_id": {beta.ID}})
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}
	body := string(got.Body)
	if !strings.Contains(body, "req-alpha") {
		t.Fatalf("own request missing from report: %s", body)
	}
	for _, leaked := range []string{
		"req-beta", beta.ID, beta.Principal, "beta",
		"principal", "fingerprint", "scope", "hmac",
		"auth_id", "auth_index", "executor", `"raw"`,
		"synthetic-alpha-key", alpha.Principal, alpha.Fingerprint,
		"held_micro_usd",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("public report leaked %q: %s", leaked, body)
		}
	}
	// The response is financial data and must never be cached.
	if got.Headers.Get("Cache-Control") != "no-store" {
		t.Error("lookup response must be uncacheable")
	}
	var out struct {
		Key   lookupKey       `json:"key"`
		Items []lookupRequest `json:"items"`
	}
	if err := json.Unmarshal(got.Body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Key.ID != alpha.ID || out.Key.Label != "alpha" {
		t.Fatalf("key summary = %+v, want alpha", out.Key)
	}
	if len(out.Items) != 1 || out.Items[0].ID != "req-alpha" {
		t.Fatalf("items = %+v, want only req-alpha", out.Items)
	}
	if len(out.Items[0].Executions) != 1 || out.Items[0].Executions[0].Provider != "openai" {
		t.Fatalf("execution view = %+v", out.Items[0].Executions)
	}
}

// TestLookupPricingIsQueryTimeActualModelAndBudgetOnly pins the customer read
// contract end to end: the four budget figures remain display data, no retired
// financial field is serialized, an execution is priced by its actual usage
// model (never the request alias), and adding a price recomputes already stored
// history. A partial-quality execution stays explicitly unpriced.
func TestLookupPricingIsQueryTimeActualModelAndBudgetOnly(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	key := createTestKey(t, h.db, "synthetic-priced-key", credit.KeySpec{
		Label:         "priced",
		QuotaMicroUSD: 1, DailyMicroUSD: 2, WeeklyMicroUSD: 3, MonthlyMicroUSD: 4,
	})
	seed := func(requestID, traceID, requestedModel, usageModel, quality, tier string, input int64) {
		t.Helper()
		if _, err := h.db.Admit(ctx, credit.AdmitInput{
			ID: requestID, TraceID: traceID, KeyID: key.ID, RequestedModel: requestedModel,
			ServiceTier: tier,
		}); err != nil {
			t.Fatalf("Admit(%s): %v", requestID, err)
		}
		if err := h.db.RecordUsage(ctx, credit.Usage{
			ID: "exec-" + requestID, RequestID: requestID, TraceID: traceID, KeyID: key.ID,
			Provider: "openai", Model: usageModel,
			Tokens: credit.Tokens{Input: input, Total: input}, Quality: quality,
		}); err != nil {
			t.Fatalf("RecordUsage(%s): %v", requestID, err)
		}
		if err := h.db.Complete(ctx, credit.Completion{ID: requestID, TraceID: traceID, Outcome: credit.OutcomeSucceeded}); err != nil {
			t.Fatalf("Complete(%s): %v", requestID, err)
		}
	}
	// The first request is an alias whose actual execution model is m1; the
	// second is partial-quality usage for m2 and can never be priced.
	seed("req-priced", "trace-priced", "alias-model", "m1", credit.QualityComplete, "priority", 10)
	seed("req-partial", "trace-partial", "m2", "m2", credit.QualityPartial, "", 5)

	lookup := func() (lookupData, string) {
		t.Helper()
		got := lookupCall(t, h, PublicLookupData, "synthetic-priced-key", nil)
		if got.StatusCode != http.StatusOK {
			t.Fatalf("lookup = %d %s", got.StatusCode, got.Body)
		}
		var out lookupData
		if err := json.Unmarshal(got.Body, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out, string(got.Body)
	}
	costByRequest := func(out lookupData) map[string]*int64 {
		costs := map[string]*int64{}
		for _, item := range out.Items {
			for _, execution := range item.Executions {
				costs[item.ID] = execution.CostMicroUSD
			}
		}
		return costs
	}

	out, body := lookup()
	if out.Key.QuotaMicroUSD != 1 || out.Key.DailyMicroUSD != 2 ||
		out.Key.WeeklyMicroUSD != 3 || out.Key.MonthlyMicroUSD != 4 {
		t.Fatalf("display budgets = %+v, want the four stored figures", out.Key)
	}
	if out.Summary.CostMicroUSD != 0 || out.Summary.UnpricedExecutions != 2 {
		t.Fatalf("unpriced summary = %+v", out.Summary)
	}
	for id, cost := range costByRequest(out) {
		if cost != nil {
			t.Fatalf("request %s cost = %d before any price, want null", id, *cost)
		}
	}
	for _, item := range out.Items {
		if item.ID == "req-priced" && item.ServiceTier != "priority" {
			t.Fatalf("request service_tier = %q, want the request fact priority", item.ServiceTier)
		}
	}
	if !strings.Contains(body, `"service_tier":"priority"`) {
		t.Fatalf("lookup must retain the request service_tier: %s", body)
	}
	if !strings.Contains(body, `"cost_micro_usd":null`) {
		t.Fatalf("unpriced cost must be explicit null: %s", body)
	}
	for _, retired := range []string{
		"spent_micro_usd", "quota_used_micro_usd", "daily_used_micro_usd",
		"weekly_used_micro_usd", "monthly_used_micro_usd", "adjustment_micro_usd",
		"price_tiers", "pricing_rule_id", "image_count",
	} {
		if strings.Contains(body, retired) {
			t.Errorf("lookup exposes retired field %q: %s", retired, body)
		}
	}

	// Pricing the request alias must not price an execution whose actual model
	// is m1.
	if err := h.db.PutPrice(ctx, credit.Price{Model: "alias-model", Rates: credit.Rates{Input: 9_000_000}}); err != nil {
		t.Fatal(err)
	}
	out, _ = lookup()
	if out.Summary.CostMicroUSD != 0 || out.Summary.UnpricedExecutions != 2 {
		t.Fatalf("request alias was priced instead of the actual model: %+v", out.Summary)
	}

	// Adding the actual model price recomputes the existing execution.
	if err := h.db.PutPrice(ctx, credit.Price{Model: "m1", Rates: credit.Rates{Input: 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	out, _ = lookup()
	if out.Summary.CostMicroUSD != 10 || out.Summary.UnpricedExecutions != 1 {
		t.Fatalf("partial summary = %+v, want 10 priced + one unpriced", out.Summary)
	}
	if cost := costByRequest(out)["req-priced"]; cost == nil || *cost != 10 {
		t.Fatalf("actual-model cost = %v, want 10", cost)
	}

	// A partial-quality execution stays null even after its model is priced.
	if err := h.db.PutPrice(ctx, credit.Price{Model: "m2", Rates: credit.Rates{Input: 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	out, _ = lookup()
	if out.Summary.CostMicroUSD != 10 || out.Summary.UnpricedExecutions != 1 {
		t.Fatalf("partial execution became priced: %+v", out.Summary)
	}
	if cost := costByRequest(out)["req-partial"]; cost != nil {
		t.Fatalf("partial-quality cost = %d, want null", *cost)
	}
}

// disabled, expired and rotated-out material all fail closed.
func TestLookupDeniesInactiveKeys(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()

	disabled := createTestKey(t, h.db, "synthetic-disabled-key", credit.KeySpec{})
	spec := disabled.Spec
	spec.Enabled = false
	if _, err := h.db.UpdateKey(ctx, disabled.ID, spec); err != nil {
		t.Fatalf("disable: %v", err)
	}

	past := time.Now().Add(-time.Hour).UTC()
	createTestKey(t, h.db, "synthetic-expired-key", credit.KeySpec{ExpiresAt: &past})

	rotated := createTestKey(t, h.db, "synthetic-rotated-old", credit.KeySpec{})
	_, fresh, err := h.db.RotateKey(ctx, rotated.ID, "")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	for _, tc := range []struct {
		name     string
		material string
		want     int
	}{
		{"disabled", "synthetic-disabled-key", http.StatusUnauthorized},
		{"expired", "synthetic-expired-key", http.StatusUnauthorized},
		{"rotated-out", "synthetic-rotated-old", http.StatusUnauthorized},
		{"rotated-in", fresh, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lookupCall(t, h, PublicLookupData, tc.material, nil); got.StatusCode != tc.want {
				t.Fatalf("status=%d want %d body=%s", got.StatusCode, tc.want, got.Body)
			}
		})
	}
}

// TestLookupCaptureOwnershipIsNoOracle proves the snapshot endpoint only serves
// the owner's request, and that a foreign id, an unknown id and an unavailable
// snapshot are indistinguishable 404s.
func TestLookupCaptureOwnershipIsNoOracle(t *testing.T) {
	h := testHandler(t) // capture disabled: Get would answer "disabled" for any id
	if err := h.db.PutPrice(context.Background(), credit.Price{Model: "m1", Rates: credit.Rates{Input: 1_000_000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	alpha := createTestKey(t, h.db, "synthetic-alpha-key", credit.KeySpec{})
	seedRequest(t, h.db, alpha, "req-alpha", "trace-alpha")
	createTestKey(t, h.db, "synthetic-beta-key", credit.KeySpec{})

	// Missing id is a bad request, never a lookup.
	if got := lookupCall(t, h, PublicLookupCapture, "synthetic-alpha-key", nil); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing id status=%d body=%s", got.StatusCode, got.Body)
	}
	// Foreign and unknown ids are the same generic 404 even though capture is
	// disabled and would otherwise answer "disabled" for every id.
	for _, tc := range []struct{ name, material, id string }{
		{"foreign-key", "synthetic-beta-key", "req-alpha"},
		{"unknown-request", "synthetic-alpha-key", "req-missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lookupCall(t, h, PublicLookupCapture, tc.material, url.Values{"id": {tc.id}})
			if got.StatusCode != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
			}
			if strings.Contains(string(got.Body), tc.id) {
				t.Errorf("404 body must not echo the request id: %s", got.Body)
			}
		})
	}
	// The owner reaches the capture store, which reports the disabled state for
	// its own request.
	own := lookupCall(t, h, PublicLookupCapture, "synthetic-alpha-key", url.Values{"id": {"req-alpha"}})
	if own.StatusCode != http.StatusOK || !strings.Contains(string(own.Body), `"status":"disabled"`) {
		t.Fatalf("owner capture status=%d body=%s", own.StatusCode, own.Body)
	}
}

// TestLookupCaptureServesOwnSnapshotAndRedacts proves a captured snapshot is
// returned only to its owner, with Authorization redacted.
func TestLookupCaptureServesOwnSnapshotAndRedacts(t *testing.T) {
	handler, store := captureEnabledHandler(t)
	if err := handler.db.PutPrice(context.Background(), credit.Price{Model: "m1", Rates: credit.Rates{Input: 1_000_000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	alpha := createTestKey(t, handler.db, "synthetic-alpha-key", credit.KeySpec{})
	seedRequest(t, handler.db, alpha, "req-alpha", "trace-alpha")
	createTestKey(t, handler.db, "synthetic-beta-key", credit.KeySpec{})

	store.Enqueue(capture.Snapshot{
		RequestID: "req-alpha",
		Headers:   http.Header{"X-Test": {"1"}, "Authorization": {"Bearer captured-secret"}},
		Body:      []byte(`{"model":"m1"}`),
		At:        time.Now().UTC(),
	})
	waitCaptured(t, store, "req-alpha")

	got := lookupCall(t, handler, PublicLookupCapture, "synthetic-alpha-key", url.Values{"id": {"req-alpha"}})
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}
	detail := string(got.Body)
	if !strings.Contains(detail, `"status":"captured"`) || !strings.Contains(detail, "X-Test") {
		t.Fatalf("captured snapshot missing: %s", detail)
	}
	if strings.Contains(detail, "captured-secret") {
		t.Fatalf("authorization header was not redacted: %s", detail)
	}
	// The same snapshot is invisible to a different key.
	if cross := lookupCall(t, handler, PublicLookupCapture, "synthetic-beta-key", url.Values{"id": {"req-alpha"}}); cross.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-key snapshot status=%d body=%s", cross.StatusCode, cross.Body)
	}
}

// TestLookupCaptureWithoutOwnershipBackendRefusesSnapshot verifies the optional
// narrow interface: a backend that cannot prove ownership refuses the snapshot
// instead of exposing a request, while the usage report keeps working.
func TestLookupCaptureWithoutOwnershipBackendRefusesSnapshot(t *testing.T) {
	base := testHandler(t)
	if err := base.db.PutPrice(context.Background(), credit.Price{Model: "m1", Rates: credit.Rates{Input: 1_000_000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	key := createTestKey(t, base.db, "synthetic-alpha-key", credit.KeySpec{})
	seedRequest(t, base.db, key, "req-alpha", "trace-alpha")

	handler := New(backendWithoutLookup{Backend: base.db}, base.captures, base.cfg)
	if got := lookupCall(t, handler, PublicLookupCapture, "synthetic-alpha-key", url.Values{"id": {"req-alpha"}}); got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("snapshot without ownership backend status=%d body=%s", got.StatusCode, got.Body)
	}
	if got := lookupCall(t, handler, PublicLookupData, "synthetic-alpha-key", nil); got.StatusCode != http.StatusOK {
		t.Fatalf("usage report must still work: %d %s", got.StatusCode, got.Body)
	}
}

// backendWithoutLookup satisfies credit.Backend but not the optional
// requestLookup capability.
type backendWithoutLookup struct{ credit.Backend }

// captureEnabledHandler builds a handler whose capture store can hold snapshots.
func captureEnabledHandler(t *testing.T) (*Handler, *capture.Store) {
	t.Helper()
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	db, err := credit.Open(credit.Options{DataDir: cfg.DataDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	captures, err := capture.Open(cfg.DataDir, capture.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = captures.Close() })
	return New(db, captures, cfg), captures
}

// waitCaptured blocks until the asynchronous capture writer has persisted the
// snapshot, so the assertion never races the writer.
func waitCaptured(t *testing.T, store *capture.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if detail, err := store.Get(id); err == nil && detail.Status == capture.StatusCaptured {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("snapshot %s was never captured", id)
}
