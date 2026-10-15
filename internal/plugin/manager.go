package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/capture"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/config"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/web"
)

// maintenanceInterval is how often the asynchronous maintenance pass runs.
// It is a variable so tests can shorten it.
var maintenanceInterval = time.Minute

// lifecycleState tracks whether the plugin may accept new accounting.
type lifecycleState int

const (
	// stateUnconfigured means no configuration has been applied yet.
	stateUnconfigured lifecycleState = iota
	// stateLive means the stores are open and new accounting is accepted.
	stateLive
	// stateReconfiguring means the live stores are being swapped: new
	// accounting is refused until the swap completes.
	stateReconfiguring
	// stateQuiesced means the stores were released and the plugin refuses new
	// accounting until it is reconfigured.
	stateQuiesced
	// stateClosed means the plugin is terminal and refuses everything.
	stateClosed
)

// liveState is an immutable snapshot of the dependencies serving one call.
type liveState struct {
	store    credit.Backend
	captures *capture.Store
	handler  *web.Handler
	cfg      config.Config
}

// Manager owns the credit and capture store lifecycle and dispatches every RPC
// method. The constructor performs no I/O: stores open only when the host sends
// plugin.register / plugin.reconfigure.
//
// Concurrency: every handler acquires the shared gate for the duration of the
// call, and lifecycle transitions take the gate exclusively. That guarantees a
// reconfigure or shutdown can never swap or close a store under an in-flight
// call, and that a swap never silently drops accounting.
type Manager struct {
	now func() time.Time

	// lifeMu serializes lifecycle transitions so a reconfigure/quiesce/shutdown
	// sequence can not interleave.
	lifeMu sync.Mutex

	// gate is held shared by every handler call and exclusively while the live
	// dependencies are swapped or released.
	gate sync.RWMutex

	// mu guards the fields below. It is only ever held for short snapshots, so
	// no external store call is ever made while holding it.
	mu               sync.Mutex
	state            lifecycleState
	live             *liveState
	raw              []byte
	hasConfig        bool
	lastError        string
	admissionBlocked bool

	maintenanceCancel context.CancelFunc
	maintenanceDone   chan struct{}
}

// NewManager returns a dispatcher that opens nothing until configured.
func NewManager() *Manager {
	return &Manager{now: time.Now}
}

// lifecycleRequest mirrors the host plugin.register/plugin.reconfigure payload.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// HandleCall dispatches one RPC method. Handler failures travel inside the
// envelope; a recovered panic becomes a plugin_error envelope so the host
// process is never taken down by this plugin.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			resp, err = mustEnvelope(errorResult("plugin_error", "internal error handling "+method, http.StatusInternalServerError))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodPluginQuiesce:
		m.quiesce()
		return okEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		m.shutdown()
		return okEnvelope(struct{}{})
	case pluginabi.MethodFrontendAuthIdentifier:
		return okEnvelope(struct {
			Identifier string `json:"identifier"`
		}{Identifier: authProviderID})
	case pluginabi.MethodFrontendAuthAuthenticate:
		return m.handleFrontendAuth(request)
	case pluginabi.MethodRequestInterceptBefore:
		// Fail closed: the interceptor converts every failure and panic into a
		// Terminate response instead of a host error envelope.
		return m.handleInterceptBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return m.handleInterceptAfter(request)
	case pluginabi.MethodRequestComplete:
		return m.handleRequestComplete(request)
	case pluginabi.MethodUsageHandle:
		return m.handleUsage(request)
	case pluginabi.MethodManagementRegister:
		return m.handleManagementRegister(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	default:
		return mustEnvelope(errorResult("unknown_method", "unknown method", 0))
	}
}

// begin snapshots the live dependencies and marks one in-flight call. The
// caller must pair it with end. When admission is true the call creates new
// accounting and is refused unless the plugin is live and financially healthy.
func (m *Manager) begin(admission bool) (*liveState, bool) {
	m.gate.RLock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == stateClosed || m.live == nil {
		m.gate.RUnlock()
		return nil, false
	}
	if admission && (m.state != stateLive || m.admissionBlocked) {
		m.gate.RUnlock()
		return nil, false
	}
	return m.live, true
}

// end releases the shared gate taken by begin.
func (m *Manager) end() {
	m.gate.RUnlock()
}

// LastError returns the sticky health message, empty when healthy.
func (m *Manager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastError
}

// recordError sets the sticky health message without blocking admissions. It is
// used for maintenance and usage problems that must be observable but must not
// stop the proxy.
func (m *Manager) recordError(err error) {
	if err == nil {
		return
	}
	m.mu.Lock()
	if m.lastError == "" {
		m.lastError = err.Error()
	}
	m.mu.Unlock()
}

// recordFinanceFault records a financial failure and blocks new admissions so
// the plugin fails closed instead of admitting unmetered requests.
func (m *Manager) recordFinanceFault(err error) {
	if err == nil {
		return
	}
	m.mu.Lock()
	if m.lastError == "" {
		m.lastError = err.Error()
	}
	m.admissionBlocked = true
	m.mu.Unlock()
}

