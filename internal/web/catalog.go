package web

import (
	"context"
	"net/http"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/catalog"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// modelCatalogSource is the narrow capability the model-catalog preview needs.
// It is satisfied by *catalog.Service and by test doubles, so the endpoint can
// be exercised without any network access.
type modelCatalogSource interface {
	Get(ctx context.Context, refresh bool) catalog.Snapshot
}

// exchangeRateSource is the narrow capability the exchange-rate endpoint needs.
type exchangeRateSource interface {
	Get(ctx context.Context, refresh bool) catalog.Rate
}

// handleModelCatalog serves the normalized models.dev catalog used by the
// management price preview.
//
// It is read-only. It never writes a price or rule and never touches the
// ledger, so a catalog refresh cannot reprice already recorded usage; a preview
// is only ever applied afterwards through the existing management API. The
// upstream host is fixed by the catalog service, so the request cannot redirect
// the fetch: no query parameter selects a URL, a source or a proxy.
func (h *Handler) handleModelCatalog(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if h.catalog == nil {
		return response(http.StatusServiceUnavailable, map[string]string{"error": "model catalog unavailable"})
	}
	refresh := req.Query.Get("refresh") == "1"
	snapshot := h.catalog.Get(ctx, refresh)
	if len(snapshot.Items) == 0 && snapshot.Error != "" {
		return response(http.StatusBadGateway, snapshot)
	}
	return response(http.StatusOK, snapshot)
}

// handleExchangeRate serves the live USD/CNY quote for display conversion only.
//
// It never bills or reprices anything. When no quote is available the response
// reports the error and carries no rate, so the UI keeps its manual fallback
// value instead of a synthesized one.
func (h *Handler) handleExchangeRate(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if h.fx == nil {
		return response(http.StatusServiceUnavailable, map[string]string{"error": "exchange rate unavailable"})
	}
	refresh := req.Query.Get("refresh") == "1"
	rate := h.fx.Get(ctx, refresh)
	if rate.Rate == "" && rate.Error != "" {
		return response(http.StatusBadGateway, rate)
	}
	return response(http.StatusOK, rate)
}
