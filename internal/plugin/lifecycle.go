package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

// handleRequestComplete records one terminal request event.
func (m *Manager) handleRequestComplete(request []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if len(request) > 0 {
		if err := json.Unmarshal(request, &completion); err != nil {
			return errorEnvelope(&PluginError{Code: "invalid_request", Message: "malformed request completion"})
		}
	}
	m.complete(completion)
	return okEnvelope(struct{}{})
}

// complete settles the terminal state of a reserved request. The native
// outcome is preserved verbatim: succeeded, failed, rejected and canceled are
// four distinct states and are never collapsed into one another.
func (m *Manager) complete(completion pluginapi.RequestCompletion) {
	id := strings.TrimSpace(completion.RequestID)
	if id == "" {
		return
	}
	deps, ok := m.begin(false)
	if !ok {
		// Quiesced or shut down: the request keeps its durable pending state
		// instead of being faked into a settlement.
		return
	}
	defer m.end()

	at := completion.CompletedAt
	if at.IsZero() {
		at = m.now()
	}
	err := deps.store.Complete(context.Background(), credit.Completion{
		ID:         id,
		TraceID:    strings.TrimSpace(completion.TraceID),
		Outcome:    completionOutcome(completion.Outcome),
		StatusCode: completion.StatusCode,
		At:         at,
	})
	switch {
	case err == nil:
		return
	case errors.Is(err, credit.ErrNotFound):
		// No associated request: the execution was a native token-count probe
		// (never reserved), or its history was purged. An unknown old event may
		// not resurrect a request and must not fail the plugin closed.
		return
	default:
		// A finance failure blocks new admissions so the plugin fails closed.
		m.recordFinanceFault(err)
	}
}

// completionOutcome maps the native terminal states onto the persisted ledger
// outcome without collapsing distinct states.
func completionOutcome(outcome pluginapi.RequestCompletionOutcome) string {
	switch outcome {
	case pluginapi.RequestCompletionSucceeded:
		return "succeeded"
	case pluginapi.RequestCompletionFailed:
		return "failed"
	case pluginapi.RequestCompletionRejected:
		return "rejected"
	case pluginapi.RequestCompletionCanceled:
		return "canceled"
	default:
		return strings.TrimSpace(string(outcome))
	}
}
