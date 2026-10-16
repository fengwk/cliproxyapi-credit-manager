package plugin

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/web"
)

// managementRouteSuffixes are the exact management API routes declared to the
// host. The financial handler owns their behavior; registering them here keeps
// the host contract in one place.
var managementRouteSuffixes = map[string][]string{
	"/api/keys":    {http.MethodGet, http.MethodPost, http.MethodPut},
	"/api/rotate":  {http.MethodPost},
	"/api/prices":  {http.MethodGet, http.MethodPut},
	"/api/usage":   {http.MethodGet},
	"/api/capture": {http.MethodGet},
	"/api/resolve": {http.MethodPost},
	"/api/health":  {http.MethodGet},
}

// resourceAssets are the exact public static files this plugin serves. The list
// is the web package's exact allow list, so the served set and the declared set
// can never drift: the public resource base must never reach the financial API
// and assets are embedded, not read from disk. Nested names are permitted so a
// vendored dependency can be served, but every entry is matched exactly.
var resourceAssets = web.AssetNames()

// lookupRoutes are the only public dynamic routes. They are customer
// self-service endpoints: unauthenticated at the host boundary, but each one
// authenticates the caller's own ledger key through the financial handler.
var lookupRoutes = []string{web.PublicLookupData, web.PublicLookupCapture}

// managementRegistrationResponse mirrors the host's expected registration shape.
type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes"`
	Resources []pluginapi.ResourceRoute   `json:"resources"`
}

// managementRequest mirrors the host management request wire shape.
type managementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleManagementRegister declares the management API routes and the public
// resource shell. Management paths are fixed under web.BasePath (which the host
// authenticates); resource paths are resolved under the host-provided resource
// base and expose static assets plus customer-authenticated read-only lookup.
func (m *Manager) handleManagementRegister(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope(&PluginError{Code: "invalid_request", Message: "malformed management registration"})
		}
	}
	resourceBase := strings.TrimRight(strings.TrimSpace(req.ResourceBasePath), "/")
	if resourceBase == "" {
		resourceBase = "/v0/resource/plugins/" + PluginID
	}

	routes := make([]pluginapi.ManagementRoute, 0, 10)
	for _, suffix := range sortedManagementSuffixes() {
		for _, method := range managementRouteSuffixes[suffix] {
			routes = append(routes, pluginapi.ManagementRoute{
				Method:      method,
				Path:        web.BasePath + suffix,
				Description: managementRouteDescription(method, suffix),
			})
		}
	}
	resources := make([]pluginapi.ResourceRoute, 0, len(resourceAssets)+len(lookupRoutes))
	for _, name := range resourceAssets {
		route := pluginapi.ResourceRoute{Path: resourceBase + "/" + name}
		if name == "index.html" {
			route.Menu = PluginName
			route.Description = PluginName + " credit management dashboard."
		}
		resources = append(resources, route)
	}
	// Customer self-service endpoints are declared as explicit resource routes
	// without a menu entry, so the sidebar keeps exactly one plugin menu.
	for _, action := range lookupRoutes {
		resources = append(resources, pluginapi.ResourceRoute{
			Path:        resourceBase + "/" + action,
			Description: PluginName + " customer self-service lookup.",
		})
	}
	return okEnvelope(managementRegistrationResponse{Routes: routes, Resources: resources})
}

// sortedManagementSuffixes returns the declared suffixes in a stable order.
func sortedManagementSuffixes() []string {
	order := []string{"/api/keys", "/api/rotate", "/api/prices", "/api/usage", "/api/capture", "/api/resolve", "/api/health"}
	out := make([]string, 0, len(order))
	for _, suffix := range order {
		if _, ok := managementRouteSuffixes[suffix]; ok {
			out = append(out, suffix)
		}
	}
	return out
}

func managementRouteDescription(method, suffix string) string {
	switch suffix {
	case "/api/keys":
		switch method {
		case http.MethodGet:
			return "List credit keys."
		case http.MethodPost:
			return "Create a credit key and return its one-time plaintext."
		default:
			return "Update a credit key specification."
		}
	case "/api/rotate":
		return "Rotate a credit key material."
	case "/api/prices":
		if method == http.MethodGet {
			return "List configured model prices."
		}
		return "Upsert one model price."
	case "/api/usage":
		return "Usage and cost report."
	case "/api/capture":
		return "One captured request snapshot, fetched on demand."
	case "/api/resolve":
		return "Resolve an unresolved request: release or charge."
	default:
		return "Plugin health and capture statistics."
	}
}

