package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/capture"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

const (
	// countTokensSuffix matches the Claude count-tokens endpoint.
	countTokensSuffix = "/count_tokens"
	// countTokensMarker matches the Gemini :countTokens method suffix.
	countTokensMarker = ":countTokens"
)

// handleInterceptBefore admits one request for accounting.
//
// Every failure path returns a successful envelope carrying Terminate so the
// host stops the request instead of failing open, and any panic is converted to
// the same fail-closed response: a credit gate must never let an unmetered
// request through because its own hook failed.
func (m *Manager) handleInterceptBefore(request []byte) (resp []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			raw, _ := okEnvelope(terminate(http.StatusServiceUnavailable, "credit manager is unavailable"))
			resp, err = raw, nil
		}
	}()
	var req pluginapi.RequestInterceptRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return okEnvelope(terminate(http.StatusServiceUnavailable, "credit manager is unavailable"))
		}
	}
	return okEnvelope(m.interceptBefore(req))
}

// handleInterceptAfter is a pure no-op: the plugin never rewrites an after-auth
// request, and a panic must still fail closed.
func (m *Manager) handleInterceptAfter(request []byte) (resp []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			raw, _ := okEnvelope(terminate(http.StatusServiceUnavailable, "credit manager is unavailable"))
			resp, err = raw, nil
		}
	}()
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

// interceptBefore resolves the caller identity, admits the request for
// accounting, and enqueues a diagnostic snapshot.
func (m *Manager) interceptBefore(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	deps, ok := m.begin(true)
	if !ok {
		// Not configured, quiesced or shut down: refuse new accounting instead
		// of admitting unmetered traffic.
		return terminate(http.StatusServiceUnavailable, "credit manager is unavailable")
	}
	defer m.end()

	scope := metadataString(req.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
	if scope == "" {
		return terminate(http.StatusUnauthorized, "caller identity is required")
	}
	ctx := context.Background()
	key, err := deps.store.KeyByScope(ctx, scope)
	if err != nil {
		// An unknown caller is a per-request rejection. A genuine storage or
		// permission failure is reported through health, which never gates
		// other keys: the next admission validates its own storage state.
		if !errors.Is(err, credit.ErrNotFound) && !errors.Is(err, credit.ErrUnauthorized) {
			m.recordError(err)
		}
		return terminate(http.StatusUnauthorized, "unknown caller identity")
	}

	// The current CPA contract always populates RequestedModel with the
	// client-requested id, which is the id this plugin matches against model
	// allow lists and records as the request model. Costs are derived later
	// from the exact usage model, so falling back to the resolved upstream
	// Model here would silently grant the wrong model, and a request without a
	// requested model is refused before any accounting row is created.
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		return terminate(http.StatusBadRequest, "requested model is required")
	}
	if isCountTokens(req) {
		// Native token counting is not billed and creates no accounting row,
		// but it must not bypass key permissions.
		if errPermit := permitCountTokens(key, model, m.now()); errPermit != nil {
			return terminate(http.StatusForbidden, errPermit.Error())
		}
		return pluginapi.RequestInterceptResponse{}
	}

	traceID := strings.TrimSpace(req.TraceID)
	if traceID == "" {
		// Without a trace the execution can never be correlated with its
		// lifecycle completion, so it must not be admitted for billing.
		return terminate(http.StatusBadRequest, "request trace is required")
	}
	// Nothing here estimates amount or tokens: admission checks the actual
	// token counters and key concurrency only, so a very large declared output
	// limit or input payload is still admitted.
	if _, errAdmit := deps.store.Admit(ctx, credit.AdmitInput{
		ID:             strings.TrimSpace(req.RequestID),
		TraceID:        traceID,
		KeyID:          key.ID,
		RequestedModel: model,
		ServiceTier:    requestServiceTier(req.Body),
		At:             m.now(),
	}); errAdmit != nil {
		if !isBusinessRejection(errAdmit) {
			// A storage or transaction failure refuses this request and is
			// reported through health. It never latches a global gate, so one
			// transient failure can not permanently block every key.
			m.recordError(errAdmit)
		}
		return terminate(admitStatus(errAdmit), admitMessage(errAdmit))
	}

	if deps.captures != nil {
		deps.captures.Enqueue(capture.Snapshot{
			RequestID: strings.TrimSpace(req.RequestID),
			Headers:   req.Headers,
			Body:      req.Body,
			At:        m.now(),
		})
	}
	return pluginapi.RequestInterceptResponse{}
}

