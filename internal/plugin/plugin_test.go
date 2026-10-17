package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/web"
)

const testModel = "test-model"

// testResourceBase is the canonical public resource base for this plugin's
// released library id, which is the only resource directory the plugin serves.
const testResourceBase = "/v0/resource/plugins/" + libraryID

// harness is one configured plugin backed by real credit and capture stores in a
// temporary directory.
type harness struct {
	t     *testing.T
	m     *Manager
	key   credit.Key
	plain string
	dir   string
	// reconfigures counts the configurations this harness has sent, so every
	// reconfigure really differs from the live one.
	reconfigures int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("TZ", "UTC")
	m := NewManager()
	dir := t.TempDir()
	h := &harness{t: t, m: m, dir: dir}
	t.Cleanup(func() {
		_, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil)
	})
	h.configure(h.yaml())
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
func captureYAML(dataDir string) []byte {
	return captureYAMLMonths(dataDir, 3)
}

// captureYAMLMonths renders a valid plugin configuration with an explicit
// retention window, which is a safe way to make two configurations differ
// without touching the immutable accounting directory.
func captureYAMLMonths(dataDir string, months int) []byte {
	return []byte(fmt.Sprintf("data_dir: %q\nhistory:\n  retention_months: %d\nrequest_capture:\n  enabled: false\n", dataDir, months))
}

// captureEnabledYAML renders a configuration with request capture enabled.
func captureEnabledYAML(dataDir string) []byte {
	return []byte(fmt.Sprintf("data_dir: %q\nhistory:\n  retention_months: 3\nrequest_capture:\n  enabled: true\n", dataDir))
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

// yaml renders a plugin configuration for the harness data directory. The
// accounting directory is immutable while the plugin runs, so every
// reconfigure in these tests reuses it.
func (h *harness) yaml() []byte {
	return captureYAML(h.dir)
}

// reconfigure applies a new configuration and returns the raw envelope. The
// retention window changes on every call so the host really reconfigures rather
// than re-sending the identical configuration (the idempotent same-config
// path), while data_dir stays immutable.
func (h *harness) reconfigure() pluginabi.Envelope {
	h.t.Helper()
	h.reconfigures++
	payload, err := json.Marshal(lifecycleRequest{
		ConfigYAML:    captureYAMLMonths(h.dir, 3+h.reconfigures),
		SchemaVersion: pluginabi.SchemaVersion,
	})
	if err != nil {
		h.t.Fatalf("marshal lifecycle request: %v", err)
	}
	out, err := h.m.HandleCall(pluginabi.MethodPluginReconfigure, payload)
	if err != nil {
		h.t.Fatalf("reconfigure returned error: %v", err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(out, &envelope); err != nil {
		h.t.Fatalf("decode reconfigure envelope: %v", err)
	}
	return envelope
}

// waitForState blocks until the manager reaches the given lifecycle state. It
// spins on an observable condition rather than sleeping so tests synchronize on
// a real barrier instead of on timing.
func (h *harness) waitForState(state lifecycleState) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.m.mu.Lock()
		got := h.m.state
		h.m.mu.Unlock()
		if got == state {
			return
		}
		runtime.Gosched()
	}
	h.t.Fatalf("manager never reached lifecycle state %d", state)
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
	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: captureYAML(t.TempDir()), SchemaVersion: pluginabi.SchemaVersion})
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

// TestRegisterRejectsOldHostSchema enforces the minimum supported host contract:
// a missing, zero or older RPC schema is refused, no ledger is created, and the
// refusal leaves a live manager and its data directory untouched.
func TestRegisterRejectsOldHostSchema(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  map[string]any
		wantCode string
	}{
		{name: "missing", payload: map[string]any{}, wantCode: "unsupported_host"},
		{name: "zero", payload: map[string]any{"schema_version": 0}, wantCode: "unsupported_host"},
		{name: "five", payload: map[string]any{"schema_version": 5}, wantCode: "unsupported_host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dataDir := filepath.Join(dir, "data")
			// config_yaml is a []byte on the wire, so the payload is built as raw
			// JSON with the base64 spelling the host uses, which also lets the
			// "missing" case omit schema_version entirely.
			payload := map[string]any{
				"config_yaml": base64.StdEncoding.EncodeToString(captureYAML(dataDir)),
			}
			for key, value := range tc.payload {
				payload[key] = value
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			m := NewManager()
			t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
			out, err := m.HandleCall(pluginabi.MethodPluginRegister, raw)
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			var envelope pluginabi.Envelope
			if err := json.Unmarshal(out, &envelope); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != tc.wantCode {
				t.Fatalf("unsupported host schema must be rejected, got %s", out)
			}
			// A refused registration must not create the accounting store.
			if _, statErr := os.Stat(dataDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("refused registration created the data directory %s (err=%v)", dataDir, statErr)
			}
			if live, ok := m.begin(false); ok {
				m.end()
				t.Fatalf("refused registration left a live store: %+v", live)
			}
		})
	}
}

