package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/config"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

// hostProbe is one recorded host callback issued by this plugin.
type hostProbe struct {
	method  string
	payload []byte
}

// fakeHost stands in for the native host callback bridge. It records every
// call, signals entry on a channel, and can hold a call inside the "host" until
// released, so tests synchronize on real events instead of sleeping.
type fakeHost struct {
	mu       sync.Mutex
	probes   []hostProbe
	inFlight int
	gate     chan struct{}
	fail     bool

	// entered receives one signal per call, including held calls.
	entered chan struct{}
}

func newFakeHost() *fakeHost {
	return &fakeHost{entered: make(chan struct{}, 64)}
}

// hold makes every later call block until release is called.
func (f *fakeHost) hold() {
	f.mu.Lock()
	if f.gate == nil {
		f.gate = make(chan struct{})
	}
	f.mu.Unlock()
}

// release unblocks every held call and stops holding later ones.
func (f *fakeHost) release() {
	f.mu.Lock()
	gate := f.gate
	f.gate = nil
	f.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// failWith makes every later call answer with a host error envelope.
func (f *fakeHost) failWith() {
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
}

func (f *fakeHost) call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.probes = append(f.probes, hostProbe{method: method, payload: append([]byte(nil), payload...)})
	f.inFlight++
	gate, fail := f.gate, f.fail
	f.mu.Unlock()

	f.entered <- struct{}{}
	if gate != nil {
		<-gate
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if fail {
		return pluginabi.NewErrorEnvelope("host_call_failed", "upstream unavailable", http.StatusBadGateway)
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: json.RawMessage(`{"status_code":200}`)})
}

func (f *fakeHost) recorded() []hostProbe {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hostProbe(nil), f.probes...)
}

func (f *fakeHost) inFlightCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight
}

// warmupYAML renders a valid plugin configuration with a warmup block.
func warmupYAML(dataDir, warmup string) []byte {
	return []byte(fmt.Sprintf(
		"data_dir: %q\nhistory:\n  retention_months: 3\nmax_pending_requests: 16\nrequest_capture:\n  enabled: false\nwarmup:\n%s",
		dataDir, warmup))
}

const (
	warmupDisabledBlock = "  enabled: false\n"
	warmupEnabledOne    = `  enabled: true
  interval: 1h
  targets:
    - auth_id: auth-one
      provider: openai
      model: test-model
`
	warmupEnabledTwo = `  enabled: true
  interval: 1h
  targets:
    - auth_id: auth-one
      provider: openai
      model: test-model
    - auth_id: auth-two
      provider: claude
      model: test-model
`
)

// newWarmupHarness configures one plugin with the given warmup block and a host
// bridge backed by fake, plus one usable customer key and price.
func newWarmupHarness(t *testing.T, fake *fakeHost, warmup string) *harness {
	t.Helper()
	t.Setenv("TZ", "UTC")
	m := NewManager()
	m.SetHostCall(fake.call)
	dir := t.TempDir()
	h := &harness{t: t, m: m, dir: dir}
	t.Cleanup(func() {
		// Shutdown drains the warmup worker, so no test leaves a goroutine that
		// can still call into the fake host.
		_, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil)
	})
	// A held fake host must be released before the shutdown above, otherwise a
	// test that fails early would leave the drain waiting on an uncancellable
	// callback and turn a clear failure into a timeout.
	t.Cleanup(fake.release)
	registerOK(t, m, warmupYAML(dir, warmup))
	store := h.store()
	key, plain, err := store.CreateKey(context.Background(), credit.KeySpec{Label: "test", Enabled: true})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := store.PutPrice(context.Background(), credit.Price{Model: testModel, Rates: credit.Rates{Input: 1000000, Output: 2000000}}); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	h.key, h.plain = key, plain
	return h
}

// registerOK applies a configuration and requires the host to accept it.
func registerOK(t *testing.T, m *Manager, yaml []byte) pluginabi.Envelope {
	t.Helper()
	envelope := reconfigureRaw(t, m, pluginabi.MethodPluginRegister, yaml)
	if !envelope.OK {
		t.Fatalf("register failed: %+v", envelope)
	}
	return envelope
}

