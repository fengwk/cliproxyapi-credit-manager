package plugin

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/web"
)

const lookupResourceBase = testResourceBase

// lookupCall drives one public resource request through the plugin dispatcher
// and decodes the host-facing management response.
func lookupCall(t *testing.T, h *harness, path, material string, query url.Values) pluginapi.ManagementResponse {
	t.Helper()
	headers := http.Header{}
	if material != "" {
		headers.Set("Authorization", "Bearer "+material)
	}
	if query == nil {
		query = url.Values{}
	}
	envelope := h.call(pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: path, Headers: headers, Query: query,
	})
	if !envelope.OK {
		t.Fatalf("lookup call failed: %s", envelope.Error)
	}
	return decodeInto[pluginapi.ManagementResponse](t, envelope)
}

// TestLookupResourcesRegisteredWithoutMenuDuplicate verifies the public
// self-service page and endpoints are declared as explicit resource routes, that
// the shared chart assets stay in the declared allow list, and that the sidebar
// still shows exactly one menu entry.
func TestLookupResourcesRegisteredWithoutMenuDuplicate(t *testing.T) {
	h := newHarness(t)
	envelope := h.call(pluginabi.MethodManagementRegister, pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: lookupResourceBase,
	})
	registration := decodeInto[managementRegistrationResponse](t, envelope)

	byPath := map[string]pluginapi.ResourceRoute{}
	menus := 0
	for _, resource := range registration.Resources {
		if !strings.HasPrefix(resource.Path, lookupResourceBase+"/") {
			t.Fatalf("resource %s escapes the public base", resource.Path)
		}
		byPath[resource.Path] = resource
		if resource.Menu != "" {
			menus++
			if resource.Path != lookupResourceBase+"/index.html" {
				t.Fatalf("menu entry on unexpected resource %s", resource.Path)
			}
		}
	}
	if menus != 1 {
		t.Fatalf("menu entries = %d, want exactly the console entry", menus)
	}
	for _, name := range []string{
		"index.html", "app.js", "style.css", "charts.js", "vendor/echarts-5.6.0.min.js",
		"lookup.html", "lookup.css", "lookup.js",
	} {
		if _, ok := byPath[lookupResourceBase+"/"+name]; !ok {
			t.Errorf("static asset %s is not declared", name)
		}
	}
	for _, route := range []string{web.PublicLookupData, web.PublicLookupCapture} {
		resource, ok := byPath[lookupResourceBase+"/"+route]
		if !ok {
			t.Fatalf("public lookup route %s is not declared", route)
		}
		if resource.Menu != "" {
			t.Fatalf("lookup route %s must not duplicate the console menu", route)
		}
	}
}

// TestLookupDispatchOwnDataOnly proves the public endpoint serves the caller's
// own data and nothing else: no other key's request or identity field, and an
// attacker supplied key_id is ignored.
func TestLookupDispatchOwnDataOnly(t *testing.T) {
	h := newHarness(t)

	// The caller's own request, settled through the normal plugin path.
	if response := h.intercept(h.request("req-own", "trace-own")); response.Terminate {
		t.Fatalf("reserve own request: %+v", response)
	}
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-own", TraceID: "trace-own", Provider: "openai", AuthID: "auth-secret-id",
		AuthIndex: "auth-secret-index", ExecutorType: "openaiExecutor", Model: testModel, APIKey: h.key.Principal,
		AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail:         pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	h.complete("req-own", "trace-own", pluginapi.RequestCompletionSucceeded)

	// Another key's request, seeded directly in the ledger.
	beta, _, err := h.store().CreateKey(context.Background(), credit.KeySpec{Label: "beta", Enabled: true})
	if err != nil {
		t.Fatalf("CreateKey(beta): %v", err)
	}
	seedLedgerRequest(t, h, beta, "req-beta", "trace-beta")

	path := lookupResourceBase + "/" + web.PublicLookupData

	// No credential and the management secret are both refused.
	if got := lookupCall(t, h, path, "", nil); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", got.StatusCode, got.Body)
	}
	if got := lookupCall(t, h, path, "synthetic-manager-secret", nil); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("management key status=%d body=%s", got.StatusCode, got.Body)
	}

	// Attack: request beta's data by key_id. It must be ignored.
	got := lookupCall(t, h, path, h.plain, url.Values{"key_id": {beta.ID}})
	if got.StatusCode != http.StatusOK {
		t.Fatalf("own status=%d body=%s", got.StatusCode, got.Body)
	}
	body := string(got.Body)
	if !strings.Contains(body, "req-own") {
		t.Fatalf("own request missing: %s", body)
	}
	for _, leaked := range []string{
		"req-beta", beta.ID, beta.Principal, `"beta"`,
		"principal", "fingerprint", "scope", "hmac",
		"auth_id", "auth_index", "executor", "auth-secret",
		h.plain,
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("public report leaked %q: %s", leaked, body)
		}
	}
	if got.Headers.Get("Cache-Control") != "no-store" {
		t.Error("lookup response must be uncacheable")
	}
}

