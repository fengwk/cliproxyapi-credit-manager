package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/web"
)

const testModel = "test-model"

// harness is one configured plugin backed by real credit and capture stores in a
// temporary directory.
type harness struct {
	t     *testing.T
	m     *Manager
	key   credit.Key
	plain string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("TZ", "UTC")
	m := NewManager()
	h := &harness{t: t, m: m}
	t.Cleanup(func() {
		_, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil)
	})
	h.configure(captureYAML(t.TempDir(), 16))
	store := h.store()
	ctx := context.Background()
	key, plain, err := store.CreateKey(ctx, credit.KeySpec{Label: "test", Enabled: true})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := store.PutPrice(ctx, credit.Price{Model: testModel, Rates: credit.Rates{Input: 1000000, Output: 2000000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	h.key, h.plain = key, plain
	return h
}

// captureYAML renders a valid plugin configuration for the given data directory.
func captureYAML(dataDir string, maxPending int) []byte {
	return []byte(fmt.Sprintf("data_dir: %q\nhistory:\n  retention_months: 3\nmax_pending_requests: %d\nrequest_capture:\n  enabled: false\n", dataDir, maxPending))
}

// captureEnabledYAML renders a configuration with request capture enabled.
func captureEnabledYAML(dataDir string) []byte {
	return []byte(fmt.Sprintf("data_dir: %q\nhistory:\n  retention_months: 3\nmax_pending_requests: 16\nrequest_capture:\n  enabled: true\n", dataDir))
}

func (h *harness) configure(yaml []byte) {
	h.t.Helper()
	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: yaml, SchemaVersion: pluginabi.SchemaVersion})
	if err != nil {
		h.t.Fatalf("marshal lifecycle request: %v", err)
	}
	raw, err := h.m.HandleCall(pluginabi.MethodPluginRegister, payload)
	if err != nil {
		h.t.Fatalf("register returned error: %v", err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		h.t.Fatalf("decode register envelope: %v", err)
	}
	if !envelope.OK {
		h.t.Fatalf("register failed: %s", raw)
	}
}

// store exposes the live credit backend for test assertions.
func (h *harness) store() credit.Backend {
	h.t.Helper()
	deps, ok := h.m.begin(false)
	if !ok {
		h.t.Fatal("plugin is not configured")
	}
	defer h.m.end()
	return deps.store
}

func (h *harness) call(method string, payload any) pluginabi.Envelope {
	h.t.Helper()
	var raw []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			h.t.Fatalf("marshal %s payload: %v", method, err)
		}
		raw = encoded
	}
	out, err := h.m.HandleCall(method, raw)
	if err != nil {
		h.t.Fatalf("%s returned transport error: %v", method, err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(out, &envelope); err != nil {
		h.t.Fatalf("decode %s envelope %s: %v", method, out, err)
	}
	return envelope
}

func decodeInto[T any](t *testing.T, envelope pluginabi.Envelope) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(envelope.Result, &out); err != nil {
		t.Fatalf("decode result %s: %v", envelope.Result, err)
	}
	return out
}

// intercept sends one before-auth interception and returns the response.
func (h *harness) intercept(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	h.t.Helper()
	envelope := h.call(pluginabi.MethodRequestInterceptBefore, req)
	if !envelope.OK {
		h.t.Fatalf("intercept returned a host error envelope (fail open risk): %s", envelope.Error)
	}
	return decodeInto[pluginapi.RequestInterceptResponse](h.t, envelope)
}