// reconfigureRaw applies a lifecycle payload and returns the raw envelope, so
// tests can assert refusal responses.
func reconfigureRaw(t *testing.T, m *Manager, method string, yaml []byte) pluginabi.Envelope {
	t.Helper()
	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: yaml, SchemaVersion: pluginabi.SchemaVersion})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	raw, err := m.HandleCall(method, payload)
	if err != nil {
		t.Fatalf("%s returned a transport error: %v", method, err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode %s envelope %s: %v", method, raw, err)
	}
	return envelope
}

// awaitProbe waits for one warmup probe to reach the fake host.
func awaitProbe(t *testing.T, fake *fakeHost) {
	t.Helper()
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("a warmup probe never reached the host")
	}
}

// awaitDrained waits for the worker's host callback to leave this library.
func awaitDrained(t *testing.T, worker *warmupWorker) {
	t.Helper()
	select {
	case <-worker.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the warmup worker never stopped")
	}
}

// awaitHealth waits until the sticky health message reports a warmup failure.
func awaitHealth(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(m.LastError(), "warmup probe failed") {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("warmup failure was never surfaced through health, last error = %q", m.LastError())
}

// TestWarmupPassPinsTargetAndMarksSession is the central probe-composition
// check: every probe must pin the configured credential and provider, carry a
// legal tiny body, a fresh proprietary session marker, and no caller identity.
func TestWarmupPassPinsTargetAndMarksSession(t *testing.T) {
	fake := newFakeHost()
	m := NewManager()
	m.SetHostCall(fake.call)

	targets := []config.WarmupTarget{
		{AuthID: "auth-openai", Provider: "openai", Model: "model-openai", Protocol: "openai"},
		{AuthID: "auth-claude", Provider: "anthropic", Model: "model-claude", Protocol: "claude"},
		{AuthID: "auth-gemini", Provider: "google", Model: "model-gemini", Protocol: "gemini"},
	}
	if attempted := m.warmupPass(context.Background(), targets); attempted != len(targets) {
		t.Fatalf("attempted = %d, want %d", attempted, len(targets))
	}
	probes := fake.recorded()
	if len(probes) != len(targets) {
		t.Fatalf("host calls = %d, want %d", len(probes), len(targets))
	}

	markers := map[string]bool{}
	for index, probe := range probes {
		want := targets[index]
		if probe.method != pluginabi.MethodHostModelExecute {
			t.Fatalf("probe %d method = %q, want %q", index, probe.method, pluginabi.MethodHostModelExecute)
		}
		// A background probe must never borrow a host callback identity from a
		// customer request: the host identifies the caller from its own context.
		if bytes.Contains(probe.payload, []byte("host_callback_id")) {
			t.Fatalf("probe %d carries a host callback id: %s", index, probe.payload)
		}
		var req pluginapi.HostModelExecutionRequest
		if err := json.Unmarshal(probe.payload, &req); err != nil {
			t.Fatalf("decode probe %d payload: %v", index, err)
		}
		if req.AuthID != want.AuthID || req.ForcedProvider != want.Provider || req.Model != want.Model {
			t.Fatalf("probe %d pinned %q/%q/%q, want %q/%q/%q",
				index, req.AuthID, req.ForcedProvider, req.Model, want.AuthID, want.Provider, want.Model)
		}
		if req.EntryProtocol != want.Protocol || req.ExitProtocol != want.Protocol || req.Stream {
			t.Fatalf("probe %d protocol = %q/%q stream=%v, want %q", index, req.EntryProtocol, req.ExitProtocol, req.Stream, want.Protocol)
		}
		if len(req.Query) != 0 || req.Path != "" || req.ProxyURL != "" || req.Alt != "" {
			t.Fatalf("probe %d must not set query/path/proxy/alt: %+v", index, req)
		}
		// No caller identity of any kind, and exactly one header: the marker.
		if len(req.Headers) != 1 {
			t.Fatalf("probe %d headers = %v, want only the session marker", index, req.Headers)
		}
		values, ok := req.Headers[warmupSessionHeader]
		if !ok || len(values) != 1 {
			t.Fatalf("probe %d session marker missing: %v", index, req.Headers)
		}
		marker := values[0]
		if !strings.HasPrefix(marker, "warmup-") || len(marker) < 20 {
			t.Fatalf("probe %d marker %q is not a random session id", index, marker)
		}
		if markers[marker] {
			t.Fatalf("session marker %q was reused across probes", marker)
		}
		markers[marker] = true

		assertTinyBody(t, index, want.Protocol, req.Body)
	}
	if strings.Contains(strings.ToLower(string(probes[0].payload)), "authorization") ||
		strings.Contains(strings.ToLower(string(probes[0].payload)), "caller_scope") {
		t.Fatalf("probe payload leaks caller identity: %s", probes[0].payload)
	}
}

// assertTinyBody verifies the probe body is a legal single-token request for
// the target protocol.
func assertTinyBody(t *testing.T, index int, protocol string, body []byte) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("probe %d body is not valid JSON: %s", index, body)
	}
	switch protocol {
	case "gemini":
		contents, _ := decoded["contents"].([]any)
		if len(contents) != 1 {
			t.Fatalf("probe %d gemini contents = %v", index, decoded["contents"])
		}
		parts, _ := contents[0].(map[string]any)["parts"].([]any)
		if len(parts) != 1 || parts[0].(map[string]any)["text"] != "." {
			t.Fatalf("probe %d gemini parts = %v", index, contents[0])
		}
		config, _ := decoded["generationConfig"].(map[string]any)
		if config["maxOutputTokens"] != float64(1) {
			t.Fatalf("probe %d gemini maxOutputTokens = %v", index, config["maxOutputTokens"])
		}
	default:
		if decoded["max_tokens"] != float64(1) {
			t.Fatalf("probe %d max_tokens = %v, want 1", index, decoded["max_tokens"])
		}
		messages, _ := decoded["messages"].([]any)
		if len(messages) != 1 || messages[0].(map[string]any)["content"] != "." {
			t.Fatalf("probe %d messages = %v", index, decoded["messages"])
		}
	}
}