// handleLifecycle applies a register or reconfigure payload.
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope(&PluginError{Code: "invalid_request", Message: "malformed lifecycle request", HTTPStatus: http.StatusBadRequest})
		}
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	return m.apply(req.ConfigYAML, req.SchemaVersion)
}

// apply validates a candidate configuration and swaps the live stores.
//
// A failed open rolls back to the previous configuration so an in-place
// reconfigure never leaves the plugin without a working store. The previous
// store is closed and the candidate opened only while the exclusive gate is
// held, and only after every in-flight call has drained, so no accounting call
// can observe a half-swapped state.
func (m *Manager) apply(raw []byte, hostSchema uint32) ([]byte, error) {
	reg, err := registrationResponse(hostSchema)
	if err != nil {
		return errorEnvelope(err)
	}

	cfg, err := config.Parse(raw)
	if err != nil {
		return errorEnvelope(&PluginError{Code: "invalid_config", Message: "invalid plugin configuration", HTTPStatus: http.StatusBadRequest})
	}

	m.mu.Lock()
	closed := m.state == stateClosed
	unchanged := m.live != nil && m.hasConfig && bytes.Equal(m.raw, raw)
	previousHasConfig := m.hasConfig
	previousRaw := m.raw
	m.mu.Unlock()
	if closed {
		return errorEnvelope(&PluginError{Code: "unavailable", Message: "plugin is shut down", HTTPStatus: http.StatusServiceUnavailable})
	}
	if unchanged {
		// A host-wide synchronization may resend identical configuration.
		return okEnvelope(reg)
	}

	// Refuse new accounting before the exclusive gate blocks the swap.
	m.mu.Lock()
	previous := m.live
	if m.state != stateClosed {
		m.state = stateReconfiguring
	}
	m.mu.Unlock()

	// A store that can report unfinished requests must not be swapped while any
	// are in flight: their rows live in the old database. Keep the old store
	// and let the host retry once the requests settle. A backend that can not
	// answer is treated as unsafe too: accounting is never swapped on a guess.
	ctx := context.Background()
	known, active, errActive := storeActive(ctx, previous)
	if errActive != nil {
		m.recordError(errActive)
	}
	if errActive != nil || (known && active) {
		m.mu.Lock()
		m.state = stateLive
		m.mu.Unlock()
		return errorEnvelope(&PluginError{Code: "busy", Message: "unfinished requests are still being settled", HTTPStatus: http.StatusConflict})
	}

	m.enterExclusive()
	defer m.exitExclusive()

	// Release the current store before opening the candidate so only one
	// connection to the accounting database exists at any time.
	m.stopMaintenance()
	m.closeLive(m.detach())

	store, err := credit.Open(credit.Options{
		DataDir:            cfg.DataDir,
		RetentionMonths:    cfg.RetentionMonths,
		MaxPendingRequests: cfg.MaxPendingRequests,
	})
	if err != nil {
		m.restore(previousRaw, previousHasConfig)
		return errorEnvelope(&PluginError{Code: "storage_unavailable", Message: "credit storage unavailable", HTTPStatus: http.StatusServiceUnavailable})
	}
	// Capture is disposable: a capture failure degrades diagnostics only and
	// must never affect the credit store or inference. Fall back to a disabled
	// store and surface the failure through health instead.
	var captureFault error
	captures, err := capture.Open(cfg.DataDir, cfg.Capture)
	if err != nil {
		captureFault = err
		captures, err = capture.Open("", capture.Config{Enabled: false})
		if err != nil {
			m.closeStore(store)
			m.restore(previousRaw, previousHasConfig)
			return errorEnvelope(&PluginError{Code: "storage_unavailable", Message: "capture storage unavailable", HTTPStatus: http.StatusServiceUnavailable})
		}
	}

	m.mu.Lock()
	m.live = m.newLiveState(store, captures, cfg)
	m.raw = append([]byte(nil), raw...)
	m.hasConfig = true
	m.state = stateLive
	m.lastError = ""
	m.admissionBlocked = false
	m.mu.Unlock()
	if captureFault != nil {
		m.recordError(captureFault)
	}
	m.startMaintenance(store, captures)
	return okEnvelope(reg)
}

// restore best-effort reopens the configuration that was live before a failed
// reconfigure. When it can not be reopened the plugin stays degraded and
// refuses new accounting rather than serving unmetered requests.
func (m *Manager) restore(raw []byte, hasConfig bool) {
	m.mu.Lock()
	m.state = stateQuiesced
	m.mu.Unlock()
	if !hasConfig {
		return
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return
	}
	store, err := credit.Open(credit.Options{
		DataDir:            cfg.DataDir,
		RetentionMonths:    cfg.RetentionMonths,
		MaxPendingRequests: cfg.MaxPendingRequests,
	})
	if err != nil {
		return
	}
	captures, err := capture.Open(cfg.DataDir, cfg.Capture)
	if err != nil {
		captures, err = capture.Open("", capture.Config{Enabled: false})
		if err != nil {
			m.closeStore(store)
			return
		}
	}
	m.mu.Lock()
	m.live = m.newLiveState(store, captures, cfg)
	m.state = stateLive
	m.mu.Unlock()
	m.startMaintenance(store, captures)
}

