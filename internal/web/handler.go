// Package web exposes only management-authenticated financial APIs.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/capture"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/catalog"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/config"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const BasePath = "/v0/management/plugins/credit-manager"

//go:embed assets/*
var assets embed.FS

type Handler struct {
	db       credit.Backend
	captures *capture.Store
	cfg      config.Config
	version  string
	health   func() string
	// catalog and fx back the read-only management catalog preview and the
	// live FX display. They are per-Handler instances so no global cache is
	// shared between handlers (or between tests).
	catalog modelCatalogSource
	fx      exchangeRateSource
}

func New(db credit.Backend, captures *capture.Store, cfg config.Config) *Handler {
	return &Handler{
		db:       db,
		captures: captures,
		cfg:      cfg,
		version:  "dev",
		catalog:  catalog.NewService(nil),
		fx:       catalog.NewFXService(nil),
	}
}

// SetStatus connects immutable runtime diagnostics before serving requests.
func (h *Handler) SetStatus(version string, health func() string) {
	h.version, h.health = version, health
}

// assetMIMEs is the complete, exact allow list of publicly served embedded
// assets. Each entry maps one exact relative path to its content type: the
// served set is fixed here, no caller supplied name is ever derived into a
// path, and no extension lookup can widen the set to an arbitrary embedded
// file. Nested paths (the vendored chart build) are permitted only as this
// exact entry.
var assetMIMEs = map[string]string{
	"index.html":                  "text/html; charset=utf-8",
	"app.js":                      "text/javascript; charset=utf-8",
	"admin-pricing.js":            "text/javascript; charset=utf-8",
	"style.css":                   "text/css; charset=utf-8",
	"charts.js":                   "text/javascript; charset=utf-8",
	"numbers.js":                  "text/javascript; charset=utf-8",
	"vendor/echarts-5.6.0.min.js": "text/javascript; charset=utf-8",
	"lookup.html":                 "text/html; charset=utf-8",
	"lookup.css":                  "text/css; charset=utf-8",
	"lookup.js":                   "text/javascript; charset=utf-8",
}

// Asset returns one embedded public asset by its exact allow-listed name.
//
// An unknown, aliased or traversal spelling is never normalized into an allowed
// name: only the exact paths above can be served. A declared asset whose file
// is not present in this build reports not-found rather than falling back to
// any other file.
func Asset(name string) ([]byte, string, bool) {
	mime, ok := assetMIMEs[name]
	if !ok {
		return nil, "", false
	}
	data, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return nil, "", false
	}
	return data, mime, true
}