// TestWarmupDisabledIssuesNoRequest proves the default configuration is inert:
// no worker is started and the host bridge is never called.
func TestWarmupDisabledIssuesNoRequest(t *testing.T) {
	for name, block := range map[string]string{
		"disabled":          warmupDisabledBlock,
		"enabled-no-target": "  enabled: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeHost()
			h := newWarmupHarness(t, fake, block)
			if worker := h.m.warmupRunning(); worker != nil {
				t.Fatalf("warmup %s must not start a worker", name)
			}
			if probes := fake.recorded(); len(probes) != 0 {
				t.Fatalf("warmup %s issued %d host calls", name, len(probes))
			}
			// Customer accounting keeps working in the same manager.
			if response := h.intercept(h.request("req-1", "trace-1")); response.Terminate {
				t.Fatalf("customer request was refused: %+v", response)
			}
			h.usage(pluginapi.UsageRecord{
				RequestID: "exec-1", TraceID: "trace-1", Provider: "openai", Model: testModel,
				APIKey: h.key.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
				Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			})
			h.complete("req-1", "trace-1", pluginapi.RequestCompletionSucceeded)
		})
	}
}

// TestWarmupWorkerStopsWhenReconfiguredOff proves the worker runs while warmup
// is enabled and stops issuing probes once it is reconfigured off, without
// disturbing customer accounting.
func TestWarmupWorkerStopsWhenReconfiguredOff(t *testing.T) {
	fake := newFakeHost()
	h := newWarmupHarness(t, fake, warmupEnabledOne)
	awaitProbe(t, fake)
	if h.m.warmupRunning() == nil {
		t.Fatal("an enabled warmup must install a worker")
	}

	envelope := reconfigureRaw(t, h.m, pluginabi.MethodPluginReconfigure, warmupYAML(h.dir, warmupDisabledBlock))
	if !envelope.OK {
		t.Fatalf("reconfigure with warmup disabled failed: %+v", envelope)
	}
	if worker := h.m.warmupRunning(); worker != nil {
		t.Fatal("the warmup worker must be released once warmup is disabled")
	}

	frozen := len(fake.recorded())
	if response := h.intercept(h.request("req-1", "trace-1")); response.Terminate {
		t.Fatalf("customer request was refused: %+v", response)
	}
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-1", TraceID: "trace-1", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail: pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-1", "trace-1", pluginapi.RequestCompletionSucceeded)

	if probes := fake.recorded(); len(probes) != frozen {
		t.Fatalf("probes resumed after warmup was disabled: %d -> %d", frozen, len(probes))
	}
	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD <= 0 {
		t.Fatalf("customer request was not billed, spent = %d", key.SpentMicroUSD)
	}
}