// TestLookupDispatchDeniesInactiveCustomerKey mirrors the inference contract:
// disabled, expired and rotated-out material are all rejected.
func TestLookupDispatchDeniesInactiveCustomerKey(t *testing.T) {
	h := newHarness(t)
	path := lookupResourceBase + "/" + web.PublicLookupData

	_, fresh, err := h.store().RotateKey(context.Background(), h.key.ID, "")
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if got := lookupCall(t, h, path, h.plain, nil); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rotated-out status=%d", got.StatusCode)
	}
	if got := lookupCall(t, h, path, fresh, nil); got.StatusCode != http.StatusOK {
		t.Fatalf("rotated-in status=%d body=%s", got.StatusCode, got.Body)
	}

	if _, _, err := h.store().CreateKey(context.Background(), credit.KeySpec{Enabled: false, Material: "synthetic-disabled-key"}); err != nil {
		t.Fatalf("CreateKey(disabled): %v", err)
	}
	if got := lookupCall(t, h, path, "synthetic-disabled-key", nil); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled status=%d", got.StatusCode)
	}

	past := time.Now().Add(-time.Hour).UTC()
	if _, _, err := h.store().CreateKey(context.Background(), credit.KeySpec{Enabled: true, Material: "synthetic-expired-key", ExpiresAt: &past}); err != nil {
		t.Fatalf("CreateKey(expired): %v", err)
	}
	if got := lookupCall(t, h, path, "synthetic-expired-key", nil); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired status=%d", got.StatusCode)
	}
}

// TestLookupDispatchCaptureOwnership covers the snapshot gate: only the owner's
// request reaches the capture store, and a foreign or unknown id is a generic
// 404 even when capture is disabled and would answer for every id.
func TestLookupDispatchCaptureOwnership(t *testing.T) {
	h := newHarness(t) // request capture is disabled in the default harness
	if response := h.intercept(h.request("req-own", "trace-own")); response.Terminate {
		t.Fatalf("reserve own request: %+v", response)
	}
	if _, _, err := h.store().CreateKey(context.Background(), credit.KeySpec{Label: "beta", Enabled: true, Material: "synthetic-beta-key"}); err != nil {
		t.Fatalf("CreateKey(beta): %v", err)
	}
	path := lookupResourceBase + "/" + web.PublicLookupCapture

	if got := lookupCall(t, h, path, h.plain, nil); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing id status=%d body=%s", got.StatusCode, got.Body)
	}
	// The owner's request id is served by the (disabled) capture store.
	own := lookupCall(t, h, path, h.plain, url.Values{"id": {"req-own"}})
	if own.StatusCode != http.StatusOK || !strings.Contains(string(own.Body), `"status":"disabled"`) {
		t.Fatalf("own snapshot status=%d body=%s", own.StatusCode, own.Body)
	}
	// Another key's id and an unknown id are the same generic 404.
	for _, tc := range []struct{ name, material, id string }{
		{"foreign", "synthetic-beta-key", "req-own"},
		{"unknown", h.plain, "req-missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lookupCall(t, h, path, tc.material, url.Values{"id": {tc.id}})
			if got.StatusCode != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
			}
			if strings.Contains(string(got.Body), tc.id) {
				t.Errorf("404 body must not echo the request id: %s", got.Body)
			}
		})
	}
}

