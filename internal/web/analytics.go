package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// AnalyticsOverview is the management route suffix of the whole-selection
// overview API. It is registered with the other management routes by the plugin
// adapter and dispatched here; it is not a public resource route.
const AnalyticsOverview = "/api/overview"

// analyticsOverview is the narrow, optional capability the overview needs from
// the ledger. It is asserted on the shared Backend rather than added to
// credit.Backend, so the common contract stays unchanged and a backend that
// cannot aggregate the whole selection simply refuses instead of serving a
// paged or partial overview. It mirrors requestLookup.
type analyticsOverview interface {
	Analytics(ctx context.Context, f credit.Filter) (credit.Analytics, error)
}

// HandleAnalytics serves GET /api/overview: the full-dataset summary, trend and
// per-model distribution for one filter. It is management-only by construction:
// the plugin adapter only dispatches management routes below the authenticated
// BasePath, and the prefix is re-checked here so an accidental public wiring
// cannot expose every key's financial overview.
//
// The filter is parsed with exactly the usage route's vocabulary and range
// defaults, so the overview and the paged usage report always describe the same
// selection. Paging is not read: the overview aggregates the whole matched set,
// never one page of it.
func (h *Handler) HandleAnalytics(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if req.Method != http.MethodGet {
		return response(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
	if !strings.HasPrefix(req.Path, BasePath+"/api/") {
		return response(http.StatusNotFound, map[string]string{"error": "not found"})
	}
	filter, err := overviewFilter(req.Query)
	if err != nil {
		return failure(err)
	}
	overview, ok := h.db.(analyticsOverview)
	if !ok {
		return response(http.StatusServiceUnavailable, map[string]string{"error": "overview unavailable"})
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := overview.Analytics(ctx, filter)
	if err != nil {
		return failure(err)
	}
	return response(http.StatusOK, result)
}

// overviewFilter parses the overview query string into a credit.Filter. The
// selectors, timestamp format and range defaults match the usage route exactly;
// malformed timestamps are rejected as invalid, never silently ignored. Page
// and page_size are deliberately not parsed because the overview never pages.
func overviewFilter(query url.Values) (credit.Filter, error) {
	q := nonNilValues(query)
	filter := credit.Filter{
		KeyID:    q.Get("key_id"),
		Model:    q.Get("model"),
		Provider: q.Get("provider"),
		AuthID:   q.Get("auth_id"),
		Result:   q.Get("result"),
		Search:   q.Get("search"),
		Bucket:   q.Get("bucket"),
	}
	var err error
	if raw := q.Get("from"); raw != "" {
		if filter.From, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			return credit.Filter{}, credit.ErrInvalid
		}
	}
	if raw := q.Get("to"); raw != "" {
		if filter.To, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			return credit.Filter{}, credit.ErrInvalid
		}
	}
	return filter, nil
}