// TestReconfigureRefusedWhileProbeInFlight proves a stuck probe neither
// disappears nor multiplies: the swap is refused, the old worker is kept, no
// second probe starts, and customer traffic keeps flowing.
func TestReconfigureRefusedWhileProbeInFlight(t *testing.T) {
	previousGrace := warmupSwapGrace
	warmupSwapGrace = 50 * time.Millisecond
	t.Cleanup(func() { warmupSwapGrace = previousGrace })

	fake := newFakeHost()
	fake.hold()
	h := newWarmupHarness(t, fake, warmupEnabledOne)
	awaitProbe(t, fake)
	worker := h.m.warmupRunning()
	if worker == nil {
		t.Fatal("an enabled warmup must install a worker")
	}
	if got := fake.inFlightCount(); got != 1 {
		t.Fatalf("probes in flight = %d, want exactly 1", got)
	}

	envelope := reconfigureRaw(t, h.m, pluginabi.MethodPluginReconfigure, warmupYAML(h.dir, warmupDisabledBlock))
	if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != http.StatusConflict {
		t.Fatalf("reconfigure during a probe must be refused with 409, got %+v", envelope)
	}
	if h.m.warmupRunning() != worker {
		t.Fatal("the refused swap must keep the existing worker, not replace it")
	}
	if got := fake.inFlightCount(); got != 1 {
		t.Fatalf("a second probe was started, in flight = %d", got)
	}
	h.m.mu.Lock()
	state := h.m.state
	h.m.mu.Unlock()
	if state != stateLive {
		t.Fatalf("a refused swap must leave the plugin live, state = %d", state)
	}
	if response := h.intercept(h.request("req-keep", "trace-keep")); response.Terminate {
		t.Fatalf("customer traffic must keep working during a probe: %+v", response)
	}
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-keep", TraceID: "trace-keep", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail: pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-keep", "trace-keep", pluginapi.RequestCompletionSucceeded)
	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD <= 0 {
		t.Fatalf("customer request during a probe was not billed, spent = %d", key.SpentMicroUSD)
	}

	// Once the probe returns the worker drains, and the same reconfigure succeeds.
	fake.release()
	awaitDrained(t, worker)
	envelope = reconfigureRaw(t, h.m, pluginabi.MethodPluginReconfigure, warmupYAML(h.dir, warmupDisabledBlock))
	if !envelope.OK {
		t.Fatalf("reconfigure after the probe drained failed: %+v", envelope)
	}
}

// TestShutdownWaitsForInFlightProbe proves plugin.shutdown never returns while
// a warmup callback can still return into this library. A bounded wait here
// would be a use-after-free once the host dlclose()s the library.
func TestShutdownWaitsForInFlightProbe(t *testing.T) {
	fake := newFakeHost()
	fake.hold()
	h := newWarmupHarness(t, fake, warmupEnabledOne)
	awaitProbe(t, fake)

	shutdownReturned := make(chan struct{})
	go func() {
		_, _ = h.m.HandleCall(pluginabi.MethodPluginShutdown, nil)
		close(shutdownReturned)
	}()
	// Once the manager is terminal it can only be parked in the drain, because
	// the probe is still held inside the host and the worker can not exit.
	h.waitForState(stateClosed)
	if got := fake.inFlightCount(); got != 1 {
		t.Fatalf("probes in flight = %d, want 1", got)
	}
	// shutdown must stay parked until the callback returns: poll for a bounded
	// window because a return here is exactly the unmapped-memory hazard.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-shutdownReturned:
			t.Fatal("shutdown returned while the warmup callback was still inside the host")
		default:
		}
		runtime.Gosched()
	}
	// The drain must not hold the exclusive gate: a probe's own usage and
	// lifecycle callbacks are delivered asynchronously by the host and must
	// still be answerable while shutdown waits. This would deadlock (and the
	// test would time out) if the gate were held across the wait.
	h.usage(pluginapi.UsageRecord{RequestID: "exec-drain", TraceID: "trace-drain", Provider: "openai", Model: testModel})
	h.complete("req-drain", "trace-drain", pluginapi.RequestCompletionSucceeded)

	fake.release()
	select {
	case <-shutdownReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown never drained the warmup callback")
	}
	if worker := h.m.warmupRunning(); worker != nil {
		t.Fatal("shutdown must release the drained worker")
	}
}