// TestRegistrationDeclaresOnlyPassiveCapabilities is the central safety check:
// the plugin must never advertise an executor, router, scheduler, response or
// stream hook, so the host can never route inference through it.
func TestRegistrationDeclaresOnlyPassiveCapabilities(t *testing.T) {
	m := NewManager()
	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: captureYAML(t.TempDir(), 16), SchemaVersion: pluginabi.SchemaVersion})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := m.HandleCall(pluginabi.MethodPluginRegister, payload)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) }()

	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("register failed: %s", raw)
	}

	var registration map[string]any
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if schema, _ := registration["schema_version"].(float64); uint32(schema) != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %v, want %d", registration["schema_version"], pluginabi.SchemaVersion)
	}
	metadata, ok := registration["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing: %s", envelope.Result)
	}
	for _, field := range []string{"Name", "Version", "Author", "GitHubRepository"} {
		if value, _ := metadata[field].(string); strings.TrimSpace(value) == "" {
			t.Fatalf("metadata.%s must be non-empty (host rejects invalid metadata)", field)
		}
	}
	capabilities, ok := registration["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities missing: %s", envelope.Result)
	}
	allowed := map[string]bool{
		"frontend_auth_provider":           true,
		"frontend_auth_provider_exclusive": true,
		"request_interceptor":              true,
		"request_lifecycle_plugin":         true,
		"usage_plugin":                     true,
		"management_api":                   true,
	}
	for name := range allowed {
		if value, _ := capabilities[name].(bool); !value {
			t.Fatalf("capability %s must be declared true, got %v", name, capabilities[name])
		}
	}
	for _, forbidden := range []string{
		"executor", "model_router", "scheduler", "response_interceptor",
		"response_stream_interceptor", "response_translator", "response_before_translator",
		"response_after_translator", "request_translator", "request_normalizer",
		"websocket_response_observer", "model_registrar", "model_provider", "auth_provider",
		"thinking_applier", "command_line_plugin", "quota_provider",
	} {
		if _, present := capabilities[forbidden]; present {
			t.Fatalf("capability %s must not be declared by a passive credit plugin", forbidden)
		}
	}
}

// TestRegisterRejectsOldHostSchema enforces the minimum supported host contract.
func TestRegisterRejectsOldHostSchema(t *testing.T) {
	m := NewManager()
	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: captureYAML(t.TempDir(), 16), SchemaVersion: 5})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := m.HandleCall(pluginabi.MethodPluginRegister, payload)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "unsupported_host" {
		t.Fatalf("old host schema must be rejected, got %s", raw)
	}
}

// TestAuthCandidatesFollowCPAOrder verifies all five credential locations and
// that the key's stable principal is returned as the authenticated identity.
func TestAuthCandidatesFollowCPAOrder(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name    string
		headers http.Header
		query   map[string]string
	}{
		{name: "authorization-bearer", headers: http.Header{"Authorization": {"Bearer " + h.plain}}},
		{name: "authorization-raw", headers: http.Header{"Authorization": {h.plain}}},
		{name: "x-goog-api-key", headers: http.Header{"X-Goog-Api-Key": {h.plain}}},
		{name: "x-api-key", headers: http.Header{"X-Api-Key": {h.plain}}},
		{name: "query-key", query: map[string]string{"key": h.plain}},
		{name: "query-auth-token", query: map[string]string{"auth_token": h.plain}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := map[string][]string{}
			for key, value := range tc.query {
				query[key] = []string{value}
			}
			envelope := h.call(pluginabi.MethodFrontendAuthAuthenticate, pluginapi.FrontendAuthRequest{
				Method: "POST", Path: "/v1/messages", Headers: tc.headers, Query: query,
			})
			response := decodeInto[pluginapi.FrontendAuthResponse](t, envelope)
			if !response.Authenticated || response.Principal != h.key.Principal {
				t.Fatalf("candidate %s: response = %+v, want principal %s", tc.name, response, h.key.Principal)
			}
			if response.Metadata["key_id"] != h.key.ID {
				t.Fatalf("candidate %s: metadata = %+v, want key_id %s", tc.name, response.Metadata, h.key.ID)
			}
		})
	}
}

