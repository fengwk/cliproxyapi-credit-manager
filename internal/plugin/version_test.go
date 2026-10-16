package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
)

// testCommit is a syntactically valid, full-length build commit.
const testCommit = "0123456789abcdef0123456789abcdef01234567"

// TestMetadataVersionIsHostComparableRelease pins the registration metadata
// version contract against the public SDK function the host itself uses
// (pluginstore.UpdateAvailable, the declared CPA v8 contract): the metadata
// version must be a bare release number, never a build-suffixed one, otherwise
// the store comparison turns non-numeric and the host always offers an update.
func TestMetadataVersionIsHostComparableRelease(t *testing.T) {
	version := metadataVersion()
	if want := strings.TrimSpace(PluginVersion); version != want {
		t.Fatalf("metadataVersion = %q, want the bare PluginVersion %q", version, want)
	}
	if strings.Contains(version, "+") {
		t.Fatalf("metadataVersion = %q must not carry a build suffix", version)
	}
	// The regression: a version with a "+<commit>" suffix is not comparable, so
	// the host would report an available update for the exact released build.
	// The bare version must compare as equal instead.
	if !pluginstore.UpdateAvailable(version+"+"+testCommit, version) {
		t.Fatalf("UpdateAvailable(%q, %q) = false; the suffixed form is expected to "+
			"be non-comparable and always offer an update", version+"+"+testCommit, version)
	}
	if pluginstore.UpdateAvailable(version, version) {
		t.Fatalf("UpdateAvailable(%q, %q) = true, want no update for the same release", version, version)
	}
	// The version stays numerically ordered, not merely equal: a lower release
	// offers an update and a higher one does not.
	if !pluginstore.UpdateAvailable("0.0.1", version) {
		t.Fatalf("UpdateAvailable(%q, %q) = false, want an upgrade for an older build", "0.0.1", version)
	}
	if pluginstore.UpdateAvailable(version, "0.0.1") {
		t.Fatalf("UpdateAvailable(%q, %q) = true, want no downgrade for a newer build", version, "0.0.1")
	}
}

// TestMetadataVersionEmptyFallback keeps the host contract valid even without an
// injected release version: an empty PluginVersion must not produce a bare "",
// which the host treats as a missing version.
func TestMetadataVersionEmptyFallback(t *testing.T) {
	original := PluginVersion
	t.Cleanup(func() { PluginVersion = original })
	PluginVersion = "  "
	if got := metadataVersion(); got != "0.0.0" {
		t.Fatalf("metadataVersion = %q, want the %q fallback", got, "0.0.0")
	}
}

// TestDiagnosticVersionKeepsBuildCommit verifies the build commit is not lost
// when it is removed from the metadata: runtime diagnostics keep the full build
// identity, while the metadata version stays bare even when a commit is present.
func TestDiagnosticVersionKeepsBuildCommit(t *testing.T) {
	original := GitCommit
	t.Cleanup(func() { GitCommit = original })

	GitCommit = ""
	if got, want := diagnosticVersion(), metadataVersion(); got != want {
		t.Fatalf("diagnosticVersion without a commit = %q, want %q", got, want)
	}
	GitCommit = "  " + testCommit + "  "
	if got, want := diagnosticVersion(), metadataVersion()+"+"+testCommit; got != want {
		t.Fatalf("diagnosticVersion = %q, want %q", got, want)
	}
	if got := metadataVersion(); strings.Contains(got, testCommit) || strings.Contains(got, "+") {
		t.Fatalf("metadataVersion = %q must not leak the build commit", got)
	}
}

// TestRegistrationReportsHostComparableVersion registers a real manager and
// asserts the host-facing registration metadata carries the bare, host
// comparable release version the store will compare.
func TestRegistrationReportsHostComparableVersion(t *testing.T) {
	m := NewManager()
	t.Cleanup(func() {
		_, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil)
	})
	payload, err := json.Marshal(lifecycleRequest{
		ConfigYAML:    captureYAML(t.TempDir(), 16),
		SchemaVersion: pluginabi.SchemaVersion,
	})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	raw, err := m.HandleCall(pluginabi.MethodPluginRegister, payload)
	if err != nil {
		t.Fatalf("register returned error: %v", err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode register envelope: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("register failed: %s", raw)
	}
	var registration struct {
		Metadata pluginapi.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	got := registration.Metadata.Version
	if want := metadataVersion(); got != want {
		t.Fatalf("registered metadata version = %q, want %q", got, want)
	}
	if strings.Contains(got, "+") {
		t.Fatalf("registered metadata version = %q must not carry a build suffix", got)
	}
	if pluginstore.UpdateAvailable(got, got) {
		t.Fatalf("registered metadata version %q would be offered as an update over itself", got)
	}
}

// TestRegistrationMetadataMatchesBareRelease ties the registered metadata
// version to the documented release string form so a future suffix (for example
// a re-added commit) is rejected explicitly rather than silently accepted.
func TestRegistrationMetadataMatchesBareRelease(t *testing.T) {
	original := PluginVersion
	t.Cleanup(func() { PluginVersion = original })

	// Leading and trailing whitespace is trimmed, never passed through.
	PluginVersion = "  1.2.3  "
	got := metadataVersion()
	if got != "1.2.3" {
		t.Fatalf("metadataVersion = %q, want the trimmed %q", got, "1.2.3")
	}
	parts := strings.Split(got, ".")
	if len(parts) != 3 {
		t.Fatalf("metadataVersion = %q, want a three segment release number", got)
	}
	for _, part := range parts {
		if part == "" {
			t.Fatalf("metadataVersion = %q has an empty segment", got)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				t.Fatalf("metadataVersion = %q has a non-numeric segment %q", got, part)
			}
		}
	}
}
