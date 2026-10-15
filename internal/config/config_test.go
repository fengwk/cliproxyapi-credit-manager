package config

import "testing"

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
