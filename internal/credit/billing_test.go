package credit

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	// Usage arrives first, while the execution is still active. A trustworthy
	// charge is booked immediately; only the hold waits for completion.
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 100, 0)))
	if got := h.key(key.ID); got.SpentMicroUSD != 300 || got.HeldMicroUSD != 300 {
		t.Fatalf("active accounting = spent %d held %d, want 300/300", got.SpentMicroUSD, got.HeldMicroUSD)
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

// TestUnknownExecutionBlocksClosureButStillCharges proves that an unreliable
// execution keeps the request (and its remaining hold) open while every
// trustworthy charge is booked immediately and never written off twice.
func TestUnknownExecutionBlocksClosureButStillCharges(t *testing.T) {
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
	// Unknown accounting is recorded but never billed as a zero.
	unknown := usageFor("u-unknown", req, "m1", Tokens{})
	unknown.Quality = QualityUnknown
	h.mustUsage(unknown)
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state after unknown usage = %q, want pending", got.State)
	}
	if got := h.key(key.ID).SpentMicroUSD; got != 0 {
		t.Fatalf("unknown usage charged %d, want 0", got)
	}
	// A trustworthy usage charges immediately but the unknown sibling still
	// blocks closure, so the remaining hold is kept instead of written off.
	h.mustUsage(usageFor("u-known", req, "m1", tokens(100, 0, 0, 100, 0)))
	got := h.request(req.ID)
	if got.State != statePending {
		t.Fatalf("state with unknown execution = %q, want pending", got.State)
	}
	if spent := h.key(key.ID).SpentMicroUSD; spent != 300 {
		t.Fatalf("spent = %d, want the immediate 300", spent)
	}
	if held := h.key(key.ID).HeldMicroUSD; held != 300 {
		t.Fatalf("hold with unknown execution = %d, want 300 retained", held)
	}
	// A repeated completion must not change anything while the request is
	// unresolved, but it is still checked for agreement.
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state after replay = %q, want pending", got.State)
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
	// Only an administrator decision can close it; the charge stays booked.
	if err := h.store.Resolve(h.ctx(), req.ID, Resolution{Action: ActionRelease, Reason: "unknown accounting"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if k := h.key(key.ID); k.SpentMicroUSD != 300 || k.HeldMicroUSD != 0 {
		t.Fatalf("after resolve spent %d held %d, want 300/0", k.SpentMicroUSD, k.HeldMicroUSD)
	}
}

// TestUnknownExecutionOrderingAndIdempotency covers the event orders the native
// bridge can produce for one execution set.
func TestUnknownExecutionOrderingAndIdempotency(t *testing.T) {
	h, key := billingHarness(t)
	ctx := h.ctx()
	reserve := func(id string) Request {
		return h.mustReserve(ReserveInput{ID: id, TraceID: "t-" + id, KeyID: key.ID, RequestedModel: "m1",
			InputEstimate: 100, OutputEstimate: 100})
	}
	// Order A: unknown first, completion second, trustworthy usage last.
	a := reserve("a")
	unknownA := usageFor("ua", a, "m1", Tokens{})
	unknownA.Quality = QualityUnknown
	h.mustUsage(unknownA)
	h.mustComplete(Completion{ID: a.ID, TraceID: a.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(a.ID).State; got != statePending {
		t.Fatalf("order A state = %q, want pending", got)
	}
	h.mustUsage(usageFor("ua2", a, "m1", tokens(100, 0, 0, 100, 0)))
	if got := h.key(key.ID).SpentMicroUSD; got != 300 {
		t.Fatalf("order A spent = %d, want 300", got)
	}
	// Order B: unknown first, trustworthy second, completion last.
	b := reserve("b")
	unknownB := usageFor("ub", b, "m1", Tokens{})
	unknownB.Quality = QualityPartial
	h.mustUsage(unknownB)
	h.mustUsage(usageFor("ub2", b, "m1", tokens(100, 0, 0, 100, 0)))
	h.mustComplete(Completion{ID: b.ID, TraceID: b.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(b.ID).State; got != statePending {
		t.Fatalf("order B state = %q, want pending", got)
	}
	if got := h.key(key.ID).SpentMicroUSD; got != 600 {
		t.Fatalf("order B spent = %d, want 600", got)
	}
	// Both events are replayable without changing money.
	h.mustUsage(unknownA)
	h.mustUsage(usageFor("ua2", a, "m1", tokens(100, 0, 0, 100, 0)))
	h.mustUsage(unknownB)
	h.mustUsage(usageFor("ub2", b, "m1", tokens(100, 0, 0, 100, 0)))
	if got := h.key(key.ID).SpentMicroUSD; got != 600 {
		t.Fatalf("replay spent = %d, want 600", got)
	}
	// Resolving with a charge adds on top of the already booked usage.
	if err := h.store.Resolve(ctx, b.ID, Resolution{Action: ActionCharge, AmountMicroUSD: 50, Reason: "manual"}); err != nil {
		t.Fatalf("resolve charge: %v", err)
	}
	if got := h.key(key.ID).SpentMicroUSD; got != 650 {
		t.Fatalf("resolve charge spent = %d, want 650", got)
	}
}

// TestCompletionOutcomes pins down which outcomes may close a request without
// an execution event. A 4xx status is never proof on its own.
func TestCompletionOutcomes(t *testing.T) {
	h, key := billingHarness(t)
	ctx := h.ctx()
	reserve := func(id string) Request {
		return h.mustReserve(ReserveInput{ID: id, TraceID: "t-" + id, KeyID: key.ID, RequestedModel: "m1",
			InputEstimate: 10, OutputEstimate: 10})
	}
	closeOrNot := func(id, outcome string, status int, wantState string) Request {
		req := reserve(id)
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: outcome, StatusCode: status})
		got := h.request(id)
		if got.State != wantState {
			t.Fatalf("%s/%d state = %q, want %q", outcome, status, got.State, wantState)
		}
		return got
	}
	// Failures without evidence stay pending regardless of the status code.
	closeOrNot("f500", OutcomeFailed, 500, statePending)
	closeOrNot("f429", OutcomeFailed, 429, statePending)
	closeOrNot("f401", OutcomeFailed, 401, statePending)
	closeOrNot("f400", OutcomeFailed, 400, statePending)
	// A cancellation is treated like a failure: partial usage may be missing.
	closeOrNot("c001", OutcomeCanceled, 0, statePending)
	closeOrNot("c499", OutcomeCanceled, 499, statePending)
	// Only a proven rejection closes without usage, whatever the status.
	closeOrNot("r000", OutcomeRejected, 0, stateSettled)
	closeOrNot("r403", OutcomeRejected, 403, stateSettled)
	// A trustworthy usage closes failures and cancellations.
	failed := reserve("f-with-usage")
	failedUsage := usageFor("uf", failed, "m1", tokens(100, 0, 0, 0, 0))
	failedUsage.Failed = true
	h.mustUsage(failedUsage)
	h.mustComplete(Completion{ID: failed.ID, TraceID: failed.TraceID, Outcome: OutcomeFailed, StatusCode: 500})
	if got := h.request(failed.ID).State; got != stateSettled {
		t.Fatalf("failed with usage state = %q, want settled", got)
	}
	canceled := reserve("c-with-usage")
	h.mustUsage(usageFor("uc", canceled, "m1", tokens(50, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: canceled.ID, TraceID: canceled.TraceID, Outcome: OutcomeCanceled})
	if got := h.request(canceled.ID).State; got != stateSettled {
		t.Fatalf("canceled with usage state = %q, want settled", got)
	}
	// A rejection still charges what was reported.
	rejected := reserve("r-with-usage")
	h.mustUsage(usageFor("ur", rejected, "m1", tokens(10, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: rejected.ID, TraceID: rejected.TraceID, Outcome: OutcomeRejected, StatusCode: 403})
	if got := h.request(rejected.ID).State; got != stateSettled {
		t.Fatalf("rejected with usage state = %q, want settled", got)
	}
	// Unsupported outcomes are rejected outright.
	if err := h.store.Complete(ctx, Completion{ID: failed.ID, Outcome: "timedout"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown outcome err = %v, want ErrInvalid", err)
	}
	// Failed-zero with unknown accounting stays pending.
	unknownZero := reserve("f-zero")
	zero := usageFor("uz", unknownZero, "m1", Tokens{})
	zero.Failed = true
	zero.Quality = QualityUnknown
	h.mustUsage(zero)
	h.mustComplete(Completion{ID: unknownZero.ID, TraceID: unknownZero.TraceID, Outcome: OutcomeFailed, StatusCode: 400})
	if got := h.request(unknownZero.ID).State; got != statePending {
		t.Fatalf("failed-zero-unknown state = %q, want pending", got)
	}
	// An unaccounted execution contradicts a rejection claim, so the request
	// stays open for reconciliation instead of writing the hold off.
	rejectedUnknown := reserve("r-unknown")
	ru := usageFor("uru", rejectedUnknown, "m1", Tokens{})
	ru.Quality = QualityUnknown
	h.mustUsage(ru)
	h.mustComplete(Completion{ID: rejectedUnknown.ID, TraceID: rejectedUnknown.TraceID,
		Outcome: OutcomeRejected, StatusCode: 403})
	if got := h.request(rejectedUnknown.ID).State; got != statePending {
		t.Fatalf("rejected with unknown execution state = %q, want pending", got)
	}
	// Money is only booked for trustworthy usage: 100 + 50 + 10 above.
	if got := h.key(key.ID).SpentMicroUSD; got != 100+50+10 {
		t.Fatalf("spent = %d, want 160", got)
	}
}

// TestCompleteValidatesRepeatedOutcomeAndStatus rejects a repeated completion
// that disagrees with what was already recorded.
func TestCompleteValidatesRepeatedOutcomeAndStatus(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeFailed, StatusCode: 502})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeFailed, StatusCode: 502})
	if err := h.store.Complete(h.ctx(), Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeFailed, StatusCode: 503}); !errors.Is(err, ErrConflict) {
		t.Fatalf("status drift err = %v, want ErrConflict", err)
	}
	if err := h.store.Complete(h.ctx(), Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeCanceled, StatusCode: 502}); !errors.Is(err, ErrConflict) {
		t.Fatalf("outcome drift err = %v, want ErrConflict", err)
	}
	if got := h.request(req.ID).State; got != statePending {
		t.Fatalf("state = %q, want pending", got)
	}
}

// TestUsageRejectsCallerSuppliedCost keeps money and quality from disagreeing:
// the core always derives the cost from the request price snapshot.
func TestUsageRejectsCallerSuppliedCost(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	u := usageFor("u1", req, "m1", tokens(10, 0, 0, 10, 0))
	cost := int64(1)
	u.CostMicroUSD = &cost
	if err := h.store.RecordUsage(h.ctx(), u); !errors.Is(err, ErrInvalid) {
		t.Fatalf("caller cost err = %v, want ErrInvalid", err)
	}
	if got := h.key(key.ID).SpentMicroUSD; got != 0 {
		t.Fatalf("spent after rejected cost = %d, want 0", got)
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
	// An active execution still owns a concurrency slot, so resolving it would
	// bypass admission accounting.
	if err := h.store.Resolve(ctx, release.ID, Resolution{Action: ActionRelease, Reason: "too early"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("resolving an active request err = %v, want ErrConflict", err)
	}
	// 100 input + 100 output at the harness rates.
	if k := h.key(key.ID); k.HeldMicroUSD != 300 {
		t.Fatalf("hold after refused resolve = %d, want 300", k.HeldMicroUSD)
	}
	// A failed lifecycle without a trustworthy execution is what an
	// administrator actually has to rule on.
	h.mustComplete(Completion{ID: release.ID, TraceID: release.TraceID, Outcome: OutcomeFailed, StatusCode: 502})
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
	h.mustComplete(Completion{ID: charge.ID, TraceID: charge.TraceID, Outcome: OutcomeFailed, StatusCode: 502})
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
	// The unaccounted execution keeps the request pending, which is the only
	// state an administrator may still rule on.
	unaccounted := usageFor("u3-unknown", partial, "m1", Tokens{})
	unaccounted.Quality = QualityUnknown
	h.mustUsage(unaccounted)
	h.mustComplete(Completion{ID: partial.ID, TraceID: partial.TraceID, Outcome: OutcomeFailed, StatusCode: 500})
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

// TestUsageReplayWithEmptyRequestID covers the native bridge, which only
// supplies a trace id: replaying the same event must be idempotent even though
// the caller never learned the request id.
func TestUsageReplayWithEmptyRequestID(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	u := usageFor("u1", req, "m1", tokens(10, 0, 0, 10, 0))
	u.RequestID = ""
	h.mustUsage(u)
	h.mustUsage(u) // must be a no-op, not a conflict
	if got := h.key(key.ID).SpentMicroUSD; got != 30 {
		t.Fatalf("spent after replay = %d, want 30", got)
	}
	got := h.request(req.ID)
	if len(got.Executions) != 1 {
		t.Fatalf("executions = %d, want 1", len(got.Executions))
	}
	// The stored event is canonicalized onto the matching request.
	if got.Executions[0].RequestID != req.ID {
		t.Fatalf("stored request id = %q, want %q", got.Executions[0].RequestID, req.ID)
	}
	// An explicitly disagreeing id is still a conflict.
	bad := u
	bad.ID = "u2"
	bad.RequestID = "elsewhere"
	if err := h.store.RecordUsage(h.ctx(), bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched request id err = %v, want ErrConflict", err)
	}
}

// TestHasActive exposes in-flight work so the native layer can refuse to close
// and reopen the database while business requests are running.
func TestHasActive(t *testing.T) {
	h, key := billingHarness(t)
	active, err := h.store.HasActive(h.ctx())
	if err != nil || active {
		t.Fatalf("initial HasActive = %v, %v", active, err)
	}
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	if active, err = h.store.HasActive(h.ctx()); err != nil || !active {
		t.Fatalf("HasActive during execution = %v, %v", active, err)
	}
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeRejected, StatusCode: 401})
	if active, err = h.store.HasActive(h.ctx()); err != nil || active {
		t.Fatalf("HasActive after completion = %v, %v", active, err)
	}
}

// TestSecondInstanceCannotOwnLockedDirectory proves the directory lock keeps a
// second process from running recovery over live executions.
func TestSecondInstanceCannotOwnLockedDirectory(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustReserve(ReserveInput{ID: "live", TraceID: "t-live", KeyID: key.ID, RequestedModel: "m1"})

	second, err := Open(Options{DataDir: h.dir, Now: h.clock.now})
	if !errors.Is(err, ErrLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second open err = %v, want ErrLocked", err)
	}
	if second != nil {
		t.Fatal("second open returned a store for a locked directory")
	}
	// The refused instance must not have marked the live execution pending.
	if got := h.request("live").State; got != stateActive {
		t.Fatalf("live request state = %q, want active", got)
	}
	// Once the owner releases the directory, a new instance takes over and
	// performs the restart transition exactly once.
	if err := h.store.Close(); err != nil {
		t.Fatalf("close owner: %v", err)
	}
	third, err := Open(Options{DataDir: h.dir, Now: h.clock.now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer third.Close()
	rep, err := third.Report(h.ctx(), Filter{From: testEpoch.AddDate(-1, 0, 0),
		To: h.clock.now().Add(time.Hour), PageSize: 10})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Items) != 1 || rep.Items[0].State != statePending {
		t.Fatalf("items after restart = %+v", rep.Items)
	}
}

// TestZeroRatePriceStillRequiresUsage keeps "free" distinct from "unaccounted":
// an all-zero rate is a real price, not evidence that nothing ran.
func TestZeroRatePriceStillRequiresUsage(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("free", Rates{})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "free",
		InputEstimate: 100, OutputEstimate: 100})
	if req.HeldMicroUSD != 0 {
		t.Fatalf("free model hold = %d, want 0", req.HeldMicroUSD)
	}
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID).State; got != statePending {
		t.Fatalf("free success without usage state = %q, want pending", got)
	}
	// An explicit complete declaration closes it at zero cost.
	u := usageFor("u1", req, "free", Tokens{})
	u.Quality = QualityComplete
	h.mustUsage(u)
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	if got := h.key(key.ID).SpentMicroUSD; got != 0 {
		t.Fatalf("spent = %d, want 0", got)
	}
}

