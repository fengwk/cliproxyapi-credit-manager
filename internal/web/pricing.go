package web

import (
	"context"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// pricingRules is the narrow, optional capability the pricing management API
// needs from the ledger. It is asserted on the shared Backend rather than added
// to credit.Backend, so the common contract stays unchanged and a backend that
// cannot manage pricing simply refuses the request.
type pricingRules interface {
	ListPricingRules(ctx context.Context) ([]credit.PricingRule, error)
	PutPricingRule(ctx context.Context, rule credit.PricingRule) error
	DeletePricingRule(ctx context.Context, id string) error
}

// pricingPreview is the optional read-only preview capability.
type pricingPreview interface {
	PreviewPrice(ctx context.Context, model, serviceTier string, inputTokens int64) (credit.PriceMatch, error)
}

// HandlePricing serves the advanced pricing routes under the plugin management
// base path:
//
//	GET  /api/pricing-rules                     => {"items":[PricingRule]}
//	PUT  /api/pricing-rules   PricingRule       => {"item":PricingRule}
//	POST /api/delete-price-rule {"id":id}       => {"ok":true}
//	GET  /api/price-preview?model=&service_tier=&input_tokens= => PriceMatch
//
// It applies the same management authorization and content-type guards as
// Handle so it stays safe when it is reached directly. The parent management
// dispatcher routes these methods here; it is a separate entry point so the
// shared financial handler and the plugin route declaration stay untouched.
func (h *Handler) HandlePricing(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if strings.TrimSpace(req.Headers.Get("Authorization")) == "" {
		return response(http.StatusUnauthorized, map[string]string{"error": "management authorization required"})
	}
	if req.Method != "GET" {
		if req.Headers.Get("Sec-Fetch-Site") == "cross-site" {
			return response(http.StatusForbidden, map[string]string{"error": "cross-site request rejected"})
		}
		contentType, _, err := mime.ParseMediaType(req.Headers.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			return response(http.StatusUnsupportedMediaType, map[string]string{"error": "application/json required"})
		}
	}
	rules, ok := h.db.(pricingRules)
	if !ok {
		return response(http.StatusServiceUnavailable, map[string]string{"error": "pricing management is unavailable"})
	}
	switch req.Method + " " + strings.TrimPrefix(req.Path, BasePath) {
	case "GET /api/pricing-rules":
		items, err := rules.ListPricingRules(ctx)
		if err != nil {
			return failure(err)
		}
		return response(http.StatusOK, map[string]any{"items": items})
	case "PUT /api/pricing-rules":
		var in credit.PricingRule
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		if err := rules.PutPricingRule(ctx, in); err != nil {
			return failure(err)
		}
		// Report the canonical stored rule (normalized modes, tiers and the
		// defaulted price model) instead of echoing the request.
		if item, ok := findPricingRule(ctx, rules, in.ID); ok {
			return response(http.StatusOK, map[string]any{"item": item})
		}
		return response(http.StatusOK, map[string]any{"item": in})
	case "POST /api/delete-price-rule":
		var in struct {
			ID string `json:"id"`
		}
		if err := decode(req.Body, &in); err != nil {
			return failure(err)
		}
		if err := rules.DeletePricingRule(ctx, in.ID); err != nil {
			return failure(err)
		}
		return response(http.StatusOK, map[string]bool{"ok": true})
	case "GET /api/price-preview":
		preview, ok := h.db.(pricingPreview)
		if !ok {
			return response(http.StatusServiceUnavailable, map[string]string{"error": "pricing preview is unavailable"})
		}
		inputTokens := int64(0)
		if raw := strings.TrimSpace(req.Query.Get("input_tokens")); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed < 0 {
				return failure(credit.ErrInvalid)
			}
			inputTokens = parsed
		}
		match, err := preview.PreviewPrice(ctx, req.Query.Get("model"), req.Query.Get("service_tier"), inputTokens)
		if err != nil {
			return failure(err)
		}
		return response(http.StatusOK, match)
	default:
		return response(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// findPricingRule returns the stored form of one rule.
func findPricingRule(ctx context.Context, rules pricingRules, id string) (credit.PricingRule, bool) {
	id = strings.TrimSpace(id)
	items, err := rules.ListPricingRules(ctx)
	if err != nil {
		return credit.PricingRule{}, false
	}
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return credit.PricingRule{}, false
}