// handleManagement dispatches one management or public resource request.
//
// Management requests are delegated unchanged to the authenticated financial
// handler. Resource requests expose embedded assets or the two explicitly
// customer-authenticated lookup endpoints. No resource path reaches an
// administrative API without host management authentication.
func (m *Manager) handleManagement(request []byte) (resp []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			raw, _ := okEnvelope(managementResponse(http.StatusServiceUnavailable, "credit manager is unavailable"))
			resp, err = raw, nil
		}
	}()
	var req managementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return okEnvelope(managementResponse(http.StatusBadRequest, "malformed management request"))
		}
	}

	deps, ok := m.begin(false)
	if !ok {
		return okEnvelope(managementResponse(http.StatusServiceUnavailable, "credit manager is unavailable"))
	}
	defer m.end()

	path := req.Path
	if strings.HasPrefix(path, web.BasePath+"/api/") {
		return okEnvelope(deps.handler.Handle(req.ManagementRequest))
	}
	relative, ok := resourceRelativePath(path)
	if !ok {
		return okEnvelope(managementResponse(http.StatusNotFound, "not found"))
	}
	if name, okAsset := staticResourceAsset(relative); okAsset {
		return okEnvelope(serveResource(name))
	}
	if action, okLookup := lookupRoute(relative); okLookup {
		return okEnvelope(deps.handler.HandleLookup(action, req.ManagementRequest))
	}
	return okEnvelope(managementResponse(http.StatusNotFound, "not found"))
}

// resourceMarker anchors a public resource path to its plugin directory. The
// segment after the marker is the host-derived plugin id and must be one of this
// plugin's own library ids; the remainder is then matched against the exact
// allow list.
const resourceMarker = "/resource/plugins/"

// resourceRelativePath returns the exact path below the plugin resource
// directory, or false when the request does not name a resource, does not name
// this plugin's resource directory, names only the directory, or is not a clean
// relative path (traversal or an aliasing spelling is rejected rather than
// normalized).
func resourceRelativePath(requestPath string) (string, bool) {
	requestPath = strings.TrimSpace(requestPath)
	marker := strings.Index(requestPath, resourceMarker)
	if marker < 0 {
		return "", false
	}
	rest := requestPath[marker+len(resourceMarker):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return "", false
	}
	// The plugin id segment must be this plugin's own resource directory: the
	// segment is the host-derived library id, which is either the released name
	// or the short legacy name. Any other plugin id is not this plugin's traffic.
	pluginID := rest[:slash]
	if pluginID != PluginID && pluginID != releasedPluginID {
		return "", false
	}
	relative := rest[slash+1:]
	if relative == "" || strings.HasSuffix(relative, "/") {
		return "", false
	}
	if relative != path.Clean(relative) || strings.HasPrefix(relative, "/") {
		return "", false
	}
	if strings.ContainsAny(relative, "\\\x00") {
		return "", false
	}
	return relative, true
}

// staticResourceAsset reports whether the relative path is one of the declared
// static assets.
func staticResourceAsset(relative string) (string, bool) {
	for _, allowed := range resourceAssets {
		if relative == allowed {
			return allowed, true
		}
	}
	return "", false
}

// lookupRoute reports whether the relative path is one of the declared public
// self-service endpoints.
func lookupRoute(relative string) (string, bool) {
	for _, allowed := range lookupRoutes {
		if relative == allowed {
			return allowed, true
		}
	}
	return "", false
}

// serveResource returns one embedded static asset. The asset is content, never
// credentials, and is served with a restrictive header set.
func serveResource(name string) pluginapi.ManagementResponse {
	data, mime, ok := web.Asset(name)
	if !ok {
		return managementResponse(http.StatusNotFound, "not found")
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":           {mime},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: data,
	}
}

// managementResponse builds a small JSON management response.
func managementResponse(status int, message string) pluginapi.ManagementResponse {
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		body = []byte(`{"error":"internal error"}`)
	}
	if status == http.StatusOK {
		body = []byte(`{}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: body,
	}
}