// TestPeriodLimitsCountUnresolvedHolds proves that a hold which is already
// admitted in the current period blocks further admissions, so two concurrent
// requests cannot jointly exceed a daily/weekly/monthly monetary limit while
// neither has been charged yet.
func TestPeriodLimitsCountUnresolvedHolds(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*KeySpec)
	}{
		{"daily", func(s *KeySpec) { s.DailyMicroUSD = 100 }},
		{"weekly", func(s *KeySpec) { s.WeeklyMicroUSD = 100 }},
		{"monthly", func(s *KeySpec) { s.MonthlyMicroUSD = 100 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustPrice("m1", Rates{Input: million}) // 1 token costs 1 microUSD
			spec := KeySpec{Enabled: true}
			tc.apply(&spec)
			key, _ := h.mustKey(spec)
			ctx := h.ctx()

			first := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
				RequestedModel: "m1", InputEstimate: 60})
			if first.HeldMicroUSD != 60 {
				t.Fatalf("hold = %d, want 60", first.HeldMicroUSD)
			}
			// Nothing has been charged yet, yet the window is already occupied.
			_, err := h.store.Reserve(ctx, ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
				RequestedModel: "m1", InputEstimate: 60})
			if !errors.Is(err, ErrQuota) {
				t.Fatalf("second hold err = %v, want ErrQuota", err)
			}
			// An admission that still fits exactly is admitted.
			if _, err := h.store.Reserve(ctx, ReserveInput{ID: "r3", TraceID: "t3", KeyID: key.ID,
				RequestedModel: "m1", InputEstimate: 40}); err != nil {
				t.Fatalf("hold exactly at the limit: %v", err)
			}
			// Closing the first request releases its hold in the same period.
			h.mustComplete(Completion{ID: first.ID, TraceID: first.TraceID, Outcome: OutcomeRejected})
			if k := h.key(key.ID); k.HeldMicroUSD != 40 {
				t.Fatalf("hold after release = %d, want 40", k.HeldMicroUSD)
			}
			if _, err := h.store.Reserve(ctx, ReserveInput{ID: "r4", TraceID: "t4", KeyID: key.ID,
				RequestedModel: "m1", InputEstimate: 60}); err != nil {
				t.Fatalf("hold after release should fit: %v", err)
			}
		})
	}
}

