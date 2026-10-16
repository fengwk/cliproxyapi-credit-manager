package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/capture"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/config"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func testHandler(t *testing.T) *Handler {
	t.Helper()
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	db, err := credit.Open(credit.Options{DataDir: cfg.DataDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	captures, err := capture.Open("", capture.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = captures.Close() })
	return New(db, captures, cfg)
}

func managementRequest(method, suffix, body string) pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{
		Method: method, Path: BasePath + suffix, Body: []byte(body),
		Headers: http.Header{
			"Authorization": {"Bearer synthetic-manager"},
			"Content-Type":  {"application/json; charset=utf-8"},
		},
		Query: url.Values{},
	}
}

// Well-formed RFC3339 dates may still exceed the ledger timestamp range.
func TestUsageRangeRejectsTimestampOverflow(t *testing.T) {
	h := testHandler(t)
	req := managementRequest("GET", "/api/usage", "")
	req.Query.Set("from", "2026-01-01T00:00:00Z")
	req.Query.Set("to", "2500-01-01T00:00:00Z")
	if got := h.Handle(req); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
	}
}

// Rejected requests must not change financial state, even if their JSON is valid.
func TestManagementBoundary(t *testing.T) {
	h := testHandler(t)
	for _, tc := range []struct {
		name string
		edit func(*pluginapi.ManagementRequest)
		code int
	}{
		{"no-header", func(r *pluginapi.ManagementRequest) { r.Headers.Del("Authorization") }, 401},
		{"cross-site", func(r *pluginapi.ManagementRequest) { r.Headers.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"wrong-media", func(r *pluginapi.ManagementRequest) { r.Headers.Set("Content-Type", "application/json-extra") }, 415},
		{"public-prefix", func(r *pluginapi.ManagementRequest) { r.Path = "/v0/resource/plugins/credit-manager/api/keys" }, 404},
		{"unknown-field", func(r *pluginapi.ManagementRequest) { r.Body = []byte(`{"enabled":true,"unknown":1}`) }, 400},
		{"trailing-json", func(r *pluginapi.ManagementRequest) { r.Body = []byte(`{"enabled":true} {}`) }, 400},
		{"oversize", func(r *pluginapi.ManagementRequest) { r.Body = []byte(strings.Repeat(" ", 65537)) }, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := managementRequest("POST", "/api/keys", `{"enabled":true}`)
			tc.edit(&r)
			if got := h.Handle(r); got.StatusCode != tc.code {
				t.Fatalf("status=%d body=%s", got.StatusCode, got.Body)
			}
		})
	}
	keys, err := h.db.ListKeys(context.Background())
	if err != nil || len(keys) != 0 {
		t.Fatalf("rejected mutation wrote keys: %+v %v", keys, err)
	}
}

// Plaintext material is only returned once; neither list nor update reveals it.
func TestManagementKeyLifecycle(t *testing.T) {
	h := testHandler(t)
	secret := "synthetic-custom-key"
	created := h.Handle(managementRequest("POST", "/api/keys", `{"enabled":true,"key":"`+secret+`"}`))
	if created.StatusCode != 201 {
		t.Fatalf("create=%d %s", created.StatusCode, created.Body)
	}
	var out struct {
		Item credit.Key `json:"item"`
		Key  string     `json:"key"`
	}
	if err := json.Unmarshal(created.Body, &out); err != nil || out.Key != secret || out.Item.Spec.Material != "" {
		t.Fatalf("create body=%s %v", created.Body, err)
	}
	listed := h.Handle(managementRequest("GET", "/api/keys", ""))
	if strings.Contains(string(listed.Body), secret) || listed.StatusCode != 200 {
		t.Fatalf("list leaked material: %s", listed.Body)
	}
	updated := h.Handle(managementRequest("PUT", "/api/keys",
		`{"id":"`+out.Item.ID+`","spec":{"enabled":true,"key":"replacement-key"}}`))
	if updated.StatusCode != 400 {
		t.Fatalf("update accepted material: %d %s", updated.StatusCode, updated.Body)
	}
	if _, err := h.db.Authenticate(context.Background(), secret); err != nil {
		t.Fatal("invalid update changed original key", err)
	}
	rotated := h.Handle(managementRequest("POST", "/api/rotate", `{"id":"`+out.Item.ID+`"}`))
	if rotated.StatusCode != 200 || strings.Contains(string(rotated.Body), secret) {
		t.Fatalf("rotate=%d %s", rotated.StatusCode, rotated.Body)
	}
	if _, err := h.db.Authenticate(context.Background(), secret); !errors.Is(err, credit.ErrUnauthorized) {
		t.Fatalf("old material still valid: %v", err)
	}
	if listed.Headers.Get("Cache-Control") != "no-store" {
		t.Fatal("financial response is cacheable")
	}
}

// Safe status mapping must not echo driver errors, paths or credential text.
func TestManagementSafeErrorsAndCaptureDisabled(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{credit.ErrInvalid, 400}, {credit.ErrConflict, 409},
		{credit.ErrNotFound, 404}, {credit.ErrQuota, 429},
		{errors.New("/private/path: synthetic-secret"), 500},
	} {
		got := failure(tc.err)
		if got.StatusCode != tc.code || strings.Contains(string(got.Body), "synthetic-secret") {
			t.Fatalf("failure=%d %s", got.StatusCode, got.Body)
		}
	}
	h := testHandler(t)
	r := managementRequest("GET", "/api/capture", "")
	r.Query.Set("id", "synthetic-request")
	got := h.Handle(r)
	if got.StatusCode != 200 || !strings.Contains(string(got.Body), `"status":"disabled"`) {
		t.Fatalf("capture=%d %s", got.StatusCode, got.Body)
	}
	r = managementRequest("GET", "/api/usage", "")
	r.Query.Set("from", "not-a-date")
	if got := h.Handle(r); got.StatusCode != 400 {
		t.Fatalf("malformed range=%d", got.StatusCode)
	}
}
