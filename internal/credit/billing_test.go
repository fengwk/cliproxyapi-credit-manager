package credit

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

const (
	million = 1_000_000
	day     = 24 * time.Hour
)

func billingHarness(t *testing.T, opts ...func(*Options)) (*harness, Key) {
	t.Helper()
	h := newHarness(t, opts...)
	h.mustPrice("m1", Rates{Input: million, Output: 2 * million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	return h, key
}

func TestReserveAdmissionRules(t *testing.T) {
	h, key := billingHarness(t)
	ctx := h.ctx()
	// Unknown model has no price: absence denies, it never means free.
	_, err := h.store.Reserve(ctx, ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "unknown"})
	if !errors.Is(err, ErrUnpriced) {
		t.Fatalf("unpriced err = %v, want ErrUnpriced", err)
	}
	// Model permission is exact.
	spec := key.Spec
	spec.AllowedModels = []string{"m1"}
	if _, err := h.store.UpdateKey(ctx, key.ID, spec); err != nil {
		t.Fatalf("update key: %v", err)
	}
	h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1"}); err != nil {
		t.Fatalf("allowed model: %v", err)
	}
	spec.AllowedModels = []string{"m2"}
	if _, err := h.store.UpdateKey(ctx, key.ID, spec); err != nil {
		t.Fatalf("update key: %v", err)
	}
	_, err = h.store.Reserve(ctx, ReserveInput{ID: "r3", TraceID: "t3", KeyID: key.ID, RequestedModel: "m1"})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("disallowed model err = %v, want ErrDenied", err)
	}
	// Disabled and expired keys are unauthorized.
	spec.AllowedModels = nil
	spec.Enabled = false
	if _, err := h.store.UpdateKey(ctx, key.ID, spec); err != nil {
		t.Fatalf("update key: %v", err)
	}
	_, err = h.store.Reserve(ctx, ReserveInput{ID: "r4", TraceID: "t4", KeyID: key.ID, RequestedModel: "m1"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled err = %v, want ErrUnauthorized", err)
	}
	spec.Enabled = true
	past := testEpoch.Add(-time.Hour)
	spec.ExpiresAt = &past
	if _, err := h.store.UpdateKey(ctx, key.ID, spec); err != nil {
		t.Fatalf("update key: %v", err)
	}
	_, err = h.store.Reserve(ctx, ReserveInput{ID: "r5", TraceID: "t5", KeyID: key.ID, RequestedModel: "m1"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired err = %v, want ErrUnauthorized", err)
	}
	// Invalid input is rejected before touching state.
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "", TraceID: "t", KeyID: key.ID, RequestedModel: "m1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty id err = %v, want ErrInvalid", err)
	}
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "x", TraceID: "t", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative estimate err = %v, want ErrInvalid", err)
	}
}