// TestQuiesceStopsProbesWithoutBlocking proves quiesce cancels the worker and
// returns without waiting for a synchronous host call it can not cancel, and
// that the canceled worker issues no further probe.
func TestQuiesceStopsProbesWithoutBlocking(t *testing.T) {
	fake := newFakeHost()
	fake.hold()
	h := newWarmupHarness(t, fake, warmupEnabledTwo)
	awaitProbe(t, fake)
	worker := h.m.warmupRunning()
	if worker == nil {
		t.Fatal("an enabled warmup must install a worker")
	}

	if envelope, err := h.m.HandleCall(pluginabi.MethodPluginQuiesce, nil); err != nil || len(envelope) == 0 {
		t.Fatalf("quiesce failed: %s %v", envelope, err)
	}
	// The worker reference is kept so shutdown can still wait for it, and no
	// second probe may be started.
	if h.m.warmupRunning() != worker {
		t.Fatal("quiesce must keep the worker reference for shutdown to drain")
	}

	fake.release()
	awaitDrained(t, worker)
	if probes := fake.recorded(); len(probes) != 1 {
		t.Fatalf("quiesce must stop further probes, host calls = %d", len(probes))
	}
}

// TestWarmupFailureIsHealthOnly proves a failed probe is reported through
// health, never through the customer ledger, and never leaks host error text.
func TestWarmupFailureIsHealthOnly(t *testing.T) {
	fake := newFakeHost()
	fake.failWith()
	h := newWarmupHarness(t, fake, warmupEnabledOne)
	awaitProbe(t, fake)
	awaitHealth(t, h.m)
	if message := h.m.LastError(); strings.Contains(message, "upstream unavailable") {
		t.Fatalf("host error text leaked into health: %q", message)
	}

	if response := h.intercept(h.request("req-1", "trace-1")); response.Terminate {
		t.Fatalf("a failed probe must not block admissions: %+v", response)
	}
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-1", TraceID: "trace-1", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail: pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-1", "trace-1", pluginapi.RequestCompletionSucceeded)
	key, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.SpentMicroUSD <= 0 {
		t.Fatalf("customer request after a failed probe was not billed, spent = %d", key.SpentMicroUSD)
	}
}

// TestWarmupAttributionIsNotClient forgeable is the attribution check: a probe's
// own usage carries no client access provider and can not be billed, while a
// customer request that forges the warmup marker header is still billed in
// full, because billing never consults the marker.
func TestWarmupAttributionIsNotClientForgeable(t *testing.T) {
	h := newHarness(t)

	request := h.request("req-forge", "trace-forge")
	request.Headers = http.Header{warmupSessionHeader: []string{"warmup-forged"}}
	if response := h.intercept(request); response.Terminate {
		t.Fatalf("a forged marker must not change admission: %+v", response)
	}
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-forge", TraceID: "trace-forge", Provider: "openai", Model: testModel,
		APIKey: h.key.Principal, AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		SessionID: "warmup-forged", Source: "warmup-forged",
		Detail: pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
	})
	h.complete("req-forge", "trace-forge", pluginapi.RequestCompletionSucceeded)
	charged, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if charged.SpentMicroUSD <= 0 {
		t.Fatalf("a forged marker must not exempt a customer from billing, spent = %d", charged.SpentMicroUSD)
	}

	// A probe's usage record carries no client identity at all: the host
	// resolves AccessProvider from the client authentication path, which a
	// background execution never has. It must be ignored without touching any
	// ledger row and without fabricating a zero charge.
	h.usage(pluginapi.UsageRecord{
		RequestID: "exec-probe", TraceID: "trace-probe", Provider: "openai", Model: testModel,
		SessionID: "warmup-probe", Source: "warmup-probe",
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	after, err := h.store().GetKey(context.Background(), h.key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if after.SpentMicroUSD != charged.SpentMicroUSD || after.HeldMicroUSD != charged.HeldMicroUSD {
		t.Fatalf("a background usage record changed the customer ledger: %+v -> %+v", charged, after)
	}
}