// TestPeriodLimitsIgnoreHoldsFromOtherPeriods keeps the same-period rule from
// overcorrecting: work admitted in an earlier period keeps its hold, but it
// does not occupy the next period's monetary window.
func TestPeriodLimitsIgnoreHoldsFromOtherPeriods(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true, MonthlyMicroUSD: 100})
	jan := time.Date(2026, time.January, 10, 12, 0, 0, 0, time.UTC)
	h.clock.set(jan)
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 100, At: jan}); err != nil {
		t.Fatalf("january reserve: %v", err)
	}
	feb := time.Date(2026, time.February, 3, 9, 0, 0, 0, time.UTC)
	h.clock.set(feb)
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 100, At: feb}); err != nil {
		t.Fatalf("february reserve: %v", err)
	}
	// The old hold is still unresolved and still counted by the lifetime window.
	if k := h.key(key.ID); k.HeldMicroUSD != 200 {
		t.Fatalf("held = %d, want both holds retained", k.HeldMicroUSD)
	}
}

// TestTokenEstimatesArePeriodScoped makes sure an unresolved request only
// consumes the token window it was admitted in, while the lifetime window keeps
// counting it.
func TestTokenEstimatesArePeriodScoped(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	ctx := h.ctx()
	jan := time.Date(2026, time.January, 10, 12, 0, 0, 0, time.UTC)
	feb := time.Date(2026, time.February, 3, 9, 0, 0, 0, time.UTC)

	// Period window: the January estimate must not starve February.
	h.clock.set(jan)
	scoped, _ := h.mustKey(KeySpec{Enabled: true,
		ModelTokenLimits: []ModelTokenLimit{{Model: "m1", Total: 400, Monthly: 200}}})
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "s1", TraceID: "ts1", KeyID: scoped.ID,
		RequestedModel: "m1", InputEstimate: 200, At: jan}); err != nil {
		t.Fatalf("january reserve: %v", err)
	}
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "s2", TraceID: "ts2", KeyID: scoped.ID,
		RequestedModel: "m1", InputEstimate: 1, At: jan}); !errors.Is(err, ErrQuota) {
		t.Fatalf("same month err = %v, want ErrQuota", err)
	}
	h.clock.set(feb)
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "s3", TraceID: "ts3", KeyID: scoped.ID,
		RequestedModel: "m1", InputEstimate: 200, At: feb}); err != nil {
		t.Fatalf("next month must have its own window: %v", err)
	}

	// Lifetime window: a stale unresolved estimate still counts against total.
	lifetime, _ := h.mustKey(KeySpec{Enabled: true,
		ModelTokenLimits: []ModelTokenLimit{{Model: "m1", Total: 300}}})
	h.clock.set(jan)
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "l1", TraceID: "tl1", KeyID: lifetime.ID,
		RequestedModel: "m1", InputEstimate: 200, At: jan}); err != nil {
		t.Fatalf("lifetime january reserve: %v", err)
	}
	h.clock.set(feb)
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "l2", TraceID: "tl2", KeyID: lifetime.ID,
		RequestedModel: "m1", InputEstimate: 200, At: feb}); !errors.Is(err, ErrQuota) {
		t.Fatalf("lifetime err = %v, want ErrQuota", err)
	}
}

