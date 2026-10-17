package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/config"
	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

// minimalBackend satisfies credit.Backend through its embedded interface but
// deliberately omits the optional key management capability.
type minimalBackend struct{ credit.Backend }

// A backend without the key management capability must render the routes
// unavailable rather than panicking or half serving them.
func TestKeyManagementUnavailableWithoutCapability(t *testing.T) {
	h := New(minimalBackend{}, nil, config.Config{})
	got := h.HandleKeyManagement(context.Background(), managementRequest("GET", "/api/audit", ""))
	if got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", got.StatusCode, got.Body)
	}
}

// An unrecognized key management path is a not-found, never a fallthrough.
func TestKeyManagementUnknownRoute(t *testing.T) {
	h := testHandler(t)
	got := h.HandleKeyManagement(context.Background(), managementRequest("GET", "/api/unknown", ""))
	if got.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", got.StatusCode, got.Body)
	}
}

// The authenticating handler serves the management APIs. This exercises the
// reveal, delete, reset, caller and audit contracts end to end at the HTTP
// boundary and proves no route leaks key material.
func TestKeyManagementRoutes(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	key, material, err := h.db.CreateKey(ctx, credit.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	// Reveal returns the original material exactly once per call.
	revealed := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/reveal", `{"id":"`+key.ID+`"}`))
	if revealed.StatusCode != http.StatusOK {
		t.Fatalf("reveal = %d %s", revealed.StatusCode, revealed.Body)
	}
	var out struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(revealed.Body, &out); err != nil || out.ID != key.ID || out.Key != material {
		t.Fatalf("reveal body = %s %v", revealed.Body, err)
	}
	// Revealing an unknown key is a not-found.
	if got := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/reveal", `{"id":"missing"}`)); got.StatusCode != http.StatusNotFound {
		t.Fatalf("reveal missing = %d %s", got.StatusCode, got.Body)
	}
	// Bad JSON and unknown fields are rejected before any work.
	if got := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/reveal", `{"nope":1}`)); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("reveal bad body = %d %s", got.StatusCode, got.Body)
	}

	// Callers: create, list, update.
	created := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/callers", `{"display_name":"Acme","enabled":true}`))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("caller create = %d %s", created.StatusCode, created.Body)
	}
	var callerOut struct {
		Item credit.Caller `json:"item"`
	}
	if err := json.Unmarshal(created.Body, &callerOut); err != nil || callerOut.Item.ID == "" {
		t.Fatalf("caller body = %s %v", created.Body, err)
	}
	listed := h.HandleKeyManagement(ctx, managementRequest("GET", "/api/callers", ""))
	if listed.StatusCode != http.StatusOK || !strings.Contains(string(listed.Body), `"items"`) {
		t.Fatalf("callers = %d %s", listed.StatusCode, listed.Body)
	}
	if got := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/callers", `{"display_name":""}`)); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty caller name = %d %s", got.StatusCode, got.Body)
	}
	updated := h.HandleKeyManagement(ctx, managementRequest("PUT", "/api/callers",
		`{"id":"`+callerOut.Item.ID+`","display_name":"Acme Ltd","enabled":false}`))
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("caller update = %d %s", updated.StatusCode, updated.Body)
	}

	// Reset requires a scope and a reason.
	if got := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/reset-tokens", `{"total":true,"reason":"x"}`)); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("reset without scope = %d %s", got.StatusCode, got.Body)
	}
	if got := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/reset-tokens", `{"id":"`+key.ID+`","total":true}`)); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("reset without reason = %d %s", got.StatusCode, got.Body)
	}
	reset := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/reset-tokens",
		`{"id":"`+key.ID+`","total":true,"daily":true,"reason":"goodwill"}`))
	if reset.StatusCode != http.StatusOK {
		t.Fatalf("reset = %d %s", reset.StatusCode, reset.Body)
	}

	// Delete disables and marks the key; a deleted key can no longer be revealed.
	if got := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/delete-key", `{"id":"`+key.ID+`","reason":"cleanup"}`)); got.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d %s", got.StatusCode, got.Body)
	}
	if got := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/reveal", `{"id":"`+key.ID+`"}`)); got.StatusCode != http.StatusConflict {
		t.Fatalf("reveal deleted = %d %s", got.StatusCode, got.Body)
	}
	if got := h.HandleKeyManagement(ctx, managementRequest("POST", "/api/delete-key", `{}`)); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete empty id = %d %s", got.StatusCode, got.Body)
	}

	// Audit is filterable, paged and never leaks the material.
	auditReq := managementRequest("GET", "/api/audit", "")
	auditReq.Query.Set("key_id", key.ID)
	auditReq.Query.Set("page", "1")
	auditReq.Query.Set("page_size", "2")
	audit := h.HandleKeyManagement(ctx, auditReq)
	if audit.StatusCode != http.StatusOK {
		t.Fatalf("audit = %d %s", audit.StatusCode, audit.Body)
	}
	if strings.Contains(string(audit.Body), material) {
		t.Fatalf("audit leaked material: %s", audit.Body)
	}
	var page credit.AuditPage
	if err := json.Unmarshal(audit.Body, &page); err != nil || page.Total == 0 || page.Page != 1 {
		t.Fatalf("audit body = %s %v", audit.Body, err)
	}
	for _, item := range page.Items {
		if item.KeyID != key.ID {
			t.Fatalf("audit filter leaked: %+v", item)
		}
	}
}
