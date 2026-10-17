package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/capture"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Public resource route names of the unauthenticated customer self-service
// endpoints. They live under the plugin resource base, so they are reached
// without host management authentication and must authenticate the customer's
// own ledger key themselves.
const (
	PublicLookupData    = "lookup/data"
	PublicLookupCapture = "lookup/capture"
)

// AssetNames returns every exact embedded asset path this package may serve, in
// a stable order. The plugin adapter derives its declared public resources from
// this list, so the served set and the registered set can never drift apart.
func AssetNames() []string {
	names := make([]string, 0, len(assetMIMEs))
	for name := range assetMIMEs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// requestLookup is the narrow, optional capability the snapshot lookup needs
// from the ledger. It is asserted on the shared Backend rather than added to
// credit.Backend, so the common contract stays unchanged and a backend that
// cannot prove ownership simply refuses the lookup instead of exposing a
// request.
type requestLookup interface {
	LookupRequest(ctx context.Context, keyID, requestID string) (credit.Request, error)
}

type customerReport interface {
	LookupReport(context.Context, credit.Filter) (credit.Report, error)
}

// lookupKey is the sanitized self-service view of one key. It deliberately
// carries display budgets and limits only: the principal, scope, fingerprint
// and any key material never leave the store, and no other key is ever
// described.
type lookupKey struct {
	ID               string                   `json:"id"`
	Label            string                   `json:"label"`
	Enabled          bool                     `json:"enabled"`
	QuotaMicroUSD    int64                    `json:"quota_micro_usd"`
	DailyMicroUSD    int64                    `json:"daily_micro_usd"`
	WeeklyMicroUSD   int64                    `json:"weekly_micro_usd"`
	MonthlyMicroUSD  int64                    `json:"monthly_micro_usd"`
	MaxConcurrent    int64                    `json:"max_concurrent"`
	AllowedModels    []string                 `json:"allowed_models,omitempty"`
	ModelTokenLimits []credit.ModelTokenLimit `json:"model_token_limits,omitempty"`
	ExpiresAt        *time.Time               `json:"expires_at,omitempty"`
	CreatedAt        time.Time                `json:"created_at"`
}

// lookupExecution is the sanitized execution view. The upstream credential
// placement (auth id/index) and the executor name identify operator-side
// accounts, so they are omitted; only data about the customer's own call is
// returned.
type lookupExecution struct {
	ID            string        `json:"id"`
	Model         string        `json:"model"`
	ResponseModel string        `json:"response_model,omitempty"`
	Provider      string        `json:"provider,omitempty"`
	Tokens        credit.Tokens `json:"tokens"`
	Quality       string        `json:"quality"`
	// CostMicroUSD is derived from the current price book at read time. It is
	// explicitly null when the execution is not priced, so "free" and "unknown"
	// never collapse into the same zero.
	CostMicroUSD *int64    `json:"cost_micro_usd"`
	Failed       bool      `json:"failed"`
	StatusCode   int       `json:"status_code,omitempty"`
	At           time.Time `json:"at"`
	LatencyMS    int64     `json:"latency_ms,omitempty"`
	TTFTMS       int64     `json:"ttft_ms,omitempty"`
	ServiceTier  string    `json:"service_tier,omitempty"`
}

// lookupRequest is the sanitized request view. It never groups requests of
// other keys and drops the raw upstream usage payload. It carries no frozen
// price, amount or image count: cost is read only from each execution.
type lookupRequest struct {
	ID          string            `json:"id"`
	TraceID     string            `json:"trace_id"`
	Model       string            `json:"model"`
	Outcome     string            `json:"outcome"`
	State       string            `json:"state"`
	StartedAt   time.Time         `json:"started_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Executions  []lookupExecution `json:"executions"`
	ServiceTier string            `json:"service_tier,omitempty"`
}

// lookupData is the self-service usage response: the authenticated key's own
// quota summary plus its own summary, trend and paged request list.
type lookupData struct {
	Key           lookupKey             `json:"key"`
	Summary       credit.Summary        `json:"summary"`
	Trend         []credit.Point        `json:"trend"`
	Items         []lookupRequest       `json:"items"`
	Total         int64                 `json:"total"`
	Page          int                   `json:"page"`
	PageSize      int                   `json:"page_size"`
	RetentionFrom time.Time             `json:"retention_from"`
	Models        []credit.ModelMetrics `json:"models"`
}

// HandleLookup serves one unauthenticated customer self-service request.
//
// action is the resource-relative route (PublicLookupData or
// PublicLookupCapture). Every route requires an explicit "Authorization:
// Bearer <customer key>" header that is validated against the ledger with the
// same Authenticate path as inference, so a disabled, expired or rotated key is
// denied. The customer key is never read from the URL, no management key is
// accepted, nothing is forwarded upstream, and no lookup bills usage.
func (h *Handler) HandleLookup(action string, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if req.Method != http.MethodGet {
		return response(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key, ok := h.authenticateCustomer(ctx, req.Headers.Get("Authorization"))
	if !ok {
		return response(http.StatusUnauthorized, map[string]string{"error": "invalid or inactive key"})
	}
	switch action {
	case PublicLookupData:
		return h.lookupUsage(ctx, key, req.Query)
	case PublicLookupCapture:
		return h.lookupCapture(ctx, key, req.Query)
	default:
		return response(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// authenticateCustomer resolves the explicit Bearer customer key through the
// ledger. A bare token or any non-Bearer scheme is rejected, and the error is
// collapsed to a single unauthenticated result so a caller cannot tell a
// disabled key from an expired, rotated or unknown one.
func (h *Handler) authenticateCustomer(ctx context.Context, authorization string) (credit.Key, bool) {
	material := bearerMaterial(authorization)
	if material == "" {
		return credit.Key{}, false
	}
	key, err := h.db.Authenticate(ctx, material)
	if err != nil {
		return credit.Key{}, false
	}
	return key, true
}

// lookupUsage returns the authenticated key's own summary, trend and request
// page. The filter always forces KeyID to the authenticated key, so any
// attacker-supplied key_id, auth_id or provider selector is ignored rather than
// widening the query to another key.
func (h *Handler) lookupUsage(ctx context.Context, key credit.Key, query url.Values) pluginapi.ManagementResponse {
	query = nonNilValues(query)
	filter := credit.Filter{
		KeyID:  key.ID,
		Model:  query.Get("model"),
		Result: query.Get("result"),
		Search: query.Get("search"),
		Bucket: query.Get("bucket"),
	}
	var err error
	if raw := query.Get("from"); raw != "" {
		if filter.From, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			return failure(credit.ErrInvalid)
		}
	}
	if raw := query.Get("to"); raw != "" {
		if filter.To, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			return failure(credit.ErrInvalid)
		}
	}
	filter.Page, _ = strconv.Atoi(query.Get("page"))
	filter.PageSize, _ = strconv.Atoi(query.Get("page_size"))
	// GetKey hydrates effective quota counters for display; Authenticate
	// intentionally avoids these management-only queries on the request path.
	key, err = h.db.GetKey(ctx, key.ID)
	if err != nil {
		return failure(err)
	}
	var report credit.Report
	if source, ok := h.db.(customerReport); ok {
		report, err = source.LookupReport(ctx, filter)
	} else {
		report, err = h.db.Report(ctx, filter)
	}
	if err != nil {
		return failure(err)
	}
	items := make([]lookupRequest, 0, len(report.Items))
	for _, item := range report.Items {
		items = append(items, sanitizeLookupRequest(item))
	}
	return response(http.StatusOK, lookupData{
		Key:           sanitizeLookupKey(key),
		Summary:       report.Summary,
		Trend:         report.Trend,
		Items:         items,
		Total:         report.Total,
		Page:          report.Page,
		PageSize:      report.PageSize,
		RetentionFrom: report.RetentionFrom,
		Models:        report.Models,
	})
}

// lookupCapture returns the captured snapshot of one request, but only after a
// dedicated ownership query proves the request belongs to the authenticated
// key. A foreign id, an unknown id and an unavailable snapshot are all reported
// as the same generic 404, so the endpoint is never an existence oracle across
// keys.
func (h *Handler) lookupCapture(ctx context.Context, key credit.Key, query url.Values) pluginapi.ManagementResponse {
	id := strings.TrimSpace(nonNilValues(query).Get("id"))
	if id == "" {
		return failure(credit.ErrInvalid)
	}
	lookup, ok := h.db.(requestLookup)
	if !ok {
		return response(http.StatusServiceUnavailable, map[string]string{"error": "snapshot lookup unavailable"})
	}
	if _, err := lookup.LookupRequest(ctx, key.ID, id); err != nil {
		if errors.Is(err, credit.ErrNotFound) || errors.Is(err, credit.ErrUnauthorized) {
			return response(http.StatusNotFound, map[string]string{"error": "not found"})
		}
		return failure(err)
	}
	detail, err := h.captures.Get(id)
	if err != nil {
		return failure(err)
	}
	if detail.Status == capture.StatusUnavailable {
		return response(http.StatusNotFound, map[string]string{"error": "not found"})
	}
	return response(http.StatusOK, detail)
}

// sanitizeLookupKey projects a ledger key onto its public self-service shape.
func sanitizeLookupKey(key credit.Key) lookupKey {
	return lookupKey{
		ID:               key.ID,
		Label:            key.Spec.Label,
		Enabled:          key.Spec.Enabled,
		QuotaMicroUSD:    key.Spec.QuotaMicroUSD,
		DailyMicroUSD:    key.Spec.DailyMicroUSD,
		WeeklyMicroUSD:   key.Spec.WeeklyMicroUSD,
		MonthlyMicroUSD:  key.Spec.MonthlyMicroUSD,
		MaxConcurrent:    key.Spec.MaxConcurrent,
		AllowedModels:    key.Spec.AllowedModels,
		ModelTokenLimits: key.Spec.ModelTokenLimits,
		ExpiresAt:        key.Spec.ExpiresAt,
		CreatedAt:        key.CreatedAt,
	}
}

// sanitizeLookupRequest projects a ledger request onto its public shape.
func sanitizeLookupRequest(item credit.Request) lookupRequest {
	executions := make([]lookupExecution, 0, len(item.Executions))
	for _, execution := range item.Executions {
		executions = append(executions, lookupExecution{
			ID:            execution.ID,
			Model:         execution.Model,
			ResponseModel: execution.ResponseModel,
			Provider:      execution.Provider,
			Tokens:        execution.Tokens,
			Quality:       execution.Quality,
			CostMicroUSD:  execution.CostMicroUSD,
			Failed:        execution.Failed,
			StatusCode:    execution.StatusCode,
			At:            execution.At,
			LatencyMS:     execution.LatencyMS,
			TTFTMS:        execution.TTFTMS,
			ServiceTier:   execution.ServiceTier,
		})
	}
	return lookupRequest{
		ID:          item.ID,
		TraceID:     item.TraceID,
		Model:       item.Model,
		Outcome:     item.Outcome,
		State:       item.State,
		StartedAt:   item.StartedAt,
		CompletedAt: item.CompletedAt,
		Executions:  executions,
		ServiceTier: item.ServiceTier,
	}
}

// bearerMaterial parses an explicit "Bearer <token>" Authorization header. It
// returns an empty string for a missing header, a bare token or any other
// scheme so the customer key is only ever taken from a clearly labelled Bearer
// credential.
func bearerMaterial(header string) string {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// nonNilValues makes an absent query safe to read.
func nonNilValues(query url.Values) url.Values {
	if query == nil {
		return url.Values{}
	}
	return query
}