// TestLedgerArithmeticDoesNotOverflowToReal drives the ledger near MaxInt64.
// SQLite would silently store an overflowing INTEGER sum as a REAL, so every
// accumulator must reject the operation and roll the transaction back instead
// of corrupting money or token accounting.
func TestLedgerArithmeticDoesNotOverflowToReal(t *testing.T) {
	const rate = 1_000_000_000_000          // microUSD per 1M tokens
	const tokensNearMax = 9_000_000_000_000 // * rate / 1M = 9e18 microUSD per charge

	// Guard helper: no monetary or token value may be stored as a float.
	assertExactIntegers := func(t *testing.T, h *harness, keyID, requestID string) {
		t.Helper()
		checks := []struct {
			name  string
			query string
			args  []any
		}{
			{"key money", `SELECT COUNT(*) FROM keys WHERE id = ? AND (typeof(spent_micro_usd) <> 'integer' OR typeof(held_micro_usd) <> 'integer')`, []any{keyID}},
			{"money counters", `SELECT COUNT(*) FROM key_money_counters WHERE key_id = ? AND typeof(micro_usd) <> 'integer'`, []any{keyID}},
			{"token counters", `SELECT COUNT(*) FROM key_token_counters WHERE key_id = ? AND typeof(tokens) <> 'integer'`, []any{keyID}},
			{"request charge", `SELECT COUNT(*) FROM requests WHERE id = ? AND typeof(charge_micro_usd) <> 'integer'`, []any{requestID}},
		}
		for _, c := range checks {
			bad, err := counterValue(context.Background(), h.store.db, c.query, c.args...)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if bad != 0 {
				t.Fatalf("%s: %d values were stored as REAL", c.name, bad)
			}
		}
	}

	t.Run("money", func(t *testing.T) {
		h := newHarness(t)
		h.mustPrice("big", Rates{Input: rate})
		key, _ := h.mustKey(KeySpec{Enabled: true})
		req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
			RequestedModel: "big", InputEstimate: tokensNearMax})
		h.mustUsage(usageFor("u1", req, "big", tokens(tokensNearMax, 0, 0, 0, 0)))
		before := h.key(key.ID)
		if before.SpentMicroUSD < math.MaxInt64/2 {
			t.Fatalf("spent = %d, not near the int64 boundary as intended", before.SpentMicroUSD)
		}
		// A second identical charge would push the accumulated total past int64.
		err := h.store.RecordUsage(h.ctx(), usageFor("u2", req, "big", tokens(tokensNearMax, 0, 0, 0, 0)))
		if err == nil {
			t.Fatal("second charge: want an overflow error, got nil")
		}
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("second charge err = %v, want ErrInvalid", err)
		}
		after := h.key(key.ID)
		if after.SpentMicroUSD != before.SpentMicroUSD || after.HeldMicroUSD != before.HeldMicroUSD {
			t.Fatalf("ledger moved after a rejected charge: %+v -> %+v", before, after)
		}
		if n, err := counterValue(h.ctx(), h.store.db,
			`SELECT COUNT(*) FROM usages WHERE request_id = ?`, req.ID); err != nil || n != 1 {
			t.Fatalf("usages = %d (%v), want the rejected execution rolled back", n, err)
		}
		assertExactIntegers(t, h, key.ID, req.ID)

		// The hold accumulator is guarded the same way.
		_, err = h.store.Reserve(h.ctx(), ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
			RequestedModel: "big", InputEstimate: tokensNearMax})
		if err == nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("hold overflow err = %v, want ErrInvalid", err)
		}
		if n, err := counterValue(h.ctx(), h.store.db,
			`SELECT COUNT(*) FROM requests WHERE id = ?`, "r2"); err != nil || n != 0 {
			t.Fatalf("rejected reservation was kept: %d (%v)", n, err)
		}
		// Releasing is still exact and the charged spend survives.
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
		final := h.key(key.ID)
		if final.HeldMicroUSD != 0 || final.SpentMicroUSD != before.SpentMicroUSD {
			t.Fatalf("after release spent %d held %d, want %d/0", final.SpentMicroUSD, final.HeldMicroUSD, before.SpentMicroUSD)
		}
		assertExactIntegers(t, h, key.ID, req.ID)
	})

	t.Run("tokens", func(t *testing.T) {
		h := newHarness(t)
		h.mustPrice("t", Rates{Input: 1}) // cheap money, huge token counts
		key, _ := h.mustKey(KeySpec{Enabled: true})
		const huge = int64(9_000_000_000_000_000_000)
		req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
			RequestedModel: "t", InputEstimate: 1})
		// Raw counters are left empty: this case is about the durable token
		// counters, not about an inconsistent provider payload.
		first := usageFor("u1", req, "t", tokens(huge, 0, 0, 0, 0))
		first.Raw = pluginapi.UsageDetail{}
		h.mustUsage(first)
		// The token counters now sit one charge below the int64 limit.
		second := usageFor("u2", req, "t", tokens(huge, 0, 0, 0, 0))
		second.Raw = pluginapi.UsageDetail{}
		err := h.store.RecordUsage(h.ctx(), second)
		if err == nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("token counter overflow err = %v, want ErrInvalid", err)
		}
		if n, err := counterValue(h.ctx(), h.store.db,
			`SELECT COALESCE(SUM(tokens),0) FROM key_token_counters WHERE key_id = ? AND win = ?`,
			key.ID, windowTotal); err != nil || n != huge {
			t.Fatalf("token counter = %d (%v), want %d unchanged", n, err, huge)
		}
		assertExactIntegers(t, h, key.ID, req.ID)
	})
}