func response(code int, body any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(body)
	if err != nil {
		code, raw = 500, []byte(`{"error":"response encoding failed"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: code, Body: raw,
		Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}},
	}
}

func failure(err error) pluginapi.ManagementResponse {
	code, message := 500, "storage operation failed; check plugin health"
	switch {
	case errors.Is(err, credit.ErrInvalid):
		code, message = 400, err.Error()
	case errors.Is(err, credit.ErrUnauthorized):
		code, message = 401, "invalid or inactive key"
	case errors.Is(err, credit.ErrNotFound):
		code, message = 404, "not found"
	case errors.Is(err, credit.ErrConflict):
		code, message = 409, "conflicting operation"
	case errors.Is(err, credit.ErrDenied):
		code, message = 403, "permission denied"
	case errors.Is(err, credit.ErrQuota), errors.Is(err, credit.ErrConcurrency):
		code, message = 429, "credit or concurrency limit reached"
	case errors.Is(err, credit.ErrUnpriced):
		code, message = 400, "model has no price"
	}
	return response(code, map[string]string{"error": message})
}

func decode(raw []byte, target any) error {
	if len(raw) > 64<<10 {
		return credit.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return credit.ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return credit.ErrInvalid
	}
	return nil
}

func (h *Handler) Handle(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if !strings.HasPrefix(req.Path, BasePath+"/api/") {
		return response(404, map[string]string{"error": "not found"})
	}
	// APIs require an explicit bearer management header at the host boundary.
	if strings.TrimSpace(req.Headers.Get("Authorization")) == "" {
		return response(401, map[string]string{"error": "management authorization required"})
	}
	if req.Method != "GET" {
		if req.Headers.Get("Sec-Fetch-Site") == "cross-site" {
			return response(403, map[string]string{"error": "cross-site request rejected"})
		}
		contentType, _, err := mime.ParseMediaType(req.Headers.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			return response(415, map[string]string{"error": "application/json required"})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch req.Method + " " + strings.TrimPrefix(req.Path, BasePath) {
	case "GET /api/overview":
		return h.HandleAnalytics(ctx, req)
	case "POST /api/reveal", "POST /api/delete-key", "POST /api/reset-tokens",
		"GET /api/callers", "POST /api/callers", "PUT /api/callers", "GET /api/audit":
		return h.HandleKeyManagement(ctx, req)
	case "GET /api/keys":
		keys, err := h.db.ListKeys(ctx)
		if err != nil {
			return failure(err)
		}
		return response(200, map[string]any{"items": keys})
	case "POST /api/keys":
		var spec credit.KeySpec
		if err := decode(req.Body, &spec); err != nil {
			return failure(err)
		}
		item, material, err := h.db.CreateKey(ctx, spec)
		if err != nil {
			return failure(err)
		}
		return response(201, map[string]any{"item": item, "key": material})
	case "PUT /api/keys":
		var in struct {
			ID   string         `json:"id"`
			Spec credit.KeySpec `json:"spec"`
		}
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		if in.Spec.Material != "" {
			return failure(credit.ErrInvalid)
		}
		item, err := h.db.UpdateKey(ctx, in.ID, in.Spec)
		if err != nil {
			return failure(err)
		}
		return response(200, map[string]any{"item": item})
	case "POST /api/rotate":
		var in struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		}
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		item, material, err := h.db.RotateKey(ctx, in.ID, in.Key)
		if err != nil {
			return failure(err)
		}
		return response(200, map[string]any{"item": item, "key": material})
	case "GET /api/prices":
		items, err := h.db.ListPrices(ctx)
		if err != nil {
			return failure(err)
		}
		return response(200, map[string]any{"items": items})
	case "PUT /api/prices":
		var in credit.Price
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		if err := h.db.PutPrice(ctx, in); err != nil {
			return failure(err)
		}
		return response(200, map[string]bool{"ok": true})
	case "GET /api/pricing-rules", "PUT /api/pricing-rules",
		"POST /api/delete-price-rule", "GET /api/price-preview":
		// Advanced pricing is served by its own entry point so the price book
		// contract above stays untouched.
		return h.HandlePricing(ctx, req)
	case "GET /api/usage":
		q := req.Query
		f := credit.Filter{KeyID: q.Get("key_id"), Model: q.Get("model"), Provider: q.Get("provider"), AuthID: q.Get("auth_id"), Result: q.Get("result"), Search: q.Get("search"), Bucket: q.Get("bucket")}
		var err error
		if q.Get("from") != "" {
			f.From, err = time.Parse(time.RFC3339Nano, q.Get("from"))
			if err != nil {
				return failure(credit.ErrInvalid)
			}
		}
		if q.Get("to") != "" {
			f.To, err = time.Parse(time.RFC3339Nano, q.Get("to"))
			if err != nil {
				return failure(credit.ErrInvalid)
			}
		}
		f.Page, _ = strconv.Atoi(q.Get("page"))
		f.PageSize, _ = strconv.Atoi(q.Get("page_size"))
		report, err := h.db.Report(ctx, f)
		if err != nil {
			return failure(err)
		}
		return response(200, report)
	case "GET /api/capture":
		if req.Query.Get("id") == "" {
			return failure(credit.ErrInvalid)
		}
		detail, err := h.captures.Get(req.Query.Get("id"))
		if err != nil {
			return failure(err)
		}
		return response(200, detail)
	case "GET /api/health":
		message := ""
		if h.health != nil {
			message = h.health()
		}
		stats := h.captures.Stats()
		status := "ok"
		if message != "" || stats.LastError != "" {
			status = "degraded"
		}
		return response(200, map[string]any{"status": status, "version": h.version, "capture": stats, "retention_months": h.cfg.RetentionMonths, "capture_enabled": h.cfg.Capture.Enabled, "last_error": message, "config": h.cfg})
	case "GET /api/model-catalog":
		return h.handleModelCatalog(ctx, req)
	case "GET /api/exchange-rate":
		return h.handleExchangeRate(ctx, req)
	default:
		return response(404, map[string]string{"error": "not found"})
	}
}
