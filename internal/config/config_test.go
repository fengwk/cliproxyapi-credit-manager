package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguration(t *testing.T) {
	// Defaults and invalid explicit limits are distinct.
	c, err := Parse(nil)
	if err != nil || c.RetentionMonths != 3 || c.Capture.Retention.Hours() != 24 ||
		c.Capture.MaxStorageBytes != 1<<30 || c.Capture.MaxBodyBytes != 10<<20 || c.Capture.Enabled {
		t.Fatalf("defaults: %#v %v", c, err)
	}
	for _, raw := range []string{"history: {retention_months: 0}", "request_capture: {retention: nope}", "unknown: 1", "max_pending_requests: -1", "history: {retention_months: 3}\n---\nunknown: 1"} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	c, err = Parse([]byte("history: {retention_months: 6}\nrequest_capture: {enabled: true, retention: 48h, max_body_bytes: 1024, max_storage_bytes: 2097152}"))
	if err != nil || c.RetentionMonths != 6 || !c.Capture.Enabled || c.Capture.Retention.Hours() != 48 {
		t.Fatalf("custom: %#v %v", c, err)
	}
}

// TestRetiredSettingsRejected pins that the removed background warmup and the
// pending-request backlog gate are refused with an explicit error instead of
// being silently ignored, which would leave an operator believing the retired
// behavior is still active.
func TestRetiredSettingsRejected(t *testing.T) {
	for _, raw := range []string{
		"warmup: {enabled: true}",
		"warmup:\n  enabled: true\n  interval: 30m\n  targets:\n    - {auth_id: a, provider: p, model: m}",
		"max_pending_requests: 16",
		"data_dir: ./ledger\nmax_pending_requests: 1",
	} {
		_, err := Parse([]byte(raw))
		if err == nil {
			t.Fatalf("accepted retired setting %q", raw)
		}
		setting := "warmup"
		if strings.Contains(raw, "max_pending_requests") {
			setting = "max_pending_requests"
		}
		if !strings.Contains(err.Error(), "field "+setting+" not found") {
			t.Fatalf("retired setting %q error = %v, want an explicit removed-setting error", raw, err)
		}
	}
}

// TestDataDirWhitespaceRejected keeps data_dir verbatim: trimming a padded path
// would silently relocate the ledger away from the path the health and capture
// settings resolve, so an empty/blank or padded value must fail closed.
func TestDataDirWhitespaceRejected(t *testing.T) {
	for _, raw := range []string{
		"data_dir: ''",
		"data_dir: ' '",
		"data_dir: \"   \"",
		"data_dir: \"\\t\"",
		"data_dir: './ledger '",
		"data_dir: ' ./ledger'",
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted whitespace-padded data_dir %q", raw)
		}
	}
	// A normal path stays valid, and embedded spaces inside a path segment are
	// not leading or trailing whitespace.
	c, err := Parse([]byte("data_dir: './ledger dir/data'"))
	if err != nil {
		t.Fatalf("embedded space rejected: %v", err)
	}
	if !filepath.IsAbs(c.DataDir) || !strings.HasSuffix(c.DataDir, filepath.Join("ledger dir", "data")) {
		t.Fatalf("data dir = %q", c.DataDir)
	}
}

// TestStoreManifestAccepted verifies that store-owned configuration metadata
// injected by the CPA plugin store when installing a plugin is accepted without error.
func TestStoreManifestAccepted(t *testing.T) {
	raw := []byte(`
enabled: true
priority: 0
store:
  id: cliproxyapi-credit-manager
  name: Credit Manager
  source_id: custom-store
  source_url: https://raw.githubusercontent.com/fengwk/cliproxyapi-credit-manager/main/registry.json
  version: 0.1.2
data_dir: ./data/cliproxyapi-credit-manager
history:
  retention_months: 3
`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("store manifest rejected: %v", err)
	}
	if cfg.Enabled == nil || !*cfg.Enabled {
		t.Fatal("expected enabled to be true")
	}
	if cfg.Store.Kind == 0 {
		t.Fatal("expected Store node to be parsed")
	}
}