// TestResolveRequiresPendingRequest verifies the state machine: an active
// execution owns a concurrency slot and a hold, so only an administrator
// decision on an already pending request may close it.
func TestResolveRequiresPendingRequest(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true, MaxConcurrent: 1})
	ctx := h.ctx()

	active := h.mustReserve(ReserveInput{ID: "a", TraceID: "ta", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 5})
	if err := h.store.Resolve(ctx, active.ID, Resolution{Action: ActionRelease, Reason: "sneaky"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("resolve active err = %v, want ErrConflict", err)
	}
	if k := h.key(key.ID); k.HeldMicroUSD != 5 {
		t.Fatalf("held = %d, want the hold untouched", k.HeldMicroUSD)
	}
	if has, err := h.store.HasActive(ctx); err != nil || !has {
		t.Fatalf("HasActive = %v/%v, want true", has, err)
	}
	// The concurrency slot is still owned by the active execution.
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "b", TraceID: "tb", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 5}); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("second reserve err = %v, want ErrConcurrency", err)
	}

	// Once the lifecycle closes without trustworthy accounting the request is
	// pending and an administrator may rule on it.
	h.mustComplete(Completion{ID: active.ID, TraceID: active.TraceID, Outcome: OutcomeFailed, StatusCode: 500})
	if err := h.store.Resolve(ctx, active.ID, Resolution{Action: ActionRelease, Reason: "manual"}); err != nil {
		t.Fatalf("resolve pending: %v", err)
	}
	if k := h.key(key.ID); k.HeldMicroUSD != 0 {
		t.Fatalf("held = %d, want released", k.HeldMicroUSD)
	}
	if has, err := h.store.HasActive(ctx); err != nil || has {
		t.Fatalf("HasActive = %v/%v, want false", has, err)
	}
	if _, err := h.store.Reserve(ctx, ReserveInput{ID: "b", TraceID: "tb", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 5}); err != nil {
		t.Fatalf("slot after resolve: %v", err)
	}
}