func TestReserveIdempotentAndConflicting(t *testing.T) {
	h, key := billingHarness(t)
	in := ReserveInput{ID: "req-1", TraceID: "trace-1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 10}
	first := h.mustReserve(in)
	second := h.mustReserve(in)
	if first.ID != second.ID || first.HeldMicroUSD != second.HeldMicroUSD {
		t.Fatalf("idempotent reserve mismatch: %+v vs %+v", first, second)
	}
	// The hold must not be double counted.
	if got := h.key(key.ID).HeldMicroUSD; got != first.HeldMicroUSD {
		t.Fatalf("held = %d, want %d", got, first.HeldMicroUSD)
	}
	conflict := in
	conflict.TraceID = "other-trace"
	if _, err := h.store.Reserve(h.ctx(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting id err = %v, want ErrConflict", err)
	}
	conflict = in
	conflict.RequestedModel = "m2"
	if _, err := h.store.Reserve(h.ctx(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting model err = %v, want ErrConflict", err)
	}
	// A trace may not be reused by another lifecycle id.
	other := in
	other.ID = "req-2"
	if _, err := h.store.Reserve(h.ctx(), other); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused trace err = %v, want ErrConflict", err)
	}
}

func TestReserveQuotaConcurrencyBacklog(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxPendingRequests = 2 })
	h.mustPrice("m1", Rates{Input: million, Output: million})
	key, _ := h.mustKey(KeySpec{Enabled: true, QuotaMicroUSD: 10, MaxConcurrent: 1})
	ctx := h.ctx()
	// Estimate of 10 tokens = 20 microUSD > quota.
	_, err := h.store.Reserve(ctx, ReserveInput{ID: "q1", TraceID: "tq1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 10})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("quota err = %v, want ErrQuota", err)
	}
	// Exactly at quota is admitted (limits are exceeded only when over).
	admitted := h.mustReserve(ReserveInput{ID: "q2", TraceID: "tq2", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 5, OutputEstimate: 5})
	if admitted.HeldMicroUSD != 10 {
		t.Fatalf("held = %d, want 10", admitted.HeldMicroUSD)
	}
	// The same key is now at its concurrency limit.
	_, err = h.store.Reserve(ctx, ReserveInput{ID: "q3", TraceID: "tq3", KeyID: key.ID, RequestedModel: "m1"})
	if !errors.Is(err, ErrConcurrency) {
		t.Fatalf("concurrency err = %v, want ErrConcurrency", err)
	}
	// A key without a concurrency limit is still bounded by unresolved backlog.
	backlogKey, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustReserve(ReserveInput{ID: "b1", TraceID: "tb1", KeyID: backlogKey.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: "b1", TraceID: "tb1", Outcome: OutcomeFailed, StatusCode: 500})
	h.mustReserve(ReserveInput{ID: "b2", TraceID: "tb2", KeyID: backlogKey.ID, RequestedModel: "m1"})
	_, err = h.store.Reserve(ctx, ReserveInput{ID: "b3", TraceID: "tb3", KeyID: backlogKey.ID, RequestedModel: "m1"})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("backlog err = %v, want ErrPending", err)
	}
}

func TestReserveReleasesConcurrencyOnlyOnce(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	// Idempotent completion keeps the pending state and the hold.
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	got := h.request(req.ID)
	if got.State != statePending {
		t.Fatalf("state = %q, want pending", got.State)
	}
	if err := h.store.Complete(h.ctx(), Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeFailed}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting outcome err = %v, want ErrConflict", err)
	}
	if err := h.store.Complete(h.ctx(), Completion{ID: req.ID, TraceID: "wrong", Outcome: OutcomeSucceeded}); !errors.Is(err, ErrConflict) {
		t.Fatalf("trace mismatch err = %v, want ErrConflict", err)
	}
	if err := h.store.Complete(h.ctx(), Completion{ID: "missing", Outcome: OutcomeSucceeded}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing request err = %v, want ErrNotFound", err)
	}
	if err := h.store.Complete(h.ctx(), Completion{ID: req.ID, Outcome: "maybe"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad outcome err = %v, want ErrInvalid", err)
	}
}

func TestUsageBeforeCompletionIsChargedOnce(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100, OutputEstimate: 100})
	if req.HeldMicroUSD != 300 {
		t.Fatalf("hold = %d, want 300", req.HeldMicroUSD)
	}
	// Usage arrives first, while the execution is still active.
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 100, 0)))
	if got := h.key(key.ID); got.SpentMicroUSD != 0 || got.HeldMicroUSD != 300 {
		t.Fatalf("active accounting = spent %d held %d, want 0/300", got.SpentMicroUSD, got.HeldMicroUSD)
	}
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	got := h.key(key.ID)
	if got.SpentMicroUSD != 300 || got.HeldMicroUSD != 0 {
		t.Fatalf("settled accounting = spent %d held %d, want 300/0", got.SpentMicroUSD, got.HeldMicroUSD)
	}
	// A closed request no longer reports a hold.
	if settled := h.request(req.ID); settled.HeldMicroUSD != 0 {
		t.Fatalf("settled request hold = %d, want 0", settled.HeldMicroUSD)
	}
	// A duplicate event must not bill twice.
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 100, 0)))
	if got := h.key(key.ID); got.SpentMicroUSD != 300 {
		t.Fatalf("after replay spent = %d, want 300", got.SpentMicroUSD)
	}
	// Cost exceeding the hold is still charged in full.
	extra := usageFor("u2", req, "m1", tokens(0, 0, 0, 1000, 0))
	extra.Provider = "openai"
	h.mustUsage(extra)
	if got := h.key(key.ID); got.SpentMicroUSD != 300+2000 {
		t.Fatalf("late charge spent = %d, want 2300", got.SpentMicroUSD)
	}
}