// TestUnsupportedHostSchemaLeavesLiveManagerServing verifies that refusing a
// host whose RPC schema predates this plugin's contract neither quiesces the
// live manager nor disturbs its ledger: the existing configuration keeps serving
// and its accounting rows survive verbatim.
func TestUnsupportedHostSchemaLeavesLiveManagerServing(t *testing.T) {
	h := newHarness(t)
	store := h.store()
	ctx := context.Background()
	before, err := store.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}

	out, err := h.m.HandleCall(pluginabi.MethodPluginRegister, []byte(`{"schema_version":5}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "unsupported_host" {
		t.Fatalf("unsupported host schema must be rejected, got %s", out)
	}

	if response := h.intercept(h.request("after-schema-reject", "trace-after-schema-reject")); response.Terminate {
		t.Fatalf("a refused host schema must not stop the live manager: %+v", response)
	}
	after, err := h.store().ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys after rejection: %v", err)
	}
	if h.store() != store || !reflect.DeepEqual(after, before) {
		t.Fatalf("refused host schema changed the ledger: before=%v after=%v", before, after)
	}
}

// TestRegisterAcceptsCurrentHostSchemaAndReportsExactSchema pins the other half
// of the contract: a current or newer host is served, and the negotiated schema
// is exactly this plugin's implemented contract instead of the newer host's
// unverified version.
func TestRegisterAcceptsCurrentHostSchemaAndReportsExactSchema(t *testing.T) {
	for _, hostSchema := range []uint32{pluginabi.SchemaVersion, pluginabi.SchemaVersion + 1} {
		t.Run(fmt.Sprintf("host-%d", hostSchema), func(t *testing.T) {
			m := NewManager()
			t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
			payload, err := json.Marshal(lifecycleRequest{ConfigYAML: captureYAML(t.TempDir()), SchemaVersion: hostSchema})
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
			if !envelope.OK {
				t.Fatalf("register failed: %s", raw)
			}
			var registration struct {
				SchemaVersion uint32 `json:"schema_version"`
			}
			if err := json.Unmarshal(envelope.Result, &registration); err != nil {
				t.Fatalf("decode registration: %v", err)
			}
			if registration.SchemaVersion != pluginabi.SchemaVersion {
				t.Fatalf("negotiated schema = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
			}
		})
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

// TestOwnAccessProviderRequiresExactPluginID verifies only this plugin's own
// released library id and provider id are accepted, so a different plugin that
// exposes a same-named provider -- including the library loaded under the short
// plugin route name -- is never treated as this plugin's traffic.
func TestOwnAccessProviderRequiresExactPluginID(t *testing.T) {
	accepted := []string{
		"plugin:" + libraryID + ":" + authProviderID,
	}
	for _, provider := range accepted {
		if !isOwnAccessProvider(provider) {
			t.Fatalf("provider %q must be recognised as this plugin's", provider)
		}
	}
	rejected := []string{
		"config-inline",
		"plugin:other-plugin:" + authProviderID,
		// The short management route segment is not a library id.
		"plugin:" + PluginID + ":" + authProviderID,
		"plugin:" + libraryID + ":credit-manager-key",
		"plugin:" + libraryID,
		"plugin:" + libraryID + ":" + authProviderID + ":extra",
		"plugin:credit-manager-admin:" + authProviderID,
	}
	for _, provider := range rejected {
		if isOwnAccessProvider(provider) {
			t.Fatalf("provider %q must not be recognised as this plugin's", provider)
		}
	}
}

// TestInterceptRequiresRequestedModel verifies the current-CPA input contract:
// the requested model is the only id this plugin prices and freezes, so a
// request that omits it is refused -- even when the host resolved a concrete
// upstream model -- without creating any accounting row and without guessing an
// id from the resolved model.
func TestInterceptRequiresRequestedModel(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name    string
		request pluginapi.RequestInterceptRequest
	}{
		{name: "missing", request: pluginapi.RequestInterceptRequest{
			RequestID: "req-nomodel", TraceID: "trace-nomodel",
			Model: testModel, Body: []byte(`{"model":"test-model"}`),
		}},
		{name: "blank", request: pluginapi.RequestInterceptRequest{
			RequestID: "req-blankmodel", TraceID: "trace-blankmodel",
			Model: testModel, RequestedModel: "   ",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.request.Metadata = map[string]any{"caller_scope": h.key.Scope}
			response := h.intercept(tc.request)
			if !response.Terminate || response.StatusCode != http.StatusBadRequest {
				t.Fatalf("a request without a requested model must terminate with 400, got %+v", response)
			}
			// No request may have been admitted for billing.
			report, err := h.store().Report(context.Background(), credit.Filter{KeyID: h.key.ID})
			if err != nil {
				t.Fatalf("Report: %v", err)
			}
			if report.Total != 0 || len(report.Items) != 0 {
				t.Fatalf("a refused request created accounting rows: %+v", report.Items)
			}
		})
	}
}

// TestInterceptBillsAliasRequestByRequestedModel verifies that an alias request
// is priced and frozen by the client-requested id only: the alias, not the
// resolved upstream model the host reports alongside it.
func TestInterceptBillsAliasRequestByRequestedModel(t *testing.T) {
	h := newHarness(t)
	store := h.store()
	ctx := context.Background()
	if err := store.PutPrice(ctx, credit.Price{Model: "alias-model", Rates: credit.Rates{Input: 1_000_000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	req := h.request("req-alias", "trace-alias")
	// The host resolves the alias to a different upstream id; billing must stay
	// on the requested alias.
	req.RequestedModel, req.Model = "alias-model", "resolved-upstream-model"
	if response := h.intercept(req); response.Terminate {
		t.Fatalf("alias admission failed: %+v", response)
	}
	h.usage(ownUsage("exec-alias", "trace-alias", h.key.Principal, pluginapi.UsageDetail{InputTokens: 1, TotalTokens: 1}))
	h.complete("req-alias", "trace-alias", pluginapi.RequestCompletionSucceeded)
	key, err := store.GetKey(ctx, h.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if key.SpentMicroUSD != 1 {
		t.Fatalf("alias spend = %d, want the alias price 1 (never the resolved id)", key.SpentMicroUSD)
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

// TestInterceptAdmitThenSettle exercises the full accounting round trip:
// admission, usage normalization and terminal completion. Admission itself
// charges nothing: the charge is booked from the reported actual usage.
func TestInterceptAdmitThenSettle(t *testing.T) {
	h := newHarness(t)
	response := h.intercept(h.request("req-1", "trace-1"))
	if response.Terminate {
		t.Fatalf("admitted request was terminated: %+v", response)
	}
	if len(response.Body) != 0 || len(response.Headers) != 0 || response.Path != "" || len(response.ClearHeaders) != 0 {
		t.Fatalf("before-auth interception must not modify the request: %+v", response)
	}
	admitted, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if admitted.SpentMicroUSD != 0 {
		t.Fatalf("spent = %d, want no charge before usage is reported", admitted.SpentMicroUSD)
	}

	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-1", TraceID: "trace-1", Provider: "openai", ExecutorType: "openaiExecutor",
		Model: testModel, APIKey: h.key.Principal,
		AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail:         pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-1", "trace-1", pluginapi.RequestCompletionSucceeded)

	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD != 2000 {
		t.Fatalf("spent = %d, want the settled actual usage charge 2000", key.SpentMicroUSD)
	}
}

// TestInterceptCountTokensSkipsBillingButKeepsPermissions verifies that native
// token counting is never billed yet still cannot escape key permissions.
func TestInterceptCountTokensSkipsBillingButKeepsPermissions(t *testing.T) {
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
	if stored.SpentMicroUSD != 0 {
		t.Fatalf("count_tokens must never be billed, spent = %d", stored.SpentMicroUSD)
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

// TestAdmissionStorageFaultIsRequestScoped verifies that a storage failure
// refuses the request that hit it (fail closed) and is observable through
// health, without latching a global gate: once storage answers again the very
// next admission succeeds. One transient failure must never permanently block
// every key.
func TestAdmissionStorageFaultIsRequestScoped(t *testing.T) {
	h := newHarness(t)
	real := h.store()
	h.m.mu.Lock()
	h.m.live.store = &oneShotAdmitFault{Backend: real, err: errors.New("storage offline")}
	h.m.mu.Unlock()

	response := h.intercept(h.request("req-fault", "trace-fault"))
	if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an admission storage fault must terminate the request: %+v", response)
	}
	if h.m.LastError() == "" {
		t.Fatal("an admission storage fault must be observable through health")
	}
	response = h.intercept(h.request("req-after-fault", "trace-after-fault"))
	if response.Terminate {
		t.Fatalf("a single recoverable storage fault must not block later admissions: %+v", response)
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
	if message := h.m.LastError(); message != "" {
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
		RequestID: "exec-usage-foreign", TraceID: "trace-usage", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, AccessProvider: "config-inline",
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	// A real background execution (an internal host path such as a warmup probe)
	// carries no access provider of this plugin's own, so it is ignored. Its
	// Source is only an upstream account label and never a billing gate.
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-usage-background", TraceID: "trace-usage", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, Source: "warmup",
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD != 0 {
		t.Fatalf("foreign and background records must not be billed, spent = %d", key.SpentMicroUSD)
	}
}

// TestReconfigureRefusesWhileRequestsActive verifies that a store reporting
// unfinished requests is retained instead of being swapped underneath them.
func TestReconfigureRefusesWhileRequestsActive(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.live.store = activeBackend{Backend: h.m.live.store, active: true}
	h.m.mu.Unlock()

	envelope := h.reconfigure()
	if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != http.StatusConflict {
		t.Fatalf("active-request reconfigure must be refused with 409, got %+v", envelope)
	}
	// The previous store must still be live and serving.
	if response := h.intercept(h.request("req-keep", "trace-keep")); response.Terminate {
		t.Fatalf("previous store must stay live after a refused reconfigure: %+v", response)
	}
}

// TestReconfigureRefusesWhenLivenessIsUnknown verifies that a backend which can
// not answer the liveness question is treated as unsafe, so accounting is never
// swapped on a guess.
func TestReconfigureRefusesWhenLivenessIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.live.store = activeBackend{Backend: h.m.live.store, err: errors.New("liveness probe failed")}
	h.m.mu.Unlock()

	envelope := h.reconfigure()
	if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != http.StatusConflict {
		t.Fatalf("an unknown liveness answer must refuse the swap, got %+v", envelope)
	}
	if h.m.LastError() == "" {
		t.Fatal("the liveness failure must be observable through health")
	}
	if response := h.intercept(h.request("req-unknown", "trace-unknown")); response.Terminate {
		t.Fatalf("the previous store must keep serving: %+v", response)
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
	h.configure(h.yaml())
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
		BasePath: "/v0/management", ResourceBasePath: testResourceBase,
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
	// The registered resources are the static asset allow list plus the public
	// customer self-service endpoints, and nothing else.
	if want := len(resourceAssets) + len(lookupRoutes); len(registration.Resources) != want {
		t.Fatalf("resources = %d, want %d", len(registration.Resources), want)
	}
	for _, resource := range registration.Resources {
		if !strings.HasPrefix(resource.Path, testResourceBase+"/") {
			t.Fatalf("resource %s escapes the public base", resource.Path)
		}
	}

	// Static assets are served without management authorization.
	shell := h.call(pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: "GET", Path: testResourceBase + "/index.html",
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
		Method: "GET", Path: testResourceBase + "/secret.txt",
	})
	missingResponse := decodeInto[pluginapi.ManagementResponse](t, missing)
	if missingResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown resource must be 404, got %d", missingResponse.StatusCode)
	}

	// The short management route segment is not a library id: it never names
	// this plugin's resource directory, so no asset is served under it.
	shortID := pluginapi.ManagementRequest{
		Method: "GET", Path: "/v0/resource/plugins/" + PluginID + "/index.html",
	}
	shortResponse := decodeInto[pluginapi.ManagementResponse](
		t, h.call(pluginabi.MethodManagementHandle, shortID))
	if shortResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("the short plugin route name must not serve resources, got %d: %s",
			shortResponse.StatusCode, shortResponse.Body)
	}
}

// Rejecting a renamed library must also stop admissions: rejecting only its
// usage notifications would allow inference without charging those requests.
func TestRenamedLibraryStopsAdmission(t *testing.T) {
	for _, id := range []string{PluginID, "other-plugin"} {
		t.Run(id, func(t *testing.T) {
			h := newHarness(t)
			envelope := h.call(pluginabi.MethodManagementRegister, pluginapi.ManagementRegistrationRequest{
				ResourceBasePath: "/v0/resource/plugins/" + id,
			})
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != "unsupported_plugin_id" {
				t.Fatalf("renamed library was registered: %+v", envelope)
			}
			response := h.intercept(h.request("refused-library", "trace-refused-library"))
			if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("renamed library admitted an unbillable request: %+v", response)
			}
		})
	}
}

// TestQuiesceKeepsSettlingAdmittedRequests verifies that quiesce never loses
// usage: a request admitted before quiesce can still be settled afterwards.
func TestQuiesceKeepsSettlingAdmittedRequests(t *testing.T) {
	h := newHarness(t)
	if response := h.intercept(h.request("req-late", "trace-late")); response.Terminate {
		t.Fatalf("admission failed: %+v", response)
	}
	if _, err := h.m.HandleCall(pluginabi.MethodPluginQuiesce, nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	// New admissions are refused while quiesced.
	if response := h.intercept(h.request("req-refused", "trace-refused")); !response.Terminate {
		t.Fatalf("quiesced plugin must refuse new admissions: %+v", response)
	}
	// The admitted request must still settle in full.
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-late", TraceID: "trace-late", Provider: "openai", ExecutorType: "openaiExecutor",
		Model: testModel, APIKey: h.key.Principal,
		AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail:         pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-late", "trace-late", pluginapi.RequestCompletionSucceeded)
	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD != 2000 {
		t.Fatalf("admitted request lost on quiesce: spent=%d want 2000", key.SpentMicroUSD)
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
	if envelope := h.reconfigure(); !envelope.OK {
		t.Fatalf("idle reconfigure must succeed: %+v", envelope)
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
		RequestID: "exec-race", TraceID: "trace-race", Provider: "openai", ExecutorType: "openaiExecutor",
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

// TestUsageSettlesAcrossDistinctLifecycleAndExecutionIDs reproduces a real host
// delivery: the usage event carries a per-execution id and the parent trace,
// never the lifecycle id that reserved the money.
func TestUsageSettlesAcrossDistinctLifecycleAndExecutionIDs(t *testing.T) {
	h := newHarness(t)
	if response := h.intercept(h.request("req-lifecycle", "trace-lifecycle")); response.Terminate {
		t.Fatalf("reserve failed: %+v", response)
	}
	h.usage(pluginapi.UsageRecord{
		RequestID: "9d1c0b7e-execution", TraceID: "trace-lifecycle", Provider: "openai", ExecutorType: "openaiExecutor",
		Model: testModel, APIKey: h.key.Principal,
		AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail:         pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	if message := h.m.LastError(); message != "" {
		t.Fatalf("an execution id distinct from the lifecycle id must attribute cleanly, health=%q", message)
	}
	h.complete("req-lifecycle", "trace-lifecycle", pluginapi.RequestCompletionSucceeded)
	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD != 2000 {
		t.Fatalf("spent = %d, want the execution charge 2000", key.SpentMicroUSD)
	}
}

// TestUsageConflictIsHealthOnlyAndKeepsServing verifies that a conflicting
// usage record is reported for review instead of silently freeing the request
// or failing every other key closed.
func TestUsageConflictIsHealthOnlyAndKeepsServing(t *testing.T) {
	h := newHarness(t)
	if response := h.intercept(h.request("req-conflict", "trace-conflict")); response.Terminate {
		t.Fatalf("reserve failed: %+v", response)
	}
	record := pluginapi.UsageRecord{
		RequestID: "exec-conflict", TraceID: "trace-conflict", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail: pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	}
	h.usage(record)
	// The same execution id with different accounting is a conflict, not a replay.
	record.Detail = pluginapi.UsageDetail{InputTokens: 9999, OutputTokens: 500, TotalTokens: 10499}
	h.usage(record)

	message := h.m.LastError()
	if message == "" || !strings.Contains(message, h.key.ID) {
		t.Fatalf("a conflicting usage record must be reported with the affected key, got %q", message)
	}
	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD == 0 {
		t.Fatal("a conflicting usage must not write off the already booked charge")
	}
	if response := h.intercept(h.request("req-after-conflict", "trace-after-conflict")); response.Terminate {
		t.Fatalf("other requests must keep being admitted: %+v", response)
	}
}

// TestUsageStorageFaultIsHealthOnly verifies that a genuine accounting write
// failure is reported through health and does not poison future admissions: the
// next request validates its own storage state instead of being refused by a
// sticky global gate.
func TestUsageStorageFaultIsHealthOnly(t *testing.T) {
	h := newHarness(t)
	real := h.store()
	h.m.mu.Lock()
	h.m.live.store = faultBackend{Backend: real, usageErr: errors.New("database or disk is full")}
	h.m.mu.Unlock()

	if response := h.intercept(h.request("req-storage", "trace-storage")); response.Terminate {
		t.Fatalf("admission failed: %+v", response)
	}
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-storage", TraceID: "trace-storage", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})

	if message := h.m.LastError(); message == "" {
		t.Fatal("a usage storage fault must be observable through health")
	}
	response := h.intercept(h.request("req-after-storage", "trace-after-storage"))
	if response.Terminate {
		t.Fatalf("a usage storage fault must not block later admissions: %+v", response)
	}
}

// TestQuotaRejectionDoesNotDisableOtherKeys pins soft post-usage quota
// admission: a key with a positive remaining quota admits a request even with a
// tiny configured limit (quota=1, spent=0), the reported actual usage is charged
// in full beyond that limit, only then does the exhausted key reject the next
// request, and no other key or the health state is affected.
func TestQuotaRejectionDoesNotDisableOtherKeys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	limited, _, err := h.store().CreateKey(ctx, credit.KeySpec{
		Label: "limited", Enabled: true, QuotaMicroUSD: 1,
	})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	limitedRequest := func(id, trace string) pluginapi.RequestInterceptRequest {
		req := h.request(id, trace)
		req.Metadata = map[string]any{"caller_scope": limited.Scope, "request_path": "/v1/messages"}
		return req
	}

	// A positive remaining quota admits the request: admission compares no
	// estimate against the limit, so a declared body far larger than the quota
	// is still admitted.
	if response := h.intercept(limitedRequest("req-quota-admit", "trace-quota-admit")); response.Terminate {
		t.Fatalf("a positive remaining quota must admit the request: %+v", response)
	}
	// The reported actual usage is charged in full, beyond the limit.
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-quota", TraceID: "trace-quota-admit", Provider: "openai", Model: testModel,
		APIKey: limited.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail: pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-quota-admit", "trace-quota-admit", pluginapi.RequestCompletionSucceeded)
	used, err := h.store().GetKey(ctx, limited.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if used.SpentMicroUSD != 2000 || used.QuotaUsedMicroUSD <= 1 {
		t.Fatalf("actual usage must be charged in full beyond the quota: spent=%d quota_used=%d",
			used.SpentMicroUSD, used.QuotaUsedMicroUSD)
	}

	// The exhausted key rejects the next request as a normal business outcome.
	response := h.intercept(limitedRequest("req-quota", "trace-quota"))
	if !response.Terminate || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("quota rejection must terminate with 429, got %+v", response)
	}
	if message := h.m.LastError(); message != "" {
		t.Fatalf("a quota rejection must not degrade health, got %q", message)
	}

	if response := h.intercept(h.request("req-other-key", "trace-other-key")); response.Terminate {
		t.Fatalf("another key must still be admitted after a quota rejection: %+v", response)
	}
}

// TestUnpricedModelRejectedThenPricedSucceeds verifies that a missing price is a
// per-request rejection that disappears once the model is priced.
func TestUnpricedModelRejectedThenPricedSucceeds(t *testing.T) {
	h := newHarness(t)
	probe := h.request("req-unpriced", "trace-unpriced")
	probe.Model, probe.RequestedModel = "unpriced-model", "unpriced-model"
	response := h.intercept(probe)
	if !response.Terminate || response.StatusCode != http.StatusForbidden {
		t.Fatalf("unpriced model must terminate with 403, got %+v", response)
	}
	if message := h.m.LastError(); message != "" {
		t.Fatalf("an unpriced model must not degrade health, got %q", message)
	}

	if err := h.store().PutPrice(context.Background(), credit.Price{
		Model: "unpriced-model", Rates: credit.Rates{Input: 1000000, Output: 1000000},
	}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	priced := h.request("req-priced", "trace-priced")
	priced.Model, priced.RequestedModel = "unpriced-model", "unpriced-model"
	if response := h.intercept(priced); response.Terminate {
		t.Fatalf("a newly priced model must be admitted: %+v", response)
	}
}

// Pricing failures must explain the operator action, never masquerade as an
// upstream availability failure or fall through to a lower-priority price.
func TestPricingRejectionExplainsMissingOrDisabledPrice(t *testing.T) {
	for _, disabledRule := range []bool{false, true} {
		name := "missing"
		if disabledRule {
			name = "disabled winning rule"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			model := "unpriced-model"
			rule := credit.PricingRule{
				ID: "disabled", MatchKind: credit.MatchExact, Pattern: testModel, Priority: 5,
				Price: credit.Price{Rates: credit.Rates{Input: 1_000_000}},
			}
			if disabledRule {
				model = testModel // A lower-priority exact base price already exists.
				if err := h.store().(pricingStore).PutPricingRule(ctx, rule); err != nil {
					t.Fatal(err)
				}
			}
			probe := h.request("denied", "trace-denied")
			probe.Model, probe.RequestedModel = model, model
			resp := h.intercept(probe)
			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(resp.ResponseBody, &body); err != nil {
				t.Fatal(err)
			}
			if !resp.Terminate || resp.StatusCode != http.StatusForbidden ||
				body.Error.Type != "permission_error" ||
				body.Error.Message != "model pricing is not configured or is disabled; contact the administrator" {
				t.Fatalf("unclear pricing rejection: status=%d body=%s", resp.StatusCode, resp.ResponseBody)
			}
			report, err := h.store().Report(ctx, credit.Filter{})
			if err != nil || report.Summary.Requests != 0 {
				t.Fatalf("pricing denial created accounting: %+v, %v", report.Summary, err)
			}
			if disabledRule {
				rule.Enabled = true
				if err := h.store().(pricingStore).PutPricingRule(ctx, rule); err != nil {
					t.Fatal(err)
				}
			} else if err := h.store().PutPrice(ctx, credit.Price{
				Model: model, Rates: credit.Rates{Input: 1_000_000},
			}); err != nil {
				t.Fatal(err)
			}
			probe.RequestID, probe.TraceID = "priced", "trace-priced"
			if resp := h.intercept(probe); resp.Terminate {
				t.Fatalf("configured price did not restore admission: %+v", resp)
			}
			if h.m.LastError() != "" {
				t.Fatalf("business rejection degraded health: %s", h.m.LastError())
			}
		})
	}
}

// TestReconfigureDrainsAdmissionBeforeLivenessCheck pins the interleaving that a
// naive check order would get wrong: an admission that is still inside Admit
// when the reconfigure starts must be drained and then observed by the
// liveness check, so its database is retained instead of abandoned.
func TestReconfigureDrainsAdmissionBeforeLivenessCheck(t *testing.T) {
	h := newHarness(t)
	blocking := blockingAdmitBackend{
		Backend: h.store(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h.m.mu.Lock()
	h.m.live.store = blocking
	h.m.mu.Unlock()

	intercepted := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() { intercepted <- h.m.interceptBefore(h.request("req-drained", "trace-drained")) }()

	// Barrier 1: the admission is inside Admit and holds the shared gate.
	<-blocking.entered

	reconfigured := make(chan pluginabi.Envelope, 1)
	go func() {
		payload, err := json.Marshal(lifecycleRequest{ConfigYAML: captureYAMLMonths(h.dir, 6), SchemaVersion: pluginabi.SchemaVersion})
		if err != nil {
			reconfigured <- pluginabi.Envelope{}
			return
		}
		out, err := h.m.HandleCall(pluginabi.MethodPluginReconfigure, payload)
		if err != nil {
			reconfigured <- pluginabi.Envelope{}
			return
		}
		var envelope pluginabi.Envelope
		if err := json.Unmarshal(out, &envelope); err != nil {
			reconfigured <- pluginabi.Envelope{}
			return
		}
		reconfigured <- envelope
	}()

	// Barrier 2: the reconfigure stopped new admissions and is now waiting for
	// the shared gate, so the admission below commits after that point.
	h.waitForState(stateReconfiguring)
	close(blocking.release)

	if response := <-intercepted; response.Terminate {
		t.Fatalf("an admission drained by the reconfigure must still be admitted: %+v", response)
	}
	envelope := <-reconfigured
	if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != http.StatusConflict {
		t.Fatalf("the drained admission must make the swap unsafe (409), got %+v", envelope)
	}
	if _, ok := h.store().(blockingAdmitBackend); !ok {
		t.Fatal("the refused reconfigure must keep the previous store")
	}
	// The drained admission is visible in the retained store as an unresolved
	// execution, which is exactly why the swap was refused.
	active, err := h.store().HasActive(context.Background())
	if err != nil || !active {
		t.Fatalf("the drained admission must be visible in the retained store: active=%v err=%v", active, err)
	}
}

// TestReconfigureRefusesDataDirChange verifies that relocating the accounting
// database at runtime is refused: only a stop and start may move it.
func TestReconfigureRefusesDataDirChange(t *testing.T) {
	h := newHarness(t)
	envelope := h.call(pluginabi.MethodPluginReconfigure, lifecycleRequest{
		ConfigYAML: captureYAML(t.TempDir()), SchemaVersion: pluginabi.SchemaVersion,
	})
	if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != http.StatusConflict {
		t.Fatalf("a data_dir change must be refused with 409, got %+v", envelope)
	}
	if response := h.intercept(h.request("req-kept", "trace-kept")); response.Terminate {
		t.Fatalf("the plugin must keep serving from the original database: %+v", response)
	}
}

// TestCountTokensCompletionIsBenign verifies that a native token-count probe,
// which never reserves money, may still be completed without degrading health or
// blocking admissions.
func TestCountTokensCompletionIsBenign(t *testing.T) {
	h := newHarness(t)
	probe := h.request("count-benign", "trace-count-benign")
	probe.Metadata = map[string]any{"caller_scope": h.key.Scope, "request_path": "/v1/messages/count_tokens"}
	if response := h.intercept(probe); response.Terminate {
		t.Fatalf("count_tokens probe was terminated: %+v", response)
	}
	h.complete("count-benign", "trace-count-benign", pluginapi.RequestCompletionSucceeded)
	if message := h.m.LastError(); message != "" {
		t.Fatalf("a count_tokens completion must be benign: health=%q", message)
	}
	if response := h.intercept(h.request("req-after-count", "trace-after-count")); response.Terminate {
		t.Fatalf("admission blocked after a benign count_tokens completion: %+v", response)
	}
}

// panicBackend simulates an internal panic to prove fail-closed behavior.
type panicBackend struct{ credit.Backend }

func (panicBackend) KeyByScope(context.Context, string) (credit.Key, error) {
	panic("injected panic")
}

// faultBackend injects store failures to prove fail-closed behavior per call.
type faultBackend struct {
	credit.Backend
	admitErr error
	usageErr error
}

func (f faultBackend) Admit(ctx context.Context, in credit.AdmitInput) (credit.Request, error) {
	if f.admitErr != nil {
		return credit.Request{}, f.admitErr
	}
	return f.Backend.Admit(ctx, in)
}

func (f faultBackend) RecordUsage(ctx context.Context, usage credit.Usage) error {
	if f.usageErr != nil {
		return f.usageErr
	}
	return f.Backend.RecordUsage(ctx, usage)
}

// blockingAdmitBackend parks one admission inside Admit so a test can
// interleave it with a lifecycle transition deterministically.
type blockingAdmitBackend struct {
	credit.Backend
	entered chan struct{}
	release chan struct{}
}

func (b blockingAdmitBackend) Admit(ctx context.Context, in credit.AdmitInput) (credit.Request, error) {
	close(b.entered)
	<-b.release
	return b.Backend.Admit(ctx, in)
}

// activeBackend advertises the optional HasActive capability.
type activeBackend struct {
	credit.Backend
	active bool
	err    error
}

func (a activeBackend) HasActive(context.Context) (bool, error) { return a.active, a.err }

// oneShotAdmitFault fails only the first admission it sees, so a test can prove
// that one storage failure leaves the plugin admitting the next request.
type oneShotAdmitFault struct {
	credit.Backend
	mu  sync.Mutex
	err error
}

func (b *oneShotAdmitFault) Admit(ctx context.Context, in credit.AdmitInput) (credit.Request, error) {
	b.mu.Lock()
	err := b.err
	b.err = nil
	b.mu.Unlock()
	if err != nil {
		return credit.Request{}, err
	}
	return b.Backend.Admit(ctx, in)
}

// TestConflictRejectionDoesNotBlockOtherKeys is the F02 regression: a
// conflicting request id is a request-level outcome that refuses only that
// request, and an unrecognized storage failure refuses only the request that hit
// it while still being observable through health.
func TestConflictRejectionDoesNotBlockOtherKeys(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	real := h.m.live.store
	h.m.live.store = &oneShotAdmitFault{Backend: real, err: credit.ErrConflict}
	h.m.mu.Unlock()

	// A: the conflicting request id is refused as 409, exactly like admitStatus
	// reports it.
	response := h.intercept(h.request("req-conflict", "trace-conflict"))
	if !response.Terminate || response.StatusCode != http.StatusConflict {
		t.Fatalf("a conflicting request must be refused with 409, got %+v", response)
	}
	if message := h.m.LastError(); message != "" {
		t.Fatalf("a conflicting request id must not degrade health: %q", message)
	}

	// B: the very next request is admitted and billed, so a conflict for one
	// request never becomes a global block.
	if response := h.intercept(h.request("req-next", "trace-next")); response.Terminate {
		t.Fatalf("another request must still be admitted: %+v", response)
	}
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-next", TraceID: "trace-next", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail: pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-next", "trace-next", pluginapi.RequestCompletionSucceeded)

	// An unrecognized storage failure is refused with 503 and reported through
	// health, but it is not sticky: the next request is admitted again.
	h.m.mu.Lock()
	h.m.live.store = faultBackend{Backend: real, admitErr: errors.New("storage exploded")}
	h.m.mu.Unlock()
	response = h.intercept(h.request("req-fault", "trace-fault"))
	if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an unrecognized storage failure must be refused with 503, got %+v", response)
	}
	if h.m.LastError() == "" {
		t.Fatal("an unrecognized storage failure must be observable through health")
	}
	h.m.mu.Lock()
	h.m.live.store = real
	h.m.mu.Unlock()
	if response := h.intercept(h.request("req-after-fault", "trace-after-fault")); response.Terminate {
		t.Fatalf("a recovered storage failure must not block later admissions: %+v", response)
	}
}

// TestRegisterAcceptsStoreManifest pins that CPA plugin store lifecycle requests
// carrying the store manifest block register and reconfigure without rejection.
func TestRegisterAcceptsStoreManifest(t *testing.T) {
	m := NewManager()
	dir := t.TempDir()
	t.Cleanup(func() {
		_, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil)
	})

	payload, err := json.Marshal(lifecycleRequest{
		ConfigYAML:    []byte(fmt.Sprintf("enabled: true\npriority: 0\nstore:\n  id: cliproxyapi-credit-manager\n  name: Credit Manager\n  version: 0.1.2\ndata_dir: %q\n", dir)),
		SchemaVersion: pluginabi.SchemaVersion,
	})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}

	raw, err := m.HandleCall(pluginabi.MethodPluginRegister, payload)
	if err != nil {
		t.Fatalf("register returned error: %v", err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode register envelope: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("register failed with store manifest: %s", raw)
	}
}