// TestLateCompletionAfterManualResolve proves a completion that arrives after
// an administrator closed a restarted request only records its metadata: the
// settlement, the released hold and the booked money stay untouched.
func TestLateCompletionAfterManualResolve(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()

	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 7})
	// A restart turns the in flight execution into pending without an outcome,
	// which is exactly what an administrator then has to decide about.
	if err := h.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(Options{DataDir: h.dir, Now: h.clock.now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	h.store = reopened
	if got := h.request(req.ID); got.State != statePending || got.Outcome != "" {
		t.Fatalf("after restart = %+v, want a pending request without outcome", got)
	}
	if err := h.store.Resolve(ctx, req.ID, Resolution{Action: ActionRelease, Reason: "operator"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	settled := h.request(req.ID)
	if settled.State != stateSettled {
		t.Fatalf("state = %q, want settled", settled.State)
	}
	spent := h.key(key.ID).SpentMicroUSD

	// The first late completion records the outcome only.
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded, StatusCode: 200})
	after := h.request(req.ID)
	if after.State != stateSettled || after.Outcome != OutcomeSucceeded {
		t.Fatalf("late completion = %+v, want settled with the outcome recorded", after)
	}
	if k := h.key(key.ID); k.SpentMicroUSD != spent || k.HeldMicroUSD != 0 {
		t.Fatalf("late completion changed money: spent %d held %d, want %d/0", k.SpentMicroUSD, k.HeldMicroUSD, spent)
	}
	var resolved string
	if err := h.store.db.QueryRow(`SELECT resolved FROM requests WHERE id = ?`, req.ID).Scan(&resolved); err != nil {
		t.Fatalf("read resolved: %v", err)
	}
	if resolved != ActionRelease {
		t.Fatalf("resolved = %q, want the administrator decision kept", resolved)
	}
	// Repeating the same completion stays idempotent, so a retried native
	// event never turns into a failure.
	if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeSucceeded, StatusCode: 200}); err != nil {
		t.Fatalf("repeated late completion: %v", err)
	}
	// Changing it is still refused.
	if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeFailed, StatusCode: 500}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting completion err = %v, want ErrConflict", err)
	}
	if k := h.key(key.ID); k.HeldMicroUSD != 0 || k.SpentMicroUSD != spent {
		t.Fatalf("refused completions changed money: %+v", k)
	}
}