// TestAuthRejectsUnknownCredential verifies the exclusive provider never admits
// an unknown credential.
func TestAuthRejectsUnknownCredential(t *testing.T) {
	h := newHarness(t)
	for _, header := range []http.Header{
		{"Authorization": {"Bearer sk-not-a-real-key"}},
		nil,
	} {
		envelope := h.call(pluginabi.MethodFrontendAuthAuthenticate, pluginapi.FrontendAuthRequest{
			Method: "POST", Path: "/v1/messages", Headers: header,
		})
		response := decodeInto[pluginapi.FrontendAuthResponse](t, envelope)
		if response.Authenticated {
			t.Fatalf("unknown credential was authenticated: %+v", response)
		}
	}
}

// TestOwnAccessProviderRequiresExactPluginID verifies a different plugin that
// exposes a same-named provider is never treated as this plugin's traffic.
func TestOwnAccessProviderRequiresExactPluginID(t *testing.T) {
	accepted := []string{
		"plugin:cliproxyapi-credit-manager:" + authProviderID,
		"plugin:credit-manager:" + authProviderID,
	}
	for _, provider := range accepted {
		if !isOwnAccessProvider(provider) {
			t.Fatalf("provider %q must be recognised as this plugin's", provider)
		}
	}
	rejected := []string{
		"config-inline",
		"plugin:other-plugin:" + authProviderID,
		"plugin:cliproxyapi-credit-manager:credit-manager-key",
		"plugin:cliproxyapi-credit-manager",
		"plugin:cliproxyapi-credit-manager:credit-manager:extra",
		"plugin:credit-manager-admin:" + authProviderID,
	}
	for _, provider := range rejected {
		if isOwnAccessProvider(provider) {
			t.Fatalf("provider %q must not be recognised as this plugin's", provider)
		}
	}
}

// TestInterceptTerminatesWithoutIdentity verifies the fail-closed identity gate.
func TestInterceptTerminatesWithoutIdentity(t *testing.T) {
	h := newHarness(t)
	response := h.intercept(pluginapi.RequestInterceptRequest{
		RequestID: "req-1", TraceID: "trace-1", Model: testModel, RequestedModel: testModel,
		Body: []byte(`{"model":"test-model"}`),
	})
	if !response.Terminate || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing identity must terminate with 401, got %+v", response)
	}
}

// TestInterceptRequiresTrace verifies that an execution without a trace is not
// admitted for billing.
func TestInterceptRequiresTrace(t *testing.T) {
	h := newHarness(t)
	response := h.intercept(h.request("req-notrace", ""))
	if !response.Terminate || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing trace must terminate with 400, got %+v", response)
	}
}

