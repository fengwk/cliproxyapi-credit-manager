package config

import (
	"testing"
	"time"
)

func TestConfiguration(t *testing.T) {
	// Defaults and invalid explicit limits are distinct.
	c, err := Parse(nil)
	if err != nil || c.RetentionMonths != 3 || c.Capture.Retention.Hours() != 24 ||
		c.Capture.MaxStorageBytes != 1<<30 || c.Capture.MaxBodyBytes != 10<<20 || c.Capture.Enabled {
		t.Fatalf("defaults: %#v %v", c, err)
	}
	for _, raw := range []string{"history: {retention_months: 0}", "request_capture: {retention: nope}", "unknown: 1", "max_pending_requests: -1"} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	c, err = Parse([]byte("history: {retention_months: 6}\nrequest_capture: {enabled: true, retention: 48h, max_body_bytes: 1024, max_storage_bytes: 2097152}"))
	if err != nil || c.RetentionMonths != 6 || !c.Capture.Enabled || c.Capture.Retention.Hours() != 48 {
		t.Fatalf("custom: %#v %v", c, err)
	}
}

// TestWarmupConfiguration pins the optional warmup contract: disabled by
// default with no targets, normalized targets, and rejection of a target that
// could not pin a credential exactly.
func TestWarmupConfiguration(t *testing.T) {
	// The default is completely inert.
	c, err := Parse(nil)
	if err != nil || c.Warmup.Enabled || c.Warmup.Period != time.Hour || len(c.Warmup.Targets) != 0 {
		t.Fatalf("warmup defaults: %#v %v", c.Warmup, err)
	}
	// Enabling warmup without targets is valid and probes nothing.
	if c, err = Parse([]byte("warmup: {enabled: true}")); err != nil || !c.Warmup.Enabled || c.Warmup.Period != time.Hour {
		t.Fatalf("enabled without targets: %#v %v", c.Warmup, err)
	}

	c, err = Parse([]byte("warmup:\n  enabled: true\n  interval: 30m\n  targets:\n    - {auth_id: ' a-1 ', provider: p1, model: m1}\n    - {auth_id: a-2, provider: p2, model: m2, protocol: GEMINI}\n"))
	if err != nil {
		t.Fatalf("warmup parse: %v", err)
	}
	if !c.Warmup.Enabled || c.Warmup.Interval != "30m" || c.Warmup.Period != 30*time.Minute || len(c.Warmup.Targets) != 2 {
		t.Fatalf("warmup parsed: %#v", c.Warmup)
	}
	if c.Warmup.Targets[0].AuthID != "a-1" || c.Warmup.Targets[0].Protocol != "openai" {
		t.Fatalf("first target must be trimmed and default to openai: %#v", c.Warmup.Targets[0])
	}
	if c.Warmup.Targets[1].Protocol != "gemini" {
		t.Fatalf("protocol must be normalized to lower case: %#v", c.Warmup.Targets[1])
	}

	// A target that can not pin auth_id, provider and model exactly, or a
	// protocol this plugin does not build a request for, is rejected.
	for _, raw := range []string{
		"warmup: {interval: nope}",
		"warmup: {interval: 0s}",
		"warmup: {interval: -1m}",
		"warmup: {targets: [{auth_id: a, provider: p, model: m, protocol: bogus}]}",
		"warmup: {targets: [{provider: p, model: m}]}",
		"warmup: {targets: [{auth_id: a, model: m}]}",
		"warmup: {targets: [{auth_id: a, provider: p}]}",
		"warmup: {unknown_field: 1}",
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}