// TestLateUnaccountedExecutionReopensSettledRequest covers the asynchronous
// case: a closed request receives an execution whose accounting is unknown, so
// it must become pending again with its original hold restored instead of
// silently absorbing the leak.
func TestLateUnaccountedExecutionReopensSettledRequest(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()

	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 9})
	h.mustUsage(usageFor("u1", req, "m1", tokens(9, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	spent := h.key(key.ID).SpentMicroUSD
	if k := h.key(key.ID); k.HeldMicroUSD != 0 {
		t.Fatalf("held = %d, want released", k.HeldMicroUSD)
	}
	var initial int64
	if err := h.store.db.QueryRow(`SELECT initial_hold_micro_usd FROM requests WHERE id = ?`, req.ID).Scan(&initial); err != nil {
		t.Fatalf("read initial hold: %v", err)
	}
	if initial != 9 {
		t.Fatalf("initial hold = %d, want 9", initial)
	}

	late := usageFor("u2", req, "m1", Tokens{})
	late.Quality = QualityUnknown
	h.mustUsage(late)
	got := h.request(req.ID)
	if got.State != statePending {
		t.Fatalf("state = %q, want pending after a late unaccounted execution", got.State)
	}
	if got.HeldMicroUSD != 9 {
		t.Fatalf("held = %d, want the pre-authorization restored", got.HeldMicroUSD)
	}
	if k := h.key(key.ID); k.HeldMicroUSD != 9 || k.SpentMicroUSD != spent {
		t.Fatalf("key after reopen = %+v, want held 9 and spent %d", k, spent)
	}
	// A second unknown event must not restore the hold twice.
	second := usageFor("u3", req, "m1", Tokens{})
	second.Quality = QualityUnknown
	h.mustUsage(second)
	if k := h.key(key.ID); k.HeldMicroUSD != 9 {
		t.Fatalf("held = %d, want 9 after a second unknown event", k.HeldMicroUSD)
	}
	// Only an administrator can close it again.
	if has, err := h.store.HasActive(ctx); err != nil || has {
		t.Fatalf("HasActive = %v/%v, want false for a pending request", has, err)
	}
	if err := h.store.Resolve(ctx, req.ID, Resolution{Action: ActionCharge, AmountMicroUSD: 30,
		Reason: "unaccounted execution"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if k := h.key(key.ID); k.HeldMicroUSD != 0 || k.SpentMicroUSD != spent+30 {
		t.Fatalf("after resolution = %+v, want held 0 and spent %d", k, spent+30)
	}

	// A late known execution is charged without reopening anything, and a
	// manually resolved request is never reopened behind the administrator.
	lateKnown := h.mustReserve(ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 4})
	h.mustUsage(usageFor("u4", lateKnown, "m1", tokens(4, 0, 0, 0, 0)))
	if k := h.key(key.ID); k.SpentMicroUSD != spent+30+4 {
		t.Fatalf("late known execution was not charged: %+v", k)
	}
	manual := h.mustReserve(ReserveInput{ID: "r3", TraceID: "t3", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 6})
	h.mustComplete(Completion{ID: manual.ID, TraceID: manual.TraceID, Outcome: OutcomeFailed, StatusCode: 502})
	if err := h.store.Resolve(ctx, manual.ID, Resolution{Action: ActionCharge, AmountMicroUSD: 11, Reason: "manual"}); err != nil {
		t.Fatalf("resolve manual: %v", err)
	}
	afterManual := h.key(key.ID).SpentMicroUSD
	unknownForManual := usageFor("u5", manual, "m1", Tokens{})
	unknownForManual.Quality = QualityUnknown
	h.mustUsage(unknownForManual)
	if got := h.request(manual.ID); got.State != stateSettled || got.HeldMicroUSD != 0 {
		t.Fatalf("manual resolution was reopened: %+v", got)
	}
	// The active r2 still owns its own hold; only the manual request must not
	// have come back to life.
	if k := h.key(key.ID); k.HeldMicroUSD != 4 || k.SpentMicroUSD != afterManual {
		t.Fatalf("manual resolution changed the ledger: %+v, want held 4 and spent %d", k, afterManual)
	}
}

// TestFailedExecutionDoesNotProveSuccess keeps the success outcome honest: a
// reliable but failed lifecycle event never closes a successful request, while
// a failed request with accounted cost still settles.
func TestFailedExecutionDoesNotProveSuccess(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})

	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 10})
	failed := usageFor("u1", req, "m1", tokens(10, 0, 0, 0, 0))
	failed.Failed = true
	h.mustUsage(failed)
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state = %q, want pending: a failed execution cannot prove success", got.State)
	}
	if k := h.key(key.ID); k.HeldMicroUSD != 10 {
		t.Fatalf("held = %d, want the hold retained", k.HeldMicroUSD)
	}
	// A reliable successful execution then closes it.
	h.mustUsage(usageFor("u2", req, "m1", tokens(10, 0, 0, 0, 0)))
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}

	// A failed lifecycle with accounted executions is already terminal.
	other := h.mustReserve(ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 10})
	broken := usageFor("u3", other, "m1", tokens(10, 0, 0, 0, 0))
	broken.Failed = true
	h.mustUsage(broken)
	h.mustComplete(Completion{ID: other.ID, TraceID: other.TraceID, Outcome: OutcomeFailed, StatusCode: 500})
	if got := h.request(other.ID); got.State != stateSettled {
		t.Fatalf("failed state = %q, want settled", got.State)
	}
}