func TestUsageAfterCompletionSettlesAndUnknownStaysPending(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100, OutputEstimate: 100})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state without usage = %q, want pending", got.State)
	}
	if held := h.key(key.ID).HeldMicroUSD; held != 300 {
		t.Fatalf("hold after completion without usage = %d, want 300", held)
	}
	// Unknown accounting must not be billed as zero.
	unknown := usageFor("u-unknown", req, "m1", Tokens{})
	unknown.Quality = QualityUnknown
	h.mustUsage(unknown)
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state after unknown usage = %q, want pending", got.State)
	}
	if got := h.key(key.ID).SpentMicroUSD; got != 0 {
		t.Fatalf("unknown usage charged %d, want 0", got)
	}
	// A later reliable usage closes the request with the known amount only.
	h.mustUsage(usageFor("u-known", req, "m1", tokens(100, 0, 0, 100, 0)))
	got := h.request(req.ID)
	if got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	if spent := h.key(key.ID).SpentMicroUSD; spent != 300 {
		t.Fatalf("spent = %d, want 300", spent)
	}
	if len(got.Executions) != 2 {
		t.Fatalf("executions = %d, want 2", len(got.Executions))
	}
	for _, e := range got.Executions {
		switch e.ID {
		case "u-unknown":
			if e.CostMicroUSD != nil {
				t.Fatalf("unknown execution must keep an unknown cost, got %d", *e.CostMicroUSD)
			}
		case "u-known":
			if e.CostMicroUSD == nil || *e.CostMicroUSD != 300 {
				t.Fatalf("known execution cost = %v", e.CostMicroUSD)
			}
		}
	}
}

func TestFailedOutcomeSettlementRules(t *testing.T) {
	h, key := billingHarness(t)
	// A 5xx failure with no usage might still have consumed tokens: stay pending.
	r1 := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 10})
	h.mustComplete(Completion{ID: r1.ID, TraceID: r1.TraceID, Outcome: OutcomeFailed, StatusCode: 500})
	if got := h.request(r1.ID); got.State != statePending {
		t.Fatalf("5xx failure state = %q, want pending", got.State)
	}
	// A proven 4xx rejection closes with no charge.
	r2 := h.mustReserve(ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 10})
	h.mustComplete(Completion{ID: r2.ID, TraceID: r2.TraceID, Outcome: OutcomeFailed, StatusCode: 429})
	if got := h.request(r2.ID); got.State != stateSettled {
		t.Fatalf("rejected state = %q, want settled", got.State)
	}
	if held := h.key(key.ID).HeldMicroUSD; held != 30 {
		t.Fatalf("held = %d, want only the unresolved request's 30", held)
	}
	// A failed execution that reported real usage is charged.
	r3 := h.mustReserve(ReserveInput{ID: "r3", TraceID: "t3", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 10})
	failed := usageFor("u-failed", r3, "m1", tokens(100, 0, 0, 100, 0))
	failed.Failed = true
	failed.Quality = ""
	failed.StatusCode = 500
	h.mustUsage(failed)
	h.mustComplete(Completion{ID: r3.ID, TraceID: r3.TraceID, Outcome: OutcomeFailed, StatusCode: 500})
	if got := h.request(r3.ID); got.State != stateSettled {
		t.Fatalf("failed-with-usage state = %q, want settled", got.State)
	}
	if spent := h.key(key.ID).SpentMicroUSD; spent != 300 {
		t.Fatalf("spent = %d, want 300", spent)
	}
	// A failed execution with zero tokens and no quality stays unknown.
	r4 := h.mustReserve(ReserveInput{ID: "r4", TraceID: "t4", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 10})
	zero := usageFor("u-zero", r4, "m1", Tokens{})
	zero.Failed = true
	zero.Quality = QualityUnknown
	h.mustUsage(zero)
	h.mustComplete(Completion{ID: r4.ID, TraceID: r4.TraceID, Outcome: OutcomeFailed, StatusCode: 500})
	if got := h.request(r4.ID); got.State != statePending {
		t.Fatalf("failed-zero state = %q, want pending", got.State)
	}
}