// newLiveState builds the serving snapshot and connects runtime diagnostics.
func (m *Manager) newLiveState(store credit.Backend, captures *capture.Store, cfg config.Config) *liveState {
	handler := web.New(store, captures, cfg)
	handler.SetStatus(metadataVersion(), m.LastError)
	return &liveState{store: store, captures: captures, handler: handler, cfg: cfg}
}

// activeChecker is optionally implemented by a credit backend that can report
// whether any request is still awaiting settlement.
type activeChecker interface {
	HasActive(context.Context) (bool, error)
}

// storeActive reports whether the given snapshot has unfinished requests. known
// is false when the backend does not implement the optional capability, so
// callers never mistake "cannot tell" for "nothing outstanding".
func storeActive(ctx context.Context, state *liveState) (known bool, active bool, err error) {
	if state == nil || state.store == nil {
		return false, false, nil
	}
	checker, ok := state.store.(activeChecker)
	if !ok {
		return false, false, nil
	}
	active, err = checker.HasActive(ctx)
	return true, active, err
}

// enterExclusive takes the exclusive gate: it blocks every new handler call and
// waits until the in-flight calls finish. Handlers are bounded local store
// calls, so the wait terminates; a lifecycle transition therefore never swaps
// or closes a store under an in-flight call.
func (m *Manager) enterExclusive() {
	m.gate.Lock()
}

// exitExclusive releases the exclusive gate taken by enterExclusive.
func (m *Manager) exitExclusive() {
	m.gate.Unlock()
}

// detach removes the live dependencies under a short lock and returns them.
func (m *Manager) detach() *liveState {
	m.mu.Lock()
	previous := m.live
	m.live = nil
	m.mu.Unlock()
	return previous
}

// closeStore closes one credit backend and surfaces any error: a close failure
// must never be silently discarded.
func (m *Manager) closeStore(store credit.Backend) {
	if store == nil {
		return
	}
	if err := store.Close(); err != nil {
		m.recordError(err)
	}
}

// closeLive releases the stores of a detached snapshot, reporting close failures.
func (m *Manager) closeLive(state *liveState) {
	if state == nil {
		return
	}
	m.closeStore(state.store)
	if state.captures != nil {
		if err := state.captures.Close(); err != nil {
			m.recordError(err)
		}
	}
}

// quiesce refuses new admissions and prepares the plugin to be replaced.
//
// Settling a reserved request is never dropped: in-flight handler calls are
// drained first, and the accounting database is released only when the backend
// can prove it holds no unfinished request. Otherwise the stores stay open so
// late usage and completion events still settle. Shutdown always releases them.
func (m *Manager) quiesce() {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	if m.state == stateClosed {
		m.mu.Unlock()
		return
	}
	m.state = stateQuiesced
	current := m.live
	m.mu.Unlock()

	m.enterExclusive()
	defer m.exitExclusive()
	m.stopMaintenance()

	known, active, err := storeActive(context.Background(), current)
	if err != nil {
		m.recordError(err)
		return
	}
	if !known || active {
		// Cannot prove the database is settled: keep it open so pending
		// requests can still be completed instead of losing accounting.
		return
	}
	m.closeLive(m.detach())
}

// shutdown releases the stores and makes the manager terminal. It is
// idempotent and drains in-flight calls before closing.
func (m *Manager) shutdown() {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	if m.state == stateClosed {
		m.mu.Unlock()
		return
	}
	m.state = stateClosed
	m.mu.Unlock()

	m.enterExclusive()
	defer m.exitExclusive()
	m.stopMaintenance()
	m.closeLive(m.detach())
}

// startMaintenance launches the periodic credit and capture maintenance pass.
// It must only be called while the exclusive gate is held (or before the
// manager is visible to handlers).
func (m *Manager) startMaintenance(store credit.Backend, captures *capture.Store) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	m.maintenanceCancel = cancel
	m.maintenanceDone = done
	m.mu.Unlock()
	go m.maintain(ctx, done, store, captures)
}

// stopMaintenance cancels and waits for the maintenance pass. It must be called
// before the stores are released so maintenance never touches a closed store.
func (m *Manager) stopMaintenance() {
	m.mu.Lock()
	cancel := m.maintenanceCancel
	done := m.maintenanceDone
	m.maintenanceCancel = nil
	m.maintenanceDone = nil
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

// maintain runs the bounded maintenance pass until cancelled. Failures are
// sticky but never stop the proxy: they surface through LastError.
func (m *Manager) maintain(ctx context.Context, done chan struct{}, store credit.Backend, captures *capture.Store) {
	defer close(done)
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if store != nil {
				if _, err := store.Cleanup(ctx); err != nil {
					m.recordError(err)
				}
			}
			if captures != nil {
				if err := captures.Cleanup(); err != nil {
					m.recordError(err)
				}
			}
		}
	}
}