// TestInterceptReserveThenSettle exercises the full accounting round trip:
// reservation, usage normalization and terminal completion.
func TestInterceptReserveThenSettle(t *testing.T) {
	h := newHarness(t)
	response := h.intercept(h.request("req-1", "trace-1"))
	if response.Terminate {
		t.Fatalf("admitted request was terminated: %+v", response)
	}
	if len(response.Body) != 0 || len(response.Headers) != 0 || response.Path != "" || len(response.ClearHeaders) != 0 {
		t.Fatalf("before-auth interception must not modify the request: %+v", response)
	}

	h.usage(pluginapi.UsageRecord{
		RequestID: "req-1", TraceID: "trace-1", Provider: "openai", ExecutorType: "openaiExecutor",
		Model: testModel, APIKey: h.key.Principal,
		AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail:         pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-1", "trace-1", pluginapi.RequestCompletionSucceeded)

	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD <= 0 {
		t.Fatalf("spent = %d, want a settled charge", key.SpentMicroUSD)
	}
	if key.HeldMicroUSD != 0 {
		t.Fatalf("held = %d, want the hold released on completion", key.HeldMicroUSD)
	}
}

// TestInterceptCountTokensSkipsHoldButKeepsPermissions verifies that native
// token counting is not billed yet still cannot escape key permissions.
func TestInterceptCountTokensSkipsHoldButKeepsPermissions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	key, _, err := h.store().CreateKey(ctx, credit.KeySpec{
		Label: "restricted", Enabled: true, AllowedModels: []string{"allowed-model"},
	})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := h.store().PutPrice(ctx, credit.Price{Model: "allowed-model", Rates: credit.Rates{Input: 1000000, Output: 1000000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}

	req := pluginapi.RequestInterceptRequest{
		RequestID: "count-1", TraceID: "trace-count", Model: "allowed-model", RequestedModel: "allowed-model",
		Body:     []byte(`{"model":"allowed-model"}`),
		Metadata: map[string]any{"caller_scope": key.Scope, "request_path": "/v1/messages/count_tokens"},
	}
	response := h.intercept(req)
	if response.Terminate {
		t.Fatalf("allowed count_tokens probe was terminated: %+v", response)
	}
	stored, err := h.store().GetKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if stored.HeldMicroUSD != 0 {
		t.Fatalf("count_tokens must not hold credit, held = %d", stored.HeldMicroUSD)
	}

	req.RequestID = "count-2"
	req.TraceID = "trace-count-2"
	req.RequestedModel = "other-model"
	req.Model = "other-model"
	response = h.intercept(req)
	if !response.Terminate || response.StatusCode != http.StatusForbidden {
		t.Fatalf("disallowed count_tokens model must terminate with 403, got %+v", response)
	}
}

// TestInterceptFailsClosedOnPanic verifies that an internal panic in the
// interceptor becomes a fail-closed Terminate response inside a healthy
// envelope, never a host error that would let the request proceed.
func TestInterceptFailsClosedOnPanic(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.live.store = panicBackend{Backend: h.m.live.store}
	h.m.mu.Unlock()

	response := h.intercept(pluginapi.RequestInterceptRequest{
		RequestID: "req-panic", TraceID: "trace-panic", Model: testModel, RequestedModel: testModel,
		Metadata: map[string]any{"caller_scope": h.key.Scope},
	})
	if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("panic must terminate with 503, got %+v", response)
	}
}

// TestAfterAuthInterceptIsNoOp verifies the passive after-auth hook.
func TestAfterAuthInterceptIsNoOp(t *testing.T) {
	h := newHarness(t)
	envelope := h.call(pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: "x"})
	if !envelope.OK {
		t.Fatalf("after-auth interception must succeed: %s", envelope.Error)
	}
	response := decodeInto[pluginapi.RequestInterceptResponse](t, envelope)
	if response.Terminate || len(response.Body) != 0 || len(response.Headers) != 0 {
		t.Fatalf("after-auth interception must be a no-op, got %+v", response)
	}
}

// TestFinanceFaultBlocksAdmission verifies that a financial failure fails the
// plugin closed: subsequent requests are refused, never unmetered.
func TestFinanceFaultBlocksAdmission(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.live.store = faultBackend{Backend: h.m.live.store, reserveErr: errors.New("storage offline")}
	h.m.mu.Unlock()

	response := h.intercept(h.request("req-fault", "trace-fault"))
	if !response.Terminate {
		t.Fatalf("finance fault must terminate the request: %+v", response)
	}
	h.m.mu.Lock()
	blocked := h.m.admissionBlocked
	h.m.mu.Unlock()
	if !blocked {
		t.Fatal("finance fault must block new admissions")
	}
	if h.m.LastError() == "" {
		t.Fatal("finance fault must be observable through health")
	}
}

// TestCompleteCancelledOutcomePreserved verifies the native terminal states are
// not collapsed into one another.
func TestCompleteCancelledOutcomePreserved(t *testing.T) {
	for outcome, want := range map[pluginapi.RequestCompletionOutcome]string{
		pluginapi.RequestCompletionSucceeded: "succeeded",
		pluginapi.RequestCompletionFailed:    "failed",
		pluginapi.RequestCompletionRejected:  "rejected",
		pluginapi.RequestCompletionCanceled:  "canceled",
	} {
		if got := completionOutcome(outcome); got != want {
			t.Fatalf("completionOutcome(%q) = %q, want %q", outcome, got, want)
		}
	}
}

// TestCompleteUnknownRequestIsBenign verifies that a completion for a request
// this plugin never reserved (for example a native token-count probe whose
// history was purged) is ignored without failing the plugin closed.
func TestCompleteUnknownRequestIsBenign(t *testing.T) {
	h := newHarness(t)
	h.complete("never-reserved", "trace-never", pluginapi.RequestCompletionSucceeded)
	h.m.mu.Lock()
	blocked, message := h.m.admissionBlocked, h.m.lastError
	h.m.mu.Unlock()
	if blocked {
		t.Fatal("an unknown completion must not block admissions")
	}
	if message != "" {
		t.Fatalf("an unknown completion must not degrade health, got %q", message)
	}
	// Admission must still work after the benign completion.
	if response := h.intercept(h.request("req-after", "trace-after")); response.Terminate {
		t.Fatalf("admission blocked after a benign completion: %+v", response)
	}
}

// TestUsageIgnoresForeignAndInternalRecords verifies usage attribution rules.
func TestUsageIgnoresForeignAndInternalRecords(t *testing.T) {
	h := newHarness(t)
	if response := h.intercept(h.request("req-usage", "trace-usage")); response.Terminate {
		t.Fatalf("reserve failed: %+v", response)
	}
	// A foreign access provider must be ignored silently.
	h.usage(pluginapi.UsageRecord{
		RequestID: "req-usage", TraceID: "trace-usage", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, AccessProvider: "config-inline",
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	// An internal host source must be ignored and reported.
	h.usage(pluginapi.UsageRecord{
		RequestID: "req-usage", TraceID: "trace-usage", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, Source: "plugin_host_model_callback",
		AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail:         pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD != 0 {
		t.Fatalf("foreign and internal records must not be billed, spent = %d", key.SpentMicroUSD)
	}
}

// TestReconfigureRefusesWhileRequestsActive verifies that a store reporting
// unfinished requests is retained instead of being swapped underneath them.
func TestReconfigureRefusesWhileRequestsActive(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.live.store = activeBackend{Backend: h.m.live.store, active: true}
	h.m.mu.Unlock()

	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: captureYAML(t.TempDir(), 32), SchemaVersion: pluginabi.SchemaVersion})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := h.m.HandleCall(pluginabi.MethodPluginReconfigure, payload)
	if err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != http.StatusConflict {
		t.Fatalf("active-request reconfigure must be refused with 409, got %s", raw)
	}
	// The previous store must still be live and serving.
	if response := h.intercept(h.request("req-keep", "trace-keep")); response.Terminate {
		t.Fatalf("previous store must stay live after a refused reconfigure: %+v", response)
	}
}

// TestQuiesceThenReconfigureReopens verifies quiesce refuses new accounting and
// that a later reconfigure restores service.
func TestQuiesceThenReconfigureReopens(t *testing.T) {
	h := newHarness(t)
	if _, err := h.m.HandleCall(pluginabi.MethodPluginQuiesce, nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	response := h.intercept(h.request("req-quiesced", "trace-quiesced"))
	if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("quiesced plugin must refuse admission, got %+v", response)
	}
	h.configure(captureYAML(t.TempDir(), 16))
	store := h.store()
	ctx := context.Background()
	key, _, err := store.CreateKey(ctx, credit.KeySpec{Label: "again", Enabled: true})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := store.PutPrice(ctx, credit.Price{Model: testModel, Rates: credit.Rates{Input: 1000000, Output: 1000000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	h.key = key
	if response := h.intercept(h.request("req-reopened", "trace-reopened")); response.Terminate {
		t.Fatalf("reconfigured plugin must admit requests again: %+v", response)
	}
}

// TestShutdownIsIdempotentAndTerminal verifies shutdown drains, closes and then
// refuses everything without failing.
func TestShutdownIsIdempotentAndTerminal(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 2; i++ {
		if _, err := h.m.HandleCall(pluginabi.MethodPluginShutdown, nil); err != nil {
			t.Fatalf("shutdown call %d: %v", i, err)
		}
	}
	response := h.intercept(h.request("req-closed", "trace-closed"))
	if !response.Terminate {
		t.Fatalf("shut-down plugin must refuse admission: %+v", response)
	}
	if envelope := h.call(pluginabi.MethodUsageHandle, pluginapi.UsageRecord{RequestID: "x"}); !envelope.OK {
		t.Fatalf("usage after shutdown must be acknowledged: %s", envelope.Error)
	}
}

// TestManagementRoutesAndResourceWhitelist verifies the management contract and
// that the public resource base exposes only static assets.
func TestManagementRoutesAndResourceWhitelist(t *testing.T) {
	h := newHarness(t)
	envelope := h.call(pluginabi.MethodManagementRegister, pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: "/v0/resource/plugins/credit-manager",
	})
	registration := decodeInto[managementRegistrationResponse](t, envelope)
	found := map[string]bool{}
	for _, route := range registration.Routes {
		found[route.Method+" "+route.Path] = true
		if !strings.HasPrefix(route.Path, web.BasePath+"/api/") {
			t.Fatalf("management route %s escapes the authenticated base %s", route.Path, web.BasePath)
		}
	}
	for _, want := range []string{
		"GET " + web.BasePath + "/api/keys",
		"POST " + web.BasePath + "/api/keys",
		"PUT " + web.BasePath + "/api/keys",
		"POST " + web.BasePath + "/api/rotate",
		"GET " + web.BasePath + "/api/prices",
		"PUT " + web.BasePath + "/api/prices",
		"GET " + web.BasePath + "/api/usage",
		"GET " + web.BasePath + "/api/capture",
		"POST " + web.BasePath + "/api/resolve",
		"GET " + web.BasePath + "/api/health",
	} {
		if !found[want] {
			t.Fatalf("management route %s is not registered", want)
		}
	}
	if len(registration.Resources) != len(resourceAssets) {
		t.Fatalf("resources = %d, want %d", len(registration.Resources), len(resourceAssets))
	}
	for _, resource := range registration.Resources {
		if !strings.HasPrefix(resource.Path, "/v0/resource/plugins/credit-manager/") {
			t.Fatalf("resource %s escapes the public base", resource.Path)
		}
	}

	// Static assets are served without management authorization.
	shell := h.call(pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: "GET", Path: "/v0/resource/plugins/credit-manager/index.html",
	})
	response := decodeInto[pluginapi.ManagementResponse](t, shell)
	if response.StatusCode != http.StatusOK || len(response.Body) == 0 {
		t.Fatalf("shell asset not served: %+v", response)
	}
	if contentType := response.Headers.Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("shell content type = %q", contentType)
	}

	// Financial APIs stay behind the management boundary: the authenticated
	// handler rejects a request without a management bearer header.
	api := h.call(pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: "GET", Path: web.BasePath + "/api/keys",
	})
	apiResponse := decodeInto[pluginapi.ManagementResponse](t, api)
	if apiResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated management call must be 401, got %d", apiResponse.StatusCode)
	}

	// Unknown resource paths never resolve.
	missing := h.call(pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: "GET", Path: "/v0/resource/plugins/credit-manager/secret.txt",
	})
	missingResponse := decodeInto[pluginapi.ManagementResponse](t, missing)
	if missingResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown resource must be 404, got %d", missingResponse.StatusCode)
	}
}

