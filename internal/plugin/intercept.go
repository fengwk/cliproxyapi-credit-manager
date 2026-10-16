package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/capture"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

const (
	// defaultOutputEstimate is the conservative output reservation used when the
	// request declares no maximum token field.
	defaultOutputEstimate = 4096
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

// interceptBefore resolves the caller identity, admits the request with a
// bounded reservation, and enqueues a diagnostic snapshot.
func (m *Manager) interceptBefore(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	deps, ok := m.begin(true)
	if !ok {
		// Not configured, quiesced, shut down, or failed closed after a finance
		// fault: refuse new accounting instead of admitting unmetered traffic.
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
		if !errors.Is(err, credit.ErrNotFound) && !errors.Is(err, credit.ErrUnauthorized) {
			m.recordFinanceFault(err)
		}
		return terminate(http.StatusUnauthorized, "unknown caller identity")
	}

	model := requestedModel(req)
	if isCountTokens(req) {
		// Native token counting is not billed and holds no reservation, but it
		// must not bypass key permissions.
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
	// The requested image count is only a billing input for a per-image price;
	// the ledger decides whether an unsupported declared value is refused.
	imageCount, imageCountDeclared := requestImageCount(req.Body)
	if _, errReserve := deps.store.Reserve(ctx, credit.ReserveInput{
		ID:                 strings.TrimSpace(req.RequestID),
		TraceID:            traceID,
		KeyID:              key.ID,
		RequestedModel:     model,
		InputEstimate:      int64(len(req.Body)),
		OutputEstimate:     outputEstimate(req.Body),
		ServiceTier:        requestServiceTier(req.Body),
		ImageCount:         imageCount,
		ImageCountDeclared: imageCountDeclared,
		At:                 m.now(),
	}); errReserve != nil {
		if !isBusinessRejection(errReserve) {
			// An unrecognized storage or transaction failure means admission can
			// no longer be trusted, so it fails closed for every key.
			m.recordFinanceFault(errReserve)
		}
		return terminate(reserveStatus(errReserve), reserveMessage(errReserve))
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

// requestedModel prefers the client-requested model, which is the id used for
// pricing, model allow lists and token limits.
func requestedModel(req pluginapi.RequestInterceptRequest) string {
	if model := strings.TrimSpace(req.RequestedModel); model != "" {
		return model
	}
	return strings.TrimSpace(req.Model)
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

// outputEstimate reads the declared maximum output tokens from the raw request
// body without modifying it, falling back to a conservative constant.
func outputEstimate(body []byte) int64 {
	if len(body) == 0 {
		return defaultOutputEstimate
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return defaultOutputEstimate
	}
	for _, name := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		raw, ok := fields[name]
		if !ok {
			continue
		}
		var value int64
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		if value > 0 {
			return value
		}
	}
	return defaultOutputEstimate
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

// requestImageCount reads the requested image count from the raw request body
// without modifying it. It returns the declared count and whether the request
// declared one at all.
//
// The value is the count the operator asked for, not the number of images an
// upstream actually returned (the plugin never inspects the response). It is
// never clamped: the ledger refuses an unsupported count for a per-image price
// so a request can not be billed for a different number of images than it
// asked for. A present but unusable value (non-integer, fractional, or outside
// the int64 range) reports declared with a zero count, which the ledger refuses
// for a per-image price. A value that is irrelevant to the billing mode, such
// as the sampling hint on a token request, never causes a rejection.
func requestImageCount(body []byte) (int64, bool) {
	if len(body) == 0 {
		return 0, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return 0, false
	}
	for _, name := range []string{"n", "n_images", "num_images", "image_count"} {
		raw, ok := fields[name]
		if !ok {
			continue
		}
		value, ok := integralInt64(raw)
		if !ok {
			return 0, true
		}
		return value, true
	}
	return 0, false
}

// integralInt64 decodes a JSON number that denotes an exact integer within the
// int64 range. A string, fractional, non-numeric or out-of-range value is
// rejected so the caller can fail closed instead of inventing a billable count.
// Exponent and trailing-zero forms are accepted only when they denote an exact
// integer (2.0 and 1e2 are integers, 2.5 and 1e30 are not).
func integralInt64(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 64 {
		return 0, false
	}
	// Bound exponent expansion before big.Rat sees untrusted request numbers.
	if i := bytes.IndexAny(raw, "eE"); i >= 0 {
		exponent, err := strconv.ParseInt(string(raw[i+1:]), 10, 16)
		if err != nil || exponent < -64 || exponent > 64 {
			return 0, false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return 0, false
	}
	number, ok := decoded.(json.Number)
	if !ok {
		return 0, false
	}
	if value, err := number.Int64(); err == nil {
		return value, true
	}
	rational, ok := new(big.Rat).SetString(number.String())
	if !ok || !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, false
	}
	return rational.Num().Int64(), true
}

// isBusinessRejection reports whether a credit failure refuses one request
// because of that request, its key or its model. Those are normal outcomes: the
// caller gets an error response while every other key keeps working. Only an
// unrecognized failure indicates that the accounting store itself is broken.
//
// A conflicting request id is included: Resolve/Reserve reject a duplicate or
// contradictory event for one request with 409, which must not be escalated
// into a global admission block for every key.
func isBusinessRejection(err error) bool {
	switch {
	case errors.Is(err, credit.ErrQuota),
		errors.Is(err, credit.ErrConcurrency),
		errors.Is(err, credit.ErrPending),
		errors.Is(err, credit.ErrUnauthorized),
		errors.Is(err, credit.ErrDenied),
		errors.Is(err, credit.ErrUnpriced),
		errors.Is(err, credit.ErrConflict),
		errors.Is(err, credit.ErrInvalid):
		return true
	default:
		return false
	}
}

// reserveStatus maps a reservation failure to a downstream HTTP status.
func reserveStatus(err error) int {
	switch {
	case errors.Is(err, credit.ErrQuota), errors.Is(err, credit.ErrConcurrency), errors.Is(err, credit.ErrPending):
		return http.StatusTooManyRequests
	case errors.Is(err, credit.ErrUnauthorized), errors.Is(err, credit.ErrDenied), errors.Is(err, credit.ErrUnpriced):
		return http.StatusForbidden
	case errors.Is(err, credit.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, credit.ErrInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusServiceUnavailable
	}
}

// reserveMessage returns a safe, non-identifying message for a reservation
// failure. No database path, key material or model detail is ever exposed.
func reserveMessage(err error) string {
	switch {
	case errors.Is(err, credit.ErrQuota):
		return "credit quota exceeded"
	case errors.Is(err, credit.ErrConcurrency):
		return "too many concurrent requests"
	case errors.Is(err, credit.ErrPending):
		return "too many unresolved requests"
	case errors.Is(err, credit.ErrUnauthorized), errors.Is(err, credit.ErrDenied):
		return "request is not permitted"
	case errors.Is(err, credit.ErrUnpriced):
		return "model is not available"
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