func TestUsageMatchingRules(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	ctx := h.ctx()
	// Unknown trace must never create a request.
	orphan := usageFor("u-orphan", req, "m1", tokens(1, 0, 0, 1, 0))
	orphan.TraceID = "no-such-trace"
	if err := h.store.RecordUsage(ctx, orphan); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan err = %v, want ErrNotFound", err)
	}
	// Wrong key for a stored trace is rejected.
	wrongKey := usageFor("u-wrong-key", req, "m1", tokens(1, 0, 0, 1, 0))
	wrongKey.KeyID = "someone-else"
	if err := h.store.RecordUsage(ctx, wrongKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong key err = %v, want ErrNotFound", err)
	}
	// A mismatching request id is a conflict.
	mismatch := usageFor("u-mismatch", req, "m1", tokens(1, 0, 0, 1, 0))
	mismatch.RequestID = "other"
	if err := h.store.RecordUsage(ctx, mismatch); !errors.Is(err, ErrConflict) {
		t.Fatalf("request mismatch err = %v, want ErrConflict", err)
	}
	// Same id with different content is a conflict, same content is a no-op.
	first := usageFor("u1", req, "m1", tokens(10, 0, 0, 10, 0))
	h.mustUsage(first)
	changed := first
	changed.Tokens = tokens(11, 0, 0, 10, 0)
	if err := h.store.RecordUsage(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("content conflict err = %v, want ErrConflict", err)
	}
	// Missing identity is invalid.
	if err := h.store.RecordUsage(ctx, Usage{RequestID: req.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing id err = %v, want ErrInvalid", err)
	}
	if err := h.store.RecordUsage(ctx, usageFor("u2", req, "m1", Tokens{Input: -1, Total: -1})); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative tokens err = %v, want ErrInvalid", err)
	}
}

