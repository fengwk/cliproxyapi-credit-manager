package plugin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/config"
)

// warmupSessionHeader carries the per-probe random session marker. It is a
// correlation label owned by this plugin: it identifies one specific probe so
// an operator can follow it through the host, and it is never a customer value.
//
// The marker is deliberately NOT a billing gate. Usage attribution never
// consults it: the host resolves UsageRecord.AccessProvider from the client
// authentication path, so a probe (which has no client identity) can not be
// attributed to a customer key, and a customer that forges this header can not
// obtain a billing exemption because no code path reads it for that purpose.
const warmupSessionHeader = "X-Credit-Manager-Warmup-Session"

// warmupSwapGrace bounds how long a reconfiguration waits for an in-flight
// warmup host callback before refusing the swap.
//
// A host model execution is a synchronous host call this plugin can not cancel,
// so the grace exists only to keep reconfiguration from waiting on a stuck
// probe. It is NOT a safety bound: a worker that does not drain inside the
// grace is never abandoned or replaced, the swap is refused with 409 and the
// previous configuration keeps running. Shutdown, which is followed by
// dlclose() of this library, waits for the callback without a bound instead.
//
// It is a variable so tests can shorten the wait.
var warmupSwapGrace = 5 * time.Second

// errWarmupUnavailable reports that warmup is enabled but the host callback
// bridge is missing, so no probe can be issued.
var errWarmupUnavailable = errors.New("warmup enabled but host callback bridge is unavailable")

// errWarmupBusy reports that a warmup host callback is still in flight, so the
// plugin refuses to replace the worker that owns it.
var errWarmupBusy = errors.New("a warmup probe is still in flight")

// warmupWorker is the single serial background warmup worker. At most one
// worker exists at a time and it issues at most one host call at a time; a
// worker is never dropped while it can still be inside a host callback.
type warmupWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// SetHostCall installs the native host callback bridge. It must be called by
// the CGO shim during cliproxy_plugin_init, before any lifecycle call; a
// manager without a bridge issues no warmup requests.
func (m *Manager) SetHostCall(call func(method string, payload []byte) ([]byte, error)) {
	m.mu.Lock()
	m.hostCall = call
	m.mu.Unlock()
}

// hostCallSnapshot returns the installed bridge, or nil when there is none.
func (m *Manager) hostCallSnapshot() func(string, []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hostCall
}

// startWarmup launches the background warmup worker when warmup is enabled with
// at least one target. It must be called while the exclusive gate is held (or
// before the manager is visible to handlers), like startMaintenance.
//
// No target, a disabled warmup, an invalid period, a missing host bridge or a
// worker that is still running starts no worker: the default configuration
// issues no request, and two workers never probe in parallel.
func (m *Manager) startWarmup(cfg config.Warmup) {
	if !cfg.Enabled || len(cfg.Targets) == 0 {
		return
	}
	if cfg.Period <= 0 {
		m.recordError(errors.New("warmup interval must be positive"))
		return
	}
	call := m.hostCallSnapshot()
	if call == nil {
		m.recordError(errWarmupUnavailable)
		return
	}
	m.mu.Lock()
	if m.warmup != nil {
		// Every caller drains the previous worker first, so this is only
		// reachable from a bug: refuse to run two probes at once.
		m.mu.Unlock()
		m.recordError(errWarmupBusy)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &warmupWorker{cancel: cancel, done: make(chan struct{})}
	m.warmup = worker
	m.mu.Unlock()
	go m.warmupLoop(ctx, worker.done, cfg)
}

// warmupRunning returns the current worker, nil when none is installed.
func (m *Manager) warmupRunning() *warmupWorker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.warmup
}

// cancelWarmup stops new probes without waiting for a probe already inside the
// host. The worker reference stays installed so a later transition can still
// wait for that callback to return.
func (m *Manager) cancelWarmup() {
	if worker := m.warmupRunning(); worker != nil {
		worker.cancel()
	}
}

// drainWarmupForSwap cancels the running worker and waits up to warmupSwapGrace
// for its in-flight host callback to return, reporting whether the worker is
// gone.
//
// Safety contract: a worker that does not drain is left installed and RUNNING
// its callback; the caller must therefore keep the current configuration and
// refuse the swap. Replacing it would start a second probe and would leave the
// library's goroutine alive past the teardown that follows.
func (m *Manager) drainWarmupForSwap() bool {
	worker := m.warmupRunning()
	if worker == nil {
		return true
	}
	worker.cancel()
	timer := time.NewTimer(warmupSwapGrace)
	defer timer.Stop()
	select {
	case <-worker.done:
	case <-timer.C:
		return false
	}
	m.mu.Lock()
	if m.warmup == worker {
		m.warmup = nil
	}
	m.mu.Unlock()
	return true
}

