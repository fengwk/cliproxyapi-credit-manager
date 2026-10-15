package plugin

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

// authProviderID is the plugin-local frontend auth provider name. The host
// prefixes it with "plugin:<library-id>:" when it reports AccessProvider, so
// identification must never assume the library id is known locally.
const authProviderID = "credit-manager-key"

// accessProviderPrefix marks a frontend auth provider owned by a native plugin.
const accessProviderPrefix = "plugin:"

// authCandidate is one credential location in the CPA candidate order.
type authCandidate struct {
	value  string
	source string
}

// authenticate is the exclusive frontend authentication provider. It mirrors
// CPA's candidate order and Bearer parsing, and returns the identity of the
// first candidate that resolves to a valid credit-manager key.
//
// The returned principal is the key's stable internal identity (cmk:<UUID>);
// the host derives the SDK caller scope from it, which the request
// interception hook then uses to resolve the same key without ever re-reading
// the bearer credential for billing.
func (m *Manager) authenticate(request pluginapi.FrontendAuthRequest) (pluginapi.FrontendAuthResponse, bool) {
	candidates := authCandidates(request)
	if len(candidates) == 0 {
		return pluginapi.FrontendAuthResponse{}, false
	}
	deps, ok := m.begin(false)
	if !ok {
		return pluginapi.FrontendAuthResponse{}, false
	}
	defer m.end()

	ctx := context.Background()
	for _, candidate := range candidates {
		key, err := deps.store.Authenticate(ctx, candidate.value)
		if err != nil {
			// A rejected candidate must never fail the whole request: the host
			// treats an unhandled credential as "try the next provider".
			if err != credit.ErrUnauthorized && err != credit.ErrInvalid {
				m.recordError(err)
			}
			continue
		}
		principal := strings.TrimSpace(key.Principal)
		if principal == "" {
			continue
		}
		return pluginapi.FrontendAuthResponse{
			Authenticated: true,
			Principal:     principal,
			Metadata: map[string]string{
				"key_id": strings.TrimSpace(key.ID),
				"source": candidate.source,
			},
		}, true
	}
	return pluginapi.FrontendAuthResponse{}, false
}

// authCandidates extracts the credential candidates in CPA's native order:
// Authorization (with Bearer parsing), X-Goog-Api-Key, X-Api-Key, then the key
// and auth_token query parameters.
func authCandidates(request pluginapi.FrontendAuthRequest) []authCandidate {
	authorization := ""
	googAPIKey := ""
	apiKey := ""
	queryKey := ""
	queryAuthToken := ""
	if request.Headers != nil {
		authorization = request.Headers.Get("Authorization")
		googAPIKey = request.Headers.Get("X-Goog-Api-Key")
		apiKey = request.Headers.Get("X-Api-Key")
	}
	if request.Query != nil {
		queryKey = request.Query.Get("key")
		queryAuthToken = request.Query.Get("auth_token")
	}

	raw := []authCandidate{
		{value: extractBearerToken(authorization), source: "authorization"},
		{value: googAPIKey, source: "x-goog-api-key"},
		{value: apiKey, source: "x-api-key"},
		{value: queryKey, source: "query-key"},
		{value: queryAuthToken, source: "query-auth-token"},
	}
	candidates := make([]authCandidate, 0, len(raw))
	for _, candidate := range raw {
		trimmed := strings.TrimSpace(candidate.value)
		if trimmed == "" {
			continue
		}
		candidate.value = trimmed
		candidates = append(candidates, candidate)
	}
	return candidates
}

// extractBearerToken returns the token of an Authorization header, tolerating a
// missing scheme exactly like CPA's built-in provider.
func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if !strings.EqualFold(parts[0], "bearer") {
		return header
	}
	return strings.TrimSpace(parts[1])
}

// isOwnAccessProvider reports whether a usage record's AccessProvider was
// resolved by this plugin's frontend auth provider, without assuming the
// host-derived library id.
func isOwnAccessProvider(provider string) bool {
	provider = strings.TrimSpace(provider)
	if !strings.HasPrefix(provider, accessProviderPrefix) {
		return false
	}
	return strings.HasSuffix(provider, ":"+authProviderID)
}

var _ = http.MethodGet
