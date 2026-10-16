package credit

import (
	"errors"
	"testing"
)

// LookupRequest is the dedicated ownership-scoped lookup used by the customer
// self-service snapshot endpoint. It must return the owner's own request with
// its executions and manual adjustments, and must never reveal anything about
// another key.
func TestLookupRequestReturnsOwnRequest(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	other, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()

	measured := h.mustReserve(ReserveInput{ID: "req-own", TraceID: "trace-own", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 100})
	h.mustUsage(usageFor("exec-own", measured, "m1", tokens(100, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: measured.ID, TraceID: measured.TraceID, Outcome: OutcomeSucceeded})

	manual := h.mustReserve(ReserveInput{ID: "req-manual", TraceID: "trace-manual", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 500})
	h.mustComplete(Completion{ID: manual.ID, TraceID: manual.TraceID, Outcome: OutcomeFailed, StatusCode: 502})
	if err := h.store.Resolve(ctx, manual.ID, Resolution{Action: ActionCharge,
		AmountMicroUSD: 777, Reason: "provider billed out of band"}); err != nil {
		t.Fatalf("resolve charge: %v", err)
	}

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
	if got.AdjustmentMicroUSD != 0 {
		t.Fatalf("measured adjustment = %d, want 0", got.AdjustmentMicroUSD)
	}

	adjusted, err := h.store.LookupRequest(ctx, key.ID, manual.ID)
	if err != nil {
		t.Fatalf("lookup adjusted request: %v", err)
	}
	if adjusted.AdjustmentMicroUSD != 777 {
		t.Fatalf("adjusted request adjustment = %d, want 777", adjusted.AdjustmentMicroUSD)
	}

	// The same request is invisible to every other key: a foreign id and an
	// unknown id are the exact same ErrNotFound, so the lookup is no oracle.
	for _, tc := range []struct {
		name    string
		keyID   string
		request string
	}{
		{"foreign-key", other.ID, measured.ID},
		{"unknown-request", key.ID, "req-missing"},
	} {
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