// drainWarmupForShutdown cancels the worker and waits without a bound for its
// in-flight host callback to leave this library.
//
// This wait is deliberately unbounded. The host frees the callback context and
// dlclose()s this shared library as soon as plugin.shutdown returns, so
// returning early would let a goroutine still executing this library's code
// return into unmapped memory. There is no host-side cancellation for a model
// execution callback, so a stuck upstream request delays plugin shutdown; that
// limitation is documented rather than papered over.
func (m *Manager) drainWarmupForShutdown() {
	worker := m.warmupRunning()
	if worker == nil {
		return
	}
	worker.cancel()
	<-worker.done
	m.mu.Lock()
	if m.warmup == worker {
		m.warmup = nil
	}
	m.mu.Unlock()
}

// warmupLoop probes every target once immediately and then once per interval
// until ctx is canceled. The pass is serial: at most one host call is in flight.
func (m *Manager) warmupLoop(ctx context.Context, done chan struct{}, cfg config.Warmup) {
	defer close(done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		m.warmupPass(ctx, cfg.Targets)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(cfg.Period)
	}
}

// warmupPass issues one probe per target, in order, and returns how many probes
// were attempted before ctx was canceled. A failing target is recorded through
// health and never stops the remaining targets or any customer request.
func (m *Manager) warmupPass(ctx context.Context, targets []config.WarmupTarget) int {
	call := m.hostCallSnapshot()
	if call == nil {
		return 0
	}
	attempted := 0
	for _, target := range targets {
		if ctx.Err() != nil {
			return attempted
		}
		attempted++
		if err := m.safeWarmupTarget(call, target); err != nil {
			m.recordError(err)
		}
	}
	return attempted
}

// safeWarmupTarget runs one probe and converts a panic at the host boundary
// into a recorded probe failure, so a probe bug can never take down the host
// process or the remaining targets.
func (m *Manager) safeWarmupTarget(call func(string, []byte) ([]byte, error), target config.WarmupTarget) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = warmupFailure(target)
		}
	}()
	return m.warmupTarget(call, target)
}

// warmupTarget issues one pinned, marked probe and consumes the host response.
//
// The probe pins AuthID and ForcedProvider exactly as configured, carries no
// host callback id and no caller identity, and therefore can not inherit a
// customer request's context or disturb host-side credential selection.
func (m *Manager) warmupTarget(call func(string, []byte) ([]byte, error), target config.WarmupTarget) error {
	payload, err := warmupPayload(target, newWarmupSessionID())
	if err != nil {
		return warmupFailure(target)
	}
	raw, err := call(pluginabi.MethodHostModelExecute, payload)
	if err != nil {
		return warmupFailure(target)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		return warmupFailure(target)
	}
	return nil
}

// warmupFailure reduces a probe failure to a bounded, non-sensitive health
// message: the host error may quote an upstream body or URL, which must not
// reach health or the management API.
func warmupFailure(target config.WarmupTarget) error {
	return errors.New("warmup probe failed for provider " + target.Provider)
}

// warmupPayload builds one host.model.execute request. Entry and exit protocol
// are both the target protocol, so the response is decoded in the same format
// the probe was written in and no cross-protocol translation is required.
func warmupPayload(target config.WarmupTarget, sessionID string) ([]byte, error) {
	body, err := warmupBody(target.Protocol, target.Model)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol:  target.Protocol,
		ExitProtocol:   target.Protocol,
		Model:          target.Model,
		Stream:         false,
		Body:           body,
		Headers:        http.Header{warmupSessionHeader: []string{sessionID}},
		ForcedProvider: target.Provider,
		AuthID:         target.AuthID,
	})
}

// warmupBody builds the smallest legal request for the target protocol: a
// single "." turn capped at one output token.
func warmupBody(protocol, model string) ([]byte, error) {
	switch protocol {
	case "claude", "openai":
		return json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages": []any{
				map[string]any{"role": "user", "content": "."},
			},
		})
	case "gemini":
		return json.Marshal(map[string]any{
			"contents": []any{
				map[string]any{"parts": []any{map[string]any{"text": "."}}},
			},
			"generationConfig": map[string]any{"maxOutputTokens": 1},
		})
	default:
		return nil, errors.New("unsupported warmup protocol")
	}
}

// newWarmupSessionID returns a fresh 128-bit random session marker. The value
// is unpredictable and unique per probe, so it can not be anticipated by a
// client and it attributes exactly one probe.
func newWarmupSessionID() string {
	return "warmup-" + rand.Text()
}
