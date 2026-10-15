package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/plugin/accounting"
)

// errOrphanUsage marks a usage record that belongs to this plugin but can not
// be attributed to a reserved request. It is surfaced through health instead of
// being guessed into an accounting row.
var errOrphanUsage = errors.New("usage record cannot be attributed to a reserved request")

// errInternalSource marks a usage record produced by an internal host path such
// as a plugin model callback. It must never be counted as customer usage.
var errInternalSource = errors.New("internal usage source ignored")

// handleUsage records one completed execution. Usage delivery is best effort on
// the host side, so a malformed payload is acknowledged and reported through
// health rather than failing the host call.
func (m *Manager) handleUsage(request []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if len(request) > 0 {
		if err := json.Unmarshal(request, &record); err != nil {
			m.recordError(errOrphanUsage)
			return okEnvelope(struct{}{})
		}
	}
	m.recordUsage(record)
	return okEnvelope(struct{}{})
}

// recordUsage attributes one usage record to a reserved request and settles it
// idempotently. Only records produced by this plugin's own frontend auth are
// considered customer usage.
func (m *Manager) recordUsage(record pluginapi.UsageRecord) {
	if !isOwnAccessProvider(record.AccessProvider) {
		return
	}
	if isInternalUsageSource(record.Source) {
		m.recordError(errInternalSource)
		return
	}
	principal := strings.TrimSpace(record.APIKey)
	// record.RequestID identifies one model execution, not the lifecycle request
	// that reserved money; the lifecycle request is found through the trace. The
	// two must never be conflated: the core rejects a usage whose request id does
	// not belong to the resolved request.
	executionID := strings.TrimSpace(record.RequestID)
	traceID := strings.TrimSpace(record.TraceID)
	if principal == "" || executionID == "" || traceID == "" {
		m.recordError(errOrphanUsage)
		return
	}

	deps, ok := m.begin(false)
	if !ok {
		// Quiesced or shut down: the pending request stays pending in durable
		// storage instead of being settled against nothing.
		m.recordError(errOrphanUsage)
		return
	}
	defer m.end()

	ctx := context.Background()
	key, err := deps.store.KeyByPrincipal(ctx, principal)
	if err != nil {
		m.reportUsageFailure(err, "unknown usage principal")
		return
	}

	normalized := accounting.Normalize(record)
	at := record.RequestedAt
	if at.IsZero() {
		at = m.now()
	}
	if errRecord := deps.store.RecordUsage(ctx, credit.Usage{
		ID:            executionID,
		TraceID:       traceID,
		KeyID:         key.ID,
		Provider:      strings.TrimSpace(record.Provider),
		AuthID:        strings.TrimSpace(record.AuthID),
		AuthIndex:     strings.TrimSpace(record.AuthIndex),
		Executor:      strings.TrimSpace(record.ExecutorType),
		Model:         strings.TrimSpace(record.Model),
		ResponseModel: strings.TrimSpace(record.ResponseModel),
		Raw:           record.Detail,
		Tokens:        normalized.Tokens,
		Quality:       normalized.Quality,
		Failed:        record.Failed,
		StatusCode:    record.Failure.StatusCode,
		At:            at,
		LatencyMS:     record.Latency.Milliseconds(),
		TTFTMS:        record.TTFT.Milliseconds(),
	}); errRecord != nil {
		m.reportUsageFailure(errRecord, "usage for key "+key.ID)
	}
}

// reportUsageFailure decides how a usage attribution failure is surfaced.
//
// Only business level rejections are reported through health: they describe one
// record (an orphan, a malformed payload, a duplicate that must be reviewed)
// and must not stop other keys. A storage or transaction fault must fail closed
// instead, because accounting can no longer be trusted for any key.
func (m *Manager) reportUsageFailure(err error, detail string) {
	switch {
	case errors.Is(err, credit.ErrNotFound),
		errors.Is(err, credit.ErrUnauthorized),
		errors.Is(err, credit.ErrDenied),
		errors.Is(err, credit.ErrInvalid),
		errors.Is(err, credit.ErrConflict):
		// Health only: the pending request stays pending for an administrator to
		// review, and other keys keep working.
		m.recordError(fmt.Errorf("%s: %w", detail, err))
	default:
		m.recordFinanceFault(err)
	}
}

// isInternalUsageSource reports whether a record was produced by an internal
// host path (for example a plugin model callback) rather than a customer
// request, so it is never attached to a customer key.
func isInternalUsageSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "internal", "warmup", "plugin_host_model_callback":
		return true
	default:
		return false
	}
}
