package credit

import (
	"errors"
	"testing"
)

// LookupRequest is the dedicated ownership-scoped lookup used by the customer
// self-service snapshot endpoint. It must return the owner's own request with
// each execution priced by the current price book, and must never reveal
// anything about another key.
func TestLookupRequestReturnsOwnRequest(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	other, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()

	measured := h.mustAdmit(AdmitInput{ID: "req-own", TraceID: "trace-own", KeyID: key.ID,
		RequestedModel: "m1"})
	h.mustUsage(usageFor("exec-own", measured, "m1", tokens(100, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: measured.ID, TraceID: measured.TraceID, Outcome: OutcomeSucceeded})

	unpriced := h.mustAdmit(AdmitInput{ID: "req-unpriced", TraceID: "trace-unpriced", KeyID: key.ID,
		RequestedModel: "unknown-model"})
	h.mustUsage(usageFor("exec-unpriced", unpriced, "unknown-model", tokens(7, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: unpriced.ID, TraceID: unpriced.TraceID, Outcome: OutcomeSucceeded})

	got, err := h.store.LookupRequest(ctx, key.ID, measured.ID)
	if err != nil {
		t.Fatalf("lookup own request: %v", err)
	}
	if got.ID != measured.ID || got.KeyID != key.ID || len(got.Executions) != 1 {
		t.Fatalf("lookup own request = %+v", got)
	}
	if got.Executions[0].ID != "exec-own" {
		t.Fatalf("execution id = %q, want exec-own", got.Executions[0].ID)
	}
	if got.Executions[0].CostMicroUSD == nil || *got.Executions[0].CostMicroUSD != 100 {
		t.Fatalf("measured cost = %v, want 100", got.Executions[0].CostMicroUSD)
	}
	if got.Executions[0].Tokens.Total != 100 {
		t.Fatalf("measured tokens = %+v, want 100", got.Executions[0].Tokens)
	}

	// The same log is repriced by the current price book on every lookup: no
	// amount was frozen at admission.
	h.mustPrice("m1", Rates{Input: 2 * million})
	repriced, err := h.store.LookupRequest(ctx, key.ID, measured.ID)
	if err != nil {
		t.Fatalf("repriced lookup: %v", err)
	}
	if c := repriced.Executions[0].CostMicroUSD; c == nil || *c != 200 {
		t.Fatalf("repriced cost = %v, want 200", c)
	}

	// A model without a price is still a valid execution: its cost is explicit
	// null and its tokens remain visible.
	unknown, err := h.store.LookupRequest(ctx, key.ID, unpriced.ID)
	if err != nil {
		t.Fatalf("lookup unpriced request: %v", err)
	}
	if len(unknown.Executions) != 1 || unknown.Executions[0].CostMicroUSD != nil {
		t.Fatalf("unpriced execution = %+v, want a null cost", unknown.Executions)
	}
	if unknown.Executions[0].Tokens.Total != 7 {
		t.Fatalf("unpriced tokens = %+v, want the recorded 7", unknown.Executions[0].Tokens)
	}

	// The same request is invisible to every other key: a foreign id and an
	// unknown id are the exact same ErrNotFound, so the lookup is no oracle.
	for _, tc := range []struct {
		name    string
		keyID   string
		request string
	}{
		{"foreign-key", other.ID, measured.ID},
		{"unknown-request", key.ID, "req-missing"}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.store.LookupRequest(ctx, tc.keyID, tc.request); !errors.Is(err, ErrNotFound) {
				t.Fatalf("lookup err = %v, want ErrNotFound", err)
			}
		})
	}

	// Empty identifiers are rejected before any query runs.
	for _, tc := range []struct{ keyID, request string }{{"", measured.ID}, {key.ID, ""}, {"  ", "  "}} {
		if _, err := h.store.LookupRequest(ctx, tc.keyID, tc.request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("lookup(%q,%q) err = %v, want ErrInvalid", tc.keyID, tc.request, err)
		}
	}
}
