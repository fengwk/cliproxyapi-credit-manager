package plugin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/config"
)

// warmupSessionHeader carries the per-probe random session marker. It is a
// correlation label owned by this plugin, never a customer value and never a
// billing gate: usage attribution is decided by the host-resolved
// UsageRecord.AccessProvider, which a background execution does not have.
const warmupSessionHeader = "X-Credit-Manager-Warmup-Session"

// warmupSwapGrace bounds how long a reconfiguration waits for an in-flight pass
// before refusing the swap. It is a reconfiguration bound only: shutdown drains
// the same non-cancellable host callback for as long as it takes. It is a
// variable so tests can shorten it.
var warmupSwapGrace = 5 * time.Second

// errWarmupUnavailable reports that warmup is enabled but the host callback
// bridge is missing, so no probe can be issued.
var errWarmupUnavailable = errors.New("warmup enabled but host callback bridge is unavailable")

// warmupWorker is the single serial probe loop. At most one worker exists at a
// time and it runs at most one pass, hence one host call, at a time. Disabling,
// quiescing or a failed reconfiguration pauses it and shutdown drains it; it is
// never abandoned, replaced or duplicated.
type warmupWorker struct {
	cancel context.CancelFunc
	done   chan struct{}

	// wake is signalled without blocking whenever the state changes, so a
	// parked loop re-evaluates it promptly.
	wake chan struct{}

	mu        sync.Mutex
	cfg       config.Warmup
	paused    bool
	passDone  chan struct{}      // non-nil while a pass runs; closed when it ends
	passAbort context.CancelFunc // cancels the running pass when paused
}

func newWarmupWorker(cfg config.Warmup) *warmupWorker {
	return &warmupWorker{wake: make(chan struct{}, 1), cfg: cfg}
}

// signal wakes the loop without ever blocking the caller.
func (w *warmupWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// start makes the worker probe cfg and clears any pause. A cfg that is disabled
// or has no target parks the loop, so reusing a worker never probes on its own.
func (w *warmupWorker) start(cfg config.Warmup) {
	w.mu.Lock()
	w.cfg = cfg
	w.paused = false
	w.mu.Unlock()
	w.signal()
}

// pause stops new probes, including the rest of a running pass, without
// touching the worker or its in-flight host callback.
func (w *warmupWorker) pause() {
	w.mu.Lock()
	w.paused = true
	abort := w.passAbort
	w.mu.Unlock()
	if abort != nil {
		abort()
	}
	w.signal()
}

// resume clears a pause and keeps the configured period.
func (w *warmupWorker) resume() {
	w.mu.Lock()
	w.paused = false
	w.mu.Unlock()
	w.signal()
}

// pauseAndWaitIdle pauses the worker and reports whether it became idle inside
// grace. The in-flight pass observed here is the only one that can still run:
// pause and pass registration take the same lock, so no pass can start after
// this returns.
func (w *warmupWorker) pauseAndWaitIdle(grace time.Duration) bool {
	w.mu.Lock()
	w.paused = true
	abort := w.passAbort
	inFlight := w.passDone
	w.mu.Unlock()
	if abort != nil {
		abort()
	}
	w.signal()
	if inFlight == nil {
		return true
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-inFlight:
		return true
	case <-timer.C:
		return false
	}
}

// probeable reports whether a configuration should issue probes at all.
func probeable(cfg config.Warmup) bool {
	return cfg.Enabled && len(cfg.Targets) > 0 && cfg.Period > 0
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

// warmupRunning returns the current worker, nil when none is installed.
func (m *Manager) warmupRunning() *warmupWorker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.warmup
}

// startWarmup makes the warmup configuration effective. It must be called
// while the exclusive gate is held (or before the manager is visible to
// handlers), like startMaintenance.
//
// An installed worker is reconfigured in place rather than replaced, so two
// probe loops never run at once. A disabled configuration, an invalid period
// and a missing host bridge start no worker at all: the default configuration
// issues no request.
func (m *Manager) startWarmup(cfg config.Warmup) {
	if worker := m.warmupRunning(); worker != nil {
		worker.start(cfg)
		return
	}
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
	ctx, cancel := context.WithCancel(context.Background())
	worker := newWarmupWorker(cfg)
	worker.cancel = cancel
	worker.done = make(chan struct{})
	m.mu.Lock()
	if m.warmup != nil {
		// Lifecycle transitions are serialized, so this is unreachable unless a
		// caller skipped the pause/drain step: never run two probe loops.
		m.mu.Unlock()
		cancel()
		m.recordError(errors.New("a warmup worker is already installed"))
		return
	}
	m.warmup = worker
	m.mu.Unlock()
	go m.warmupLoop(ctx, call, worker)
}

// pauseWarmup stops new probes and keeps the worker installed, so a later
// resume restores the same loop instead of starting a second one.
func (m *Manager) pauseWarmup() {
	if worker := m.warmupRunning(); worker != nil {
		worker.pause()
	}
}

// stopWarmupForSwap pauses the worker and retires it once its pass is idle.
//
// A pass that does not go idle inside warmupSwapGrace is left installed and
// running: the worker is resumed and the caller must refuse the swap, so a
// failed reconfiguration keeps the previous periodic warmup instead of leaving
// it dead. The worker is never abandoned and a second loop is never started.
func (m *Manager) stopWarmupForSwap() bool {
	worker := m.warmupRunning()
	if worker == nil {
		return true
	}
	worker.mu.Lock()
	wasPaused := worker.paused
	worker.mu.Unlock()
	if !worker.pauseAndWaitIdle(warmupSwapGrace) {
		if !wasPaused {
			worker.resume()
		}
		return false
	}
	worker.cancel()
	<-worker.done
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
	worker.pause()
	worker.cancel()
	<-worker.done
	m.mu.Lock()
	if m.warmup == worker {
		m.warmup = nil
	}
	m.mu.Unlock()
}

// warmupLoop runs one pass immediately, then one per interval, until ctx is
// canceled. Pausing aborts the rest of the running pass and parks the loop;
// resuming probes again.
func (m *Manager) warmupLoop(ctx context.Context, call func(string, []byte) ([]byte, error), w *warmupWorker) {
	defer close(w.done)
	for {
		// The pass decision and its registration share one critical section, so
		// a pause can never interleave and leave a pass running unobserved.
		w.mu.Lock()
		cfg := w.cfg
		run := !w.paused && probeable(cfg) && ctx.Err() == nil
		var (
			passCtx  context.Context
			passDone chan struct{}
		)
		if run {
			passCtx, w.passAbort = context.WithCancel(ctx)
			passDone = make(chan struct{})
			w.passDone = passDone
		}
		w.mu.Unlock()

		if !run {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
			}
			continue
		}

		m.warmupPass(passCtx, call, cfg.Targets)
		w.mu.Lock()
		abort := w.passAbort
		w.passAbort = nil
		w.passDone = nil
		w.mu.Unlock()
		if abort != nil {
			abort()
		}
		close(passDone)

		timer := time.NewTimer(cfg.Period)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// warmupPass issues one probe per target, in order, and returns how many probes
// were attempted before the pass was aborted. A failing target is recorded
// through health and never stops the remaining targets or any customer request.
func (m *Manager) warmupPass(ctx context.Context, call func(string, []byte) ([]byte, error), targets []config.WarmupTarget) int {
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