// TestLookupDispatchRejectsForgedPaths proves no URL shape can reach the
// financial API or any undeclared asset through the public resource base, and
// that a different plugin id is never treated as this plugin's resource base.
func TestLookupDispatchRejectsForgedPaths(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"management-api-under-resource", lookupResourceBase + "/api/keys"},
		{"traversal-to-api", lookupResourceBase + "/../plugins/" + libraryID + "/api/keys"},
		{"lookup-data-suffix", lookupResourceBase + "/lookup/data/extra"},
		{"lookup-directory", lookupResourceBase + "/lookup"},
		{"nested-traversal", lookupResourceBase + "/vendor/../../app.js"},
		{"unknown-asset", lookupResourceBase + "/does-not-exist"},
		{"other-plugin-id", "/v0/resource/plugins/other-plugin/app.js"},
		{"short-plugin-route-name", "/v0/resource/plugins/" + PluginID + "/app.js"},
		{"empty-relative", lookupResourceBase + "/"},
		{"dot-segment", lookupResourceBase + "/./app.js"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lookupCall(t, h, tc.path, h.plain, nil)
			if got.StatusCode != http.StatusNotFound {
				t.Fatalf("path %s status=%d body=%s", tc.path, got.StatusCode, got.Body)
			}
			if strings.Contains(string(got.Body), `"items"`) {
				t.Fatalf("path %s reached the financial API: %s", tc.path, got.Body)
			}
		})
	}
}

// TestLookupDispatchServesOwnLookupAssets verifies the lookup page shell and its
// own static assets are publicly served from the exact allow list.
func TestLookupDispatchServesOwnLookupAssets(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ name, mime string }{
		{"lookup.html", "text/html"},
		{"lookup.js", "text/javascript"},
		{"lookup.css", "text/css"},
		{"app.js", "text/javascript"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lookupCall(t, h, lookupResourceBase+"/"+tc.name, "", nil)
			if got.StatusCode != http.StatusOK || len(got.Body) == 0 {
				t.Fatalf("asset %s status=%d", tc.name, got.StatusCode)
			}
			if contentType := got.Headers.Get("Content-Type"); !strings.HasPrefix(contentType, tc.mime) {
				t.Fatalf("asset %s content-type = %q", tc.name, contentType)
			}
		})
	}
}

// seedLedgerRequest admits and settles one request for key through the store.
func seedLedgerRequest(t *testing.T, h *harness, key credit.Key, id, trace string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.store().Admit(ctx, credit.AdmitInput{ID: id, TraceID: trace, KeyID: key.ID, RequestedModel: testModel}); err != nil {
		t.Fatalf("Admit(%s): %v", id, err)
	}
	if err := h.store().RecordUsage(ctx, credit.Usage{
		ID: "exec-" + id, RequestID: id, TraceID: trace, KeyID: key.ID,
		Provider: "openai", Model: testModel, Tokens: credit.Tokens{Input: 10, Total: 10}, Quality: credit.QualityComplete,
	}); err != nil {
		t.Fatalf("RecordUsage(%s): %v", id, err)
	}
	if err := h.store().Complete(ctx, credit.Completion{ID: id, TraceID: trace, Outcome: credit.OutcomeSucceeded}); err != nil {
		t.Fatalf("Complete(%s): %v", id, err)
	}
}