func TestResolveReleaseAndCharge(t *testing.T) {
	h, key := billingHarness(t)
	ctx := h.ctx()
	release := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100, OutputEstimate: 100})
	if err := h.store.Resolve(ctx, release.ID, Resolution{Action: ActionRelease, Reason: ""}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing reason err = %v, want ErrInvalid", err)
	}
	if err := h.store.Resolve(ctx, release.ID, Resolution{Action: "refund", Reason: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad action err = %v, want ErrInvalid", err)
	}
	if err := h.store.Resolve(ctx, release.ID, Resolution{Action: ActionRelease, Reason: "provider outage"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	got := h.request(release.ID)
	if got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	if k := h.key(key.ID); k.SpentMicroUSD != 0 || k.HeldMicroUSD != 0 {
		t.Fatalf("after release spent %d held %d, want 0/0", k.SpentMicroUSD, k.HeldMicroUSD)
	}
	// A second resolve on a closed request is a conflict, not a silent reprice.
	if err := h.store.Resolve(ctx, release.ID, Resolution{Action: ActionRelease, Reason: "again"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat resolve err = %v, want ErrConflict", err)
	}
	charge := h.mustReserve(ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 10})
	if err := h.store.Resolve(ctx, charge.ID, Resolution{Action: ActionCharge, AmountMicroUSD: 777, Reason: "manual"}); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if k := h.key(key.ID); k.SpentMicroUSD != 777 || k.HeldMicroUSD != 0 {
		t.Fatalf("after charge spent %d held %d, want 777/0", k.SpentMicroUSD, k.HeldMicroUSD)
	}
	var count int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM adjustments WHERE request_id = ?`, charge.ID).Scan(&count); err != nil {
		t.Fatalf("count adjustments: %v", err)
	}
	if count != 1 {
		t.Fatalf("adjustments = %d, want 1", count)
	}
	// A released request that already reported usage still charges that usage.
	partial := h.mustReserve(ReserveInput{ID: "r3", TraceID: "t3", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u3", partial, "m1", tokens(100, 0, 0, 0, 0)))
	if err := h.store.Resolve(ctx, partial.ID, Resolution{Action: ActionRelease, Reason: "partial"}); err != nil {
		t.Fatalf("release partial: %v", err)
	}
	if k := h.key(key.ID); k.SpentMicroUSD != 777+100 {
		t.Fatalf("spent = %d, want 877", k.SpentMicroUSD)
	}
}

// TestLateUsageChargesAdmissionPeriod verifies counters follow the admission
// period, not the time the late event arrived.
func TestLateUsageChargesAdmissionPeriod(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		At: testEpoch, InputEstimate: 10, OutputEstimate: 10})
	// The event arrives in the next month.
	h.clock.set(testEpoch.AddDate(0, 1, 0))
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})

	june, err := moneyCounter(h.ctx(), h.store.db, key.ID, windowMonth, "2026-06")
	if err != nil {
		t.Fatalf("june counter: %v", err)
	}
	if june != 100 {
		t.Fatalf("june charge = %d, want 100", june)
	}
	july, err := moneyCounter(h.ctx(), h.store.db, key.ID, windowMonth, "2026-07")
	if err != nil {
		t.Fatalf("july counter: %v", err)
	}
	if july != 0 {
		t.Fatalf("july charge = %d, want 0", july)
	}
	total, err := moneyCounter(h.ctx(), h.store.db, key.ID, windowTotal, "")
	if err != nil {
		t.Fatalf("total counter: %v", err)
	}
	if total != 100 {
		t.Fatalf("total charge = %d, want 100", total)
	}
	dayCounter, err := moneyCounter(h.ctx(), h.store.db, key.ID, windowDay, "2026-06-15")
	if err != nil {
		t.Fatalf("day counter: %v", err)
	}
	if dayCounter != 100 {
		t.Fatalf("day charge = %d, want 100", dayCounter)
	}
	weekCounter, err := moneyCounter(h.ctx(), h.store.db, key.ID, windowWeek, weekPeriod(testEpoch))
	if err != nil {
		t.Fatalf("week counter: %v", err)
	}
	if weekCounter != 100 {
		t.Fatalf("week charge = %d, want 100", weekCounter)
	}
}

func TestPeriodLimitsResetAcrossPeriods(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million, Output: million})
	key, _ := h.mustKey(KeySpec{Enabled: true, DailyMicroUSD: 100})
	ctx := h.ctx()
	spend := func(id, trace string, amount int64) error {
		req, err := h.store.Reserve(ctx, ReserveInput{ID: id, TraceID: trace, KeyID: key.ID,
			RequestedModel: "m1", InputEstimate: amount, OutputEstimate: 0})
		if err != nil {
			return err
		}
		// Settle with the real charge so the daily counter is consumed.
		h.mustUsage(usageFor("u-"+id, req, "m1", tokens(amount, 0, 0, 0, 0)))
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
		return nil
	}
	if err := spend("r1", "t1", 100); err != nil {
		t.Fatalf("first spend: %v", err)
	}
	if err := spend("r2", "t2", 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("second spend err = %v, want ErrQuota", err)
	}
	// The next UTC day starts with a fresh window.
	h.clock.set(testEpoch.AddDate(0, 0, 1))
	if err := spend("r3", "t3", 100); err != nil {
		t.Fatalf("next day spend: %v", err)
	}
	if err := spend("r4", "t4", 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("same-day spend err = %v, want ErrQuota", err)
	}
}

func TestModelTokenLimits(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	h.mustPrice("m2", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{
		Enabled:          true,
		ModelTokenLimits: []ModelTokenLimit{{Model: "m1", Daily: 150}},
	})
	ctx := h.ctx()
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100, OutputEstimate: 0})
	// The admission estimate consumes budget before any usage is reported.
	_, err := h.store.Reserve(ctx, ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100, OutputEstimate: 0})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("estimate over limit err = %v, want ErrQuota", err)
	}
	// Another model is unaffected.
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "r3", TraceID: "t3", KeyID: key.ID, RequestedModel: "m2"}); err != nil {
		t.Fatalf("other model: %v", err)
	}
	// Reported usage moves the estimate into durable counters.
	h.mustUsage(usageFor("u1", req, "m1", tokens(150, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	used, err := tokenCounter(ctx, h.store.db, key.ID, "m1", windowDay, dayPeriod(testEpoch))
	if err != nil {
		t.Fatalf("token counter: %v", err)
	}
	if used != 150 {
		t.Fatalf("token counter = %d, want 150", used)
	}
	_, err = h.store.Reserve(ctx, ReserveInput{ID: "r4", TraceID: "t4", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 1, OutputEstimate: 0})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("consumed over limit err = %v, want ErrQuota", err)
	}
	// The next day is a new window.
	h.clock.set(testEpoch.AddDate(0, 0, 1))
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "r5", TraceID: "t5", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100, OutputEstimate: 0}); err != nil {
		t.Fatalf("next day reserve: %v", err)
	}
}

func TestConcurrentReserveHonoursConcurrencyLimit(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true, MaxConcurrent: 2})
	const attempts = 12
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := h.store.Reserve(context.Background(), ReserveInput{
				ID:             "req-" + itoa(i),
				TraceID:        "trace-" + itoa(i),
				KeyID:          key.ID,
				RequestedModel: "m1",
			})
			results[i] = err
		}(i)
	}
	wg.Wait()
	ok, limited := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrConcurrency):
			limited++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 2 || limited != attempts-2 {
		t.Fatalf("admitted %d, limited %d; want 2/%d", ok, limited, attempts-2)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// TestRepricingDoesNotReBillHistory proves the request price snapshot is used
// for billing: a later price change must not silently reprice in-flight work.
func TestRepricingDoesNotReBillHistory(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100, OutputEstimate: 100})
	h.mustPrice("m1", Rates{Input: 50 * million, Output: 60 * million})
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 100, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.key(key.ID).SpentMicroUSD; got != 300 {
		t.Fatalf("spent after repricing = %d, want the original 300", got)
	}
	settled := h.request(req.ID)
	if settled.Price.Rates.Input != million || settled.Price.Version != 1 {
		t.Fatalf("price snapshot = %+v", settled.Price)
	}
}

// TestBusyDatabaseIsObservable ensures a locked database surfaces an error
// instead of looking like a successful admission.
func TestBusyDatabaseIsObservable(t *testing.T) {
	h, key := billingHarness(t)
	contender, err := sql.Open("sqlite", dsnFor(h.store.dbPath))
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	defer contender.Close()
	contender.SetMaxOpenConns(1)
	conn, err := contender.Conn(context.Background())
	if err != nil {
		t.Fatalf("contender conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("take exclusive lock: %v", err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = h.store.Reserve(ctx, ReserveInput{ID: "busy", TraceID: "t-busy", KeyID: key.ID, RequestedModel: "m1"})
	if err == nil {
		t.Fatal("reserve succeeded against a locked database")
	}
	var count int
	_ = conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM requests`).Scan(&count)
	if count != 0 {
		t.Fatalf("requests after failed reserve = %d, want 0", count)
	}
}