// TestUsageReplayWithoutTimestamp covers a caller that omits the event time: the
// replay must be recognised even though the wall clock moved on.
func TestUsageReplayWithoutTimestamp(t *testing.T) {
	h, key := billingHarness(t)
	ctx := h.ctx()
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 10})

	first := usageFor("u1", req, "m1", tokens(10, 0, 0, 0, 0))
	if !first.At.IsZero() {
		t.Fatal("the case needs an omitted timestamp")
	}
	if err := h.store.RecordUsage(ctx, first); err != nil {
		t.Fatalf("record: %v", err)
	}
	h.clock.advance(3 * time.Hour)
	replay := usageFor("u1", req, "m1", tokens(10, 0, 0, 0, 0))
	if err := h.store.RecordUsage(ctx, replay); err != nil {
		t.Fatalf("replay without a timestamp: %v", err)
	}
	// An explicit, different timestamp is still a conflicting reuse.
	conflicting := usageFor("u1", req, "m1", tokens(10, 0, 0, 0, 0))
	conflicting.At = h.clock.now()
	if err := h.store.RecordUsage(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("explicit timestamp err = %v, want ErrConflict", err)
	}
	if n, err := counterValue(ctx, h.store.db,
		`SELECT COUNT(*) FROM usages WHERE request_id = ?`, req.ID); err != nil || n != 1 {
		t.Fatalf("usages = %d (%v), want a single execution", n, err)
	}
	if k := h.key(key.ID); k.SpentMicroUSD != 10 {
		t.Fatalf("spent = %d, want a single charge", k.SpentMicroUSD)
	}
}

// TestCompletionAfterManualResolveKeepsRecordedOutcome covers a request that
// completed normally and was then closed by an administrator: the recorded
// outcome stays authoritative and is never rewritten by a later completion.
func TestCompletionAfterManualResolveKeepsRecordedOutcome(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 5})

	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeFailed, StatusCode: 502})
	// An unresolved request repeats its outcome idempotently.
	if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeFailed, StatusCode: 502}); err != nil {
		t.Fatalf("repeat before resolve: %v", err)
	}
	if got := h.request(req.ID); got.State != statePending || got.Outcome != OutcomeFailed {
		t.Fatalf("before resolve = %+v, want a pending failure", got)
	}
	if err := h.store.Resolve(ctx, req.ID, Resolution{Action: ActionRelease, Reason: "operator"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The same completion is still a no-op once the request is closed.
	if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeFailed, StatusCode: 502}); err != nil {
		t.Fatalf("repeat after resolve: %v", err)
	}
	// A different outcome must not be mistaken for a first late completion.
	if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeSucceeded, StatusCode: 200}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed outcome err = %v, want ErrConflict", err)
	}
	if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeFailed, StatusCode: 500}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed status err = %v, want ErrConflict", err)
	}
	got := h.request(req.ID)
	if got.State != stateSettled || got.Outcome != OutcomeFailed {
		t.Fatalf("request = %+v, want the recorded failure kept", got)
	}
	if k := h.key(key.ID); k.HeldMicroUSD != 0 || k.SpentMicroUSD != 0 {
		t.Fatalf("money changed: %+v", k)
	}
}