// TestQuiesceKeepsSettlingReservedRequests verifies that quiesce never loses
// usage: a request reserved before quiesce can still be settled afterwards.
func TestQuiesceKeepsSettlingReservedRequests(t *testing.T) {
	h := newHarness(t)
	if response := h.intercept(h.request("req-late", "trace-late")); response.Terminate {
		t.Fatalf("reserve failed: %+v", response)
	}
	if _, err := h.m.HandleCall(pluginabi.MethodPluginQuiesce, nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	// New admissions are refused while quiesced.
	if response := h.intercept(h.request("req-refused", "trace-refused")); !response.Terminate {
		t.Fatalf("quiesced plugin must refuse new admissions: %+v", response)
	}
	// The reserved request must still settle.
	h.usage(pluginapi.UsageRecord{
		RequestID: "req-late", TraceID: "trace-late", Provider: "openai", ExecutorType: "openaiExecutor",
		Model: testModel, APIKey: h.key.Principal,
		AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail:         pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-late", "trace-late", pluginapi.RequestCompletionSucceeded)
	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD <= 0 || key.HeldMicroUSD != 0 {
		t.Fatalf("reserved request lost on quiesce: spent=%d held=%d", key.SpentMicroUSD, key.HeldMicroUSD)
	}
}

// TestCaptureFailureDegradesWithoutBlockingInference verifies that a capture
// storage failure never breaks the credit store or request admission, and stays
// observable through health.
func TestCaptureFailureDegradesWithoutBlockingInference(t *testing.T) {
	dir := t.TempDir()
	// A directory where the capture database file belongs makes capture.Open
	// fail while the credit store opens normally.
	if err := os.MkdirAll(filepath.Join(dir, "request-captures.db"), 0o700); err != nil {
		t.Fatalf("prepare capture failure: %v", err)
	}
	m := NewManager()
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: captureEnabledYAML(dir), SchemaVersion: pluginabi.SchemaVersion})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := m.HandleCall(pluginabi.MethodPluginRegister, payload)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("capture failure must not fail registration: %s", raw)
	}
	if m.LastError() == "" {
		t.Fatal("capture degradation must be observable through health")
	}

	deps, ok := m.begin(false)
	if !ok {
		t.Fatal("plugin must stay configured after a capture failure")
	}
	defer m.end()
	key, _, err := deps.store.CreateKey(context.Background(), credit.KeySpec{Label: "degraded", Enabled: true})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := deps.store.PutPrice(context.Background(), credit.Price{Model: testModel, Rates: credit.Rates{Input: 1000000, Output: 1000000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	response := m.interceptBefore(pluginapi.RequestInterceptRequest{
		RequestID: "req-degraded", TraceID: "trace-degraded", Model: testModel, RequestedModel: testModel,
		Body:     []byte(`{"model":"test-model"}`),
		Metadata: map[string]any{"caller_scope": key.Scope},
	})
	if response.Terminate {
		t.Fatalf("capture failure must not block admission: %+v", response)
	}
}

// TestReconfigureAllowsWhenNothingActive verifies the HasActive capability
// permits a swap once the backend reports no unfinished requests.
func TestReconfigureAllowsWhenNothingActive(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.live.store = activeBackend{Backend: h.m.live.store, active: false}
	h.m.mu.Unlock()
	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: captureYAML(t.TempDir(), 32), SchemaVersion: pluginabi.SchemaVersion})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := h.m.HandleCall(pluginabi.MethodPluginReconfigure, payload)
	if err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("idle reconfigure must succeed: %s", raw)
	}
}

