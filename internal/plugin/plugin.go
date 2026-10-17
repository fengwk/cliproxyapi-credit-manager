// Package plugin implements the CLIProxyAPI native credit-manager plugin: an
// exclusive frontend authenticator plus passive request-interception, request
// lifecycle and usage hooks backed by the credit and capture stores, and a
// management API.
//
// It never executes models: it deliberately declares no executor, router,
// scheduler, response or stream hook, so the host can never route inference
// through it.
package plugin

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// PluginID is the stable plugin route segment used for its Management API
	// paths. The host derives its own library identifier from the plugin
	// filename; this constant is the documented, stable route name.
	PluginID = "credit-manager"
	// PluginName is the human-readable plugin name shown by the host.
	PluginName = "Credit Manager"
	// Author identifies the plugin author for registration metadata.
	Author = "fengwk"
	// GitHubRepository satisfies the host registration metadata requirement.
	GitHubRepository = "https://github.com/fengwk/cliproxyapi-credit-manager"
)

// Build-time variables, overridable with
// -ldflags "-X github.com/fengwk/cliproxyapi-credit-manager/internal/plugin.PluginVersion=...".
var (
	// PluginVersion is the plugin release version.
	PluginVersion = "0.1.0"
	// GitCommit is the full commit hash of the build, empty for local builds.
	GitCommit = ""
	// SDKVersion is the CLIProxyAPI SDK the plugin was built against. It is the
	// minimum host contract this plugin supports.
	SDKVersion = "v8.0.30"
)

// Metadata returns the registration metadata reported to the host.
func Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             PluginName,
		Version:          metadataVersion(),
		Author:           Author,
		GitHubRepository: GitHubRepository,
	}
}

// metadataVersion is the release version reported to the host as registration
// metadata. It is deliberately a bare dotted release number: the CPA plugin
// store compares an installed metadata version with the registry release
// segment by segment (pluginstore.UpdateAvailable), and a build suffix such as
// "+<commit>" makes that comparison non-numeric, so the host would always offer
// a spurious update even for the exact released build. Build provenance is
// exposed through runtime diagnostics instead. See diagnosticVersion.
func metadataVersion() string {
	version := strings.TrimSpace(PluginVersion)
	if version == "" {
		return "0.0.0"
	}
	return version
}

// diagnosticVersion is the human-facing build identity used by runtime
// diagnostics (the management health endpoint). It keeps the full build commit
// so an operator can identify the exact source revision, which the numeric-only
// metadata version deliberately omits so the host's plugin store can compare
// versions.
func diagnosticVersion() string {
	version := metadataVersion()
	commit := strings.TrimSpace(GitCommit)
	if commit == "" {
		return version
	}
	return version + "+" + commit
}

// registration is the plugin.register / plugin.reconfigure response payload.
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

// registrationCapability declares exactly the capabilities this plugin uses.
// It deliberately omits every executor, router, scheduler, response and stream
// hook so the host can never route inference through the plugin.
type registrationCapability struct {
	FrontendAuthProvider          bool `json:"frontend_auth_provider"`
	FrontendAuthProviderExclusive bool `json:"frontend_auth_provider_exclusive"`
	RequestInterceptor            bool `json:"request_interceptor"`
	RequestLifecyclePlugin        bool `json:"request_lifecycle_plugin"`
	UsagePlugin                   bool `json:"usage_plugin"`
	ManagementAPI                 bool `json:"management_api"`
}

// registrationResponse requires the current SDK contract and reports exactly
// that schema; it never negotiates down to an older or undefined contract.
func registrationResponse(hostSchema uint32) (registration, error) {
	if hostSchema < pluginabi.SchemaVersion {
		return registration{}, &PluginError{
			Code:       "unsupported_host",
			Message:    "host RPC schema does not meet the current SDK contract",
			HTTPStatus: 0,
		}
	}
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata:      Metadata(),
		Capabilities: registrationCapability{
			FrontendAuthProvider:          true,
			FrontendAuthProviderExclusive: true,
			RequestInterceptor:            true,
			RequestLifecyclePlugin:        true,
			UsagePlugin:                   true,
			ManagementAPI:                 true,
		},
	}, nil
}