// permitCountTokens enforces the cheap key permissions that must also apply to
// native token counting: enabled, unexpired, and an exactly allowed model.
func permitCountTokens(key credit.Key, model string, now time.Time) error {
	if !key.Spec.Enabled {
		return errors.New("key is disabled")
	}
	if key.Spec.ExpiresAt != nil && !now.Before(*key.Spec.ExpiresAt) {
		return errors.New("key has expired")
	}
	if !modelAllowed(key.Spec.AllowedModels, model) {
		return errors.New("model is not allowed for this key")
	}
	return nil
}

// modelAllowed applies the exact-model allow list contract: an empty list means
// every model, otherwise the requested id must match exactly.
func modelAllowed(allowed []string, model string) bool {
	if len(allowed) == 0 {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, candidate := range allowed {
		if strings.TrimSpace(candidate) == model {
			return true
		}
	}
	return false
}

// isCountTokens reports whether the execution is a native token-count request,
// identified from the CPA request-path metadata.
func isCountTokens(req pluginapi.RequestInterceptRequest) bool {
	path := metadataString(req.Metadata, cliproxyexecutor.RequestPathMetadataKey)
	if path == "" {
		return false
	}
	return strings.HasSuffix(path, countTokensSuffix) || strings.HasSuffix(path, countTokensMarker)
}

// metadataString reads a string value from the read-only metadata snapshot.
func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// requestServiceTier reads the requested service tier from the raw request body
// without modifying it. Both the native service_tier key and the shorter tier
// key are accepted; an absent or non-string value reads as empty.
func requestServiceTier(body []byte) string {
	var fields map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &fields) != nil {
		return ""
	}
	for _, name := range []string{"service_tier", "tier"} {
		raw, ok := fields[name]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// isBusinessRejection reports whether a credit failure refuses one request
// because of that request, its key or its model. Those are normal outcomes: the
// caller gets an error response while every other key keeps working. Only an
// unrecognized failure indicates that the accounting store itself is broken,
// which is reported through health without latching a global admission gate.
//
// A conflicting request id is included: Admit rejects a duplicate or
// contradictory event for one request with 409, which must not be escalated
// into a global admission block for every key.
func isBusinessRejection(err error) bool {
	switch {
	case errors.Is(err, credit.ErrQuota),
		errors.Is(err, credit.ErrConcurrency),
		errors.Is(err, credit.ErrUnauthorized),
		errors.Is(err, credit.ErrDenied),
		errors.Is(err, credit.ErrConflict),
		errors.Is(err, credit.ErrInvalid):
		return true
	default:
		return false
	}
}

// admitStatus maps an admission failure to a downstream HTTP status.
func admitStatus(err error) int {
	switch {
	case errors.Is(err, credit.ErrQuota), errors.Is(err, credit.ErrConcurrency):
		return http.StatusTooManyRequests
	case errors.Is(err, credit.ErrUnauthorized), errors.Is(err, credit.ErrDenied):
		return http.StatusForbidden
	case errors.Is(err, credit.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, credit.ErrInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusServiceUnavailable
	}
}

// admitMessage returns a safe, non-identifying message for an admission
// failure. No database path, key material or model detail is ever exposed.
func admitMessage(err error) string {
	switch {
	case errors.Is(err, credit.ErrQuota):
		return "credit quota exceeded"
	case errors.Is(err, credit.ErrConcurrency):
		return "too many concurrent requests"
	case errors.Is(err, credit.ErrUnauthorized), errors.Is(err, credit.ErrDenied):
		return "request is not permitted"
	case errors.Is(err, credit.ErrConflict):
		return "conflicting request"
	default:
		return "credit manager is unavailable"
	}
}

// terminate builds a fail-closed interceptor response that stops the request.
func terminate(status int, message string) pluginapi.RequestInterceptResponse {
	body, err := json.Marshal(map[string]any{
		"error": map[string]string{"message": message, "type": terminationErrorType(status)},
	})
	if err != nil {
		body = []byte(`{"error":{"message":"request refused","type":"api_error"}}`)
	}
	return pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      status,
		ResponseHeaders: http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		ResponseBody:    body,
	}
}

// FailClosedInterceptEnvelope is the ABI-level answer for a before-auth
// interception the plugin could not even process: an oversized request or a
// panic at the CGO boundary. It carries the same fail-closed Terminate the hook
// itself returns for that method, because an error envelope or a non-zero
// return code would make the host continue the request unmetered.
func FailClosedInterceptEnvelope() []byte {
	raw, err := okEnvelope(terminate(http.StatusServiceUnavailable, "credit manager is unavailable"))
	if err != nil {
		return []byte(`{"ok":true,"result":{"terminate":true,"status_code":503}}`)
	}
	return raw
}

// terminationErrorType maps a status to the error type shared by the OpenAI and
// Anthropic error envelopes.
func terminationErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}