// TestManagerConstructorNeedsNoIO is a static guard that the ABI glue never
// calls back into the host: the manager must be constructible with no arguments.
func TestManagerConstructorNeedsNoIO(t *testing.T) {
	m := NewManager()
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if m.LastError() != "" {
		t.Fatalf("a fresh manager must be healthy, got %q", m.LastError())
	}
}

// TestAuthNeverLeaksKeyMaterial verifies the one-time plaintext key never
// appears in an authentication response or in a stored/returned key object.
func TestAuthNeverLeaksKeyMaterial(t *testing.T) {
	h := newHarness(t)
	envelope := h.call(pluginabi.MethodFrontendAuthAuthenticate, pluginapi.FrontendAuthRequest{
		Method: "POST", Path: "/v1/messages", Headers: http.Header{"Authorization": {"Bearer " + h.plain}},
	})
	if strings.Contains(string(envelope.Result), h.plain) {
		t.Fatalf("auth response leaked key material: %s", envelope.Result)
	}
	keys, err := h.store().ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	if strings.Contains(string(encoded), h.plain) {
		t.Fatalf("stored keys leaked material: %s", encoded)
	}
}

// TestConcurrentUsageAndCompletionSettleExactlyOnce drives usage and completion
// concurrently to prove idempotent, exactly-once settlement.
func TestConcurrentUsageAndCompletionSettleExactlyOnce(t *testing.T) {
	h := newHarness(t)
	if response := h.intercept(h.request("req-race", "trace-race")); response.Terminate {
		t.Fatalf("reserve failed: %+v", response)
	}
	record := pluginapi.UsageRecord{
		RequestID: "req-race", TraceID: "trace-race", Provider: "openai", ExecutorType: "openaiExecutor",
		Model: testModel, APIKey: h.key.Principal,
		AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail:         pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	}
	usagePayload, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	completionPayload, err := json.Marshal(pluginapi.RequestCompletion{
		RequestID: "req-race", TraceID: "trace-race",
		Outcome: pluginapi.RequestCompletionSucceeded, StatusCode: http.StatusOK,
	})
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}

	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := h.m.HandleCall(pluginabi.MethodUsageHandle, usagePayload); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := h.m.HandleCall(pluginabi.MethodRequestComplete, completionPayload); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent settlement error: %v", err)
	}

	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.HeldMicroUSD != 0 {
		t.Fatalf("held = %d, want 0 after completion", key.HeldMicroUSD)
	}
	if key.SpentMicroUSD != 2000 {
		t.Fatalf("spent = %d, want exactly one settlement of 2000 microUSD", key.SpentMicroUSD)
	}
}

