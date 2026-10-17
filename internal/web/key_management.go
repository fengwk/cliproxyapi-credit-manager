package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// keyManagement is the optional management capability the ledger may expose.
// It is asserted on the shared Backend instead of widening credit.Backend, so a
// backend that cannot manage keys renders the routes unavailable (503) rather
// than breaking the common contract.
type keyManagement interface {
	RevealKey(ctx context.Context, id string) (string, error)
	DeleteKey(ctx context.Context, id, reason string) error
	ResetTokens(ctx context.Context, spec credit.ResetSpec) (credit.ResetResult, error)
	ListCallers(ctx context.Context) ([]credit.Caller, error)
	CreateCaller(ctx context.Context, spec credit.CallerSpec) (credit.Caller, error)
	UpdateCaller(ctx context.Context, spec credit.CallerSpec) (credit.Caller, error)
	Audit(ctx context.Context, f credit.AuditFilter) (credit.AuditPage, error)
}

// HandleKeyManagement serves the key management, caller and audit routes. It is
// invoked by Handler.Handle only after the shared management gate (bearer
// authorization, cross-site and content-type checks) already accepted the
// request, so it never re-implements authentication and must not be registered
// on a public resource path.
func (h *Handler) HandleKeyManagement(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	km, ok := h.db.(keyManagement)
	if !ok {
		return response(http.StatusServiceUnavailable, map[string]string{"error": "key management unavailable"})
	}
	switch req.Method + " " + strings.TrimPrefix(req.Path, BasePath) {
	case "POST /api/reveal":
		var in struct {
			ID string `json:"id"`
		}
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		material, err := km.RevealKey(ctx, in.ID)
		if err != nil {
			return failure(err)
		}
		return response(http.StatusOK, map[string]string{"id": in.ID, "key": material})
	case "POST /api/delete-key":
		var in struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		}
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		if err := km.DeleteKey(ctx, in.ID, in.Reason); err != nil {
			return failure(err)
		}
		return response(http.StatusOK, map[string]bool{"ok": true})
	case "POST /api/reset-tokens":
		var in credit.ResetSpec
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		result, err := km.ResetTokens(ctx, in)
		if err != nil {
			return failure(err)
		}
		return response(http.StatusOK, result)
	case "GET /api/callers":
		items, err := km.ListCallers(ctx)
		if err != nil {
			return failure(err)
		}
		return response(http.StatusOK, map[string]any{"items": items})
	case "POST /api/callers":
		var in credit.CallerSpec
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		item, err := km.CreateCaller(ctx, in)
		if err != nil {
			return failure(err)
		}
		return response(http.StatusCreated, map[string]any{"item": item})
	case "PUT /api/callers":
		var in credit.CallerSpec
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		item, err := km.UpdateCaller(ctx, in)
		if err != nil {
			return failure(err)
		}
		return response(http.StatusOK, map[string]any{"item": item})
	case "GET /api/audit":
		q := req.Query
		filter := credit.AuditFilter{
			KeyID:    q.Get("key_id"),
			CallerID: q.Get("caller_id"),
		}
		filter.Page, _ = strconv.Atoi(q.Get("page"))
		filter.PageSize, _ = strconv.Atoi(q.Get("page_size"))
		filter.Limit, _ = strconv.Atoi(q.Get("limit"))
		page, err := km.Audit(ctx, filter)
		if err != nil {
			return failure(err)
		}
		return response(http.StatusOK, page)
	default:
		return response(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}