// request builds one interception request bound to the harness key scope.
func (h *harness) request(id, trace string) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		RequestID: id, TraceID: trace, Model: testModel, RequestedModel: testModel,
		Body:     []byte(`{"model":"test-model","max_tokens":128}`),
		Metadata: map[string]any{"caller_scope": h.key.Scope, "request_path": "/v1/messages"},
	}
}

func (h *harness) usage(record pluginapi.UsageRecord) {
	h.t.Helper()
	if envelope := h.call(pluginabi.MethodUsageHandle, record); !envelope.OK {
		h.t.Fatalf("usage handle failed: %s", envelope.Error)
	}
}

func (h *harness) complete(id, trace string, outcome pluginapi.RequestCompletionOutcome) {
	h.t.Helper()
	if envelope := h.call(pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{
		RequestID: id, TraceID: trace, Outcome: outcome, StatusCode: http.StatusOK,
	}); !envelope.OK {
		h.t.Fatalf("request complete failed: %s", envelope.Error)
	}
}

// panicBackend simulates an internal panic to prove fail-closed behavior.
type panicBackend struct{ credit.Backend }

func (panicBackend) KeyByScope(context.Context, string) (credit.Key, error) {
	panic("injected panic")
}

// faultBackend injects a reservation failure to prove fail-closed behavior.
type faultBackend struct {
	credit.Backend
	reserveErr error
}

func (f faultBackend) Reserve(context.Context, credit.ReserveInput) (credit.Request, error) {
	return credit.Request{}, f.reserveErr
}

// activeBackend advertises the optional HasActive capability.
type activeBackend struct {
	credit.Backend
	active bool
	err    error
}

func (a activeBackend) HasActive(context.Context) (bool, error) { return a.active, a.err }
