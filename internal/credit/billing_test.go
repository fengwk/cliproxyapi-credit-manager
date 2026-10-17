package credit

import (
	"context"
	"database/sql"
	"errors"
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

// costOf is the query-time cost summary of one key's whole test window.
func (h *harness) costOf(keyID string) (int64, int64) {
	h.t.Helper()
	sum := h.report(keyID).Summary
	return sum.CostMicroUSD, sum.UnpricedExecutions
}

func TestAdmitValidatesKeyAndInput(t *testing.T) {
	h, key := billingHarness(t)
	ctx := h.ctx()
	// A model with no price at all is admitted: admission never reads the price
	// book, so an unpriced model is an unpriced execution, not a denied one.
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "unknown"}); err != nil {
		t.Fatalf("unpriced model admission: %v", err)
	}
	// Model permission is exact.
	spec := key.Spec
	spec.AllowedModels = []string{"m1"}
	if _, err := h.store.UpdateKey(ctx, key.ID, spec); err != nil {
		t.Fatalf("update key: %v", err)
	}
	h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1"})
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "r3", TraceID: "t3", KeyID: key.ID, RequestedModel: "m1"}); err != nil {
		t.Fatalf("allowed model: %v", err)
	}
	spec.AllowedModels = []string{"m2"}
	if _, err := h.store.UpdateKey(ctx, key.ID, spec); err != nil {
		t.Fatalf("update key: %v", err)
	}
	_, err := h.store.Admit(ctx, AdmitInput{ID: "r4", TraceID: "t4", KeyID: key.ID, RequestedModel: "m1"})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("disallowed model err = %v, want ErrDenied", err)
	}
	// Disabled and expired keys are unauthorized.
	spec.AllowedModels = nil
	spec.Enabled = false
	if _, err := h.store.UpdateKey(ctx, key.ID, spec); err != nil {
		t.Fatalf("update key: %v", err)
	}
	_, err = h.store.Admit(ctx, AdmitInput{ID: "r5", TraceID: "t5", KeyID: key.ID, RequestedModel: "m1"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled err = %v, want ErrUnauthorized", err)
	}
	spec.Enabled = true
	past := testEpoch.Add(-time.Hour)
	spec.ExpiresAt = &past
	if _, err := h.store.UpdateKey(ctx, key.ID, spec); err != nil {
		t.Fatalf("update key: %v", err)
	}
	_, err = h.store.Admit(ctx, AdmitInput{ID: "r6", TraceID: "t6", KeyID: key.ID, RequestedModel: "m1"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired err = %v, want ErrUnauthorized", err)
	}
	// Invalid input is rejected before touching state.
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "", TraceID: "t", KeyID: key.ID, RequestedModel: "m1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty id err = %v, want ErrInvalid", err)
	}
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "x", TraceID: "t", KeyID: key.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing model err = %v, want ErrInvalid", err)
	}
}

// TestDollarBudgetsNeverGateAdmission pins the product decision: the four
// canonical money fields are display-only budgets. A key whose budgets are all
// far exceeded by actual usage still admits, and no budget value is ever read as
// a limit.
func TestDollarBudgetsNeverGateAdmission(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million}) // 1 token = 1 microUSD
	key, _ := h.mustKey(KeySpec{
		Enabled: true, QuotaMicroUSD: 1, DailyMicroUSD: 1, WeeklyMicroUSD: 1, MonthlyMicroUSD: 1})
	ctx := h.ctx()

	first := h.mustAdmit(AdmitInput{ID: "b1", TraceID: "tb1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u-b1", first, "m1", tokens(10_000, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: first.ID, TraceID: first.TraceID, Outcome: OutcomeSucceeded})
	if cost, unpriced := h.costOf(key.ID); cost != 10_000 || unpriced != 0 {
		t.Fatalf("cost = %d/%d, want 10000 priced executions", cost, unpriced)
	}
	// Every budget is exceeded many times over; admission still succeeds.
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "b2", TraceID: "tb2", KeyID: key.ID,
		RequestedModel: "m1"}); err != nil {
		t.Fatalf("admission after exceeding every dollar budget: %v", err)
	}
	// The budget figures survive as reported spec values, never as usage.
	got := h.key(key.ID)
	if got.Spec.QuotaMicroUSD != 1 || got.Spec.MonthlyMicroUSD != 1 {
		t.Fatalf("budget figures = %+v, want the configured display values", got.Spec)
	}
}

func TestAdmitIdempotentAndConflicting(t *testing.T) {
	h, key := billingHarness(t)
	in := AdmitInput{ID: "req-1", TraceID: "trace-1", KeyID: key.ID, RequestedModel: "m1"}
	first := h.mustAdmit(in)
	second := h.mustAdmit(in)
	if first.ID != second.ID || first.TraceID != second.TraceID {
		t.Fatalf("idempotent admission mismatch: %+v vs %+v", first, second)
	}
	// A repeated admission is the same request and stores no price or amount.
	if len(first.Executions) != 0 {
		t.Fatalf("fresh admission carries executions: %+v", first)
	}
	if cost, unpriced := h.costOf(key.ID); cost != 0 || unpriced != 0 {
		t.Fatalf("repeated admission produced cost %d/%d, want none", cost, unpriced)
	}
	conflict := in
	conflict.TraceID = "other-trace"
	if _, err := h.store.Admit(h.ctx(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting id err = %v, want ErrConflict", err)
	}
	conflict = in
	conflict.RequestedModel = "m2"
	if _, err := h.store.Admit(h.ctx(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting model err = %v, want ErrConflict", err)
	}
	// A trace may not be reused by another lifecycle id.
	other := in
	other.ID = "req-2"
	if _, err := h.store.Admit(h.ctx(), other); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused trace err = %v, want ErrConflict", err)
	}
}

// TestAdmitRecordsServiceTierFact pins that the request stores the service tier
// as a request fact, normalized, and that it is what a usage event without its
// own tier inherits.
func TestAdmitRecordsServiceTierFact(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1", ServiceTier: "PRIORITY"})
	if req.ServiceTier != "priority" {
		t.Fatalf("request service tier = %q, want priority", req.ServiceTier)
	}
	h.mustUsage(usageFor("u1", req, "m1", tokens(1, 0, 0, 0, 0)))
	exec := h.request(req.ID).Executions[0]
	if exec.ServiceTier != "priority" {
		t.Fatalf("inherited execution tier = %q, want priority", exec.ServiceTier)
	}
}

func TestAdmitConcurrencyAndOverage(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true, ModelTokenLimits: []ModelTokenLimit{
		{Model: "m1", Total: 10}}, MaxConcurrent: 1})
	ctx := h.ctx()

	active := h.mustAdmit(AdmitInput{ID: "a", TraceID: "ta", KeyID: key.ID, RequestedModel: "m1"})
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "b", TraceID: "tb", KeyID: key.ID, RequestedModel: "m1"}); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("concurrency err = %v, want ErrConcurrency", err)
	}
	// Actual usage exceeding the token limit is recorded in full, never capped:
	// admission only checks what was already consumed when it ran.
	h.mustUsage(usageFor("u-a", active, "m1", tokens(25, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: active.ID, TraceID: active.TraceID, Outcome: OutcomeSucceeded})
	if sum := h.report(key.ID).Summary; sum.Tokens.Total != 25 {
		t.Fatalf("tokens = %d, want the full 25", sum.Tokens.Total)
	}
	// Completion released the concurrency slot; the exhausted token limit denies.
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "c", TraceID: "tc", KeyID: key.ID, RequestedModel: "m1"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("post-overage err = %v, want ErrQuota", err)
	}
}

// TestUnresolvedAccountingDoesNotBlockAdmission pins that unknown or unresolved
// accounting never becomes a backlog gate: later requests on the same key are
// still admitted while an earlier one is unresolved.
func TestUnresolvedAccountingDoesNotBlockAdmission(t *testing.T) {
	h, key := billingHarness(t)
	pending := h.mustAdmit(AdmitInput{ID: "p1", TraceID: "tp1", KeyID: key.ID, RequestedModel: "m1"})
	unknown := usageFor("u-unknown", pending, "m1", Tokens{})
	unknown.Quality = QualityUnknown
	h.mustUsage(unknown)
	h.mustComplete(Completion{ID: pending.ID, TraceID: pending.TraceID, Outcome: OutcomeFailed, StatusCode: 500})
	if got := h.request(pending.ID); got.State != statePending {
		t.Fatalf("state = %q, want pending", got.State)
	}
	// Many unresolved requests must not stop the next admission.
	for i := 0; i < 32; i++ {
		h.mustAdmit(AdmitInput{ID: "later-" + itoa(i), TraceID: "tl-" + itoa(i), KeyID: key.ID, RequestedModel: "m1"})
	}
}

func TestAdmitReleasesConcurrencyOnlyOnce(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	// Idempotent completion keeps the pending state.
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

// TestUsageCostIsDerivedOncePerExecution exercises the query-time derivation: a
// usage recorded while the execution is still active is already priced, a replay
// does not double it, and a late execution adds its own amount.
func TestUsageCostIsDerivedOncePerExecution(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 100, 0)))
	if cost, unpriced := h.costOf(key.ID); cost != 300 || unpriced != 0 {
		t.Fatalf("active cost = %d/%d, want 300 priced", cost, unpriced)
	}
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if cost, _ := h.costOf(key.ID); cost != 300 {
		t.Fatalf("settled cost = %d, want 300", cost)
	}
	// A duplicate event must not be counted twice.
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 100, 0)))
	if cost, _ := h.costOf(key.ID); cost != 300 {
		t.Fatalf("after replay cost = %d, want 300", cost)
	}
	// A late execution is priced in full after completion.
	extra := usageFor("u2", req, "m1", tokens(0, 0, 0, 1000, 0))
	h.mustUsage(extra)
	if cost, _ := h.costOf(key.ID); cost != 300+2000 {
		t.Fatalf("late cost = %d, want 2300", cost)
	}
	// The page shows the same per-execution amounts as the summary.
	items := h.report(key.ID).Items
	if len(items) != 1 || len(items[0].Executions) != 2 {
		t.Fatalf("items = %+v", items)
	}
	for _, e := range items[0].Executions {
		switch e.ID {
		case "u1":
			if e.CostMicroUSD == nil || *e.CostMicroUSD != 300 {
				t.Fatalf("u1 cost = %v, want 300", e.CostMicroUSD)
			}
		case "u2":
			if e.CostMicroUSD == nil || *e.CostMicroUSD != 2000 {
				t.Fatalf("u2 cost = %v, want 2000", e.CostMicroUSD)
			}
		}
	}
}

// TestUnknownExecutionBlocksClosure proves that an unreliable execution keeps the
// request open, contributes no cost, and is counted as unpriced, while a
// trustworthy sibling is priced immediately.
func TestUnknownExecutionBlocksClosure(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state without usage = %q, want pending", got.State)
	}
	unknown := usageFor("u-unknown", req, "m1", Tokens{})
	unknown.Quality = QualityUnknown
	h.mustUsage(unknown)
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state after unknown usage = %q, want pending", got.State)
	}
	if cost, unpriced := h.costOf(key.ID); cost != 0 || unpriced != 1 {
		t.Fatalf("unknown-only cost = %d/%d, want 0 cost and 1 unpriced", cost, unpriced)
	}
	// A trustworthy sibling is priced but the unknown execution still blocks
	// closure.
	h.mustUsage(usageFor("u-known", req, "m1", tokens(100, 0, 0, 100, 0)))
	got := h.request(req.ID)
	if got.State != statePending {
		t.Fatalf("state with unknown execution = %q, want pending", got.State)
	}
	if cost, unpriced := h.costOf(key.ID); cost != 300 || unpriced != 1 {
		t.Fatalf("cost = %d/%d, want 300 with 1 unpriced", cost, unpriced)
	}
	// A repeated completion must not change anything while the request is
	// unresolved, but it is still checked for agreement.
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state after replay = %q, want pending", got.State)
	}
	for _, e := range h.request(req.ID).Executions {
		switch e.ID {
		case "u-unknown":
			if e.CostMicroUSD != nil {
				t.Fatalf("unknown execution must keep a null cost, got %d", *e.CostMicroUSD)
			}
		case "u-known":
			if e.CostMicroUSD == nil || *e.CostMicroUSD != 300 {
				t.Fatalf("known execution cost = %v", e.CostMicroUSD)
			}
		}
	}
}

// TestUnknownExecutionOrderingAndIdempotency covers the event orders the native
// bridge can produce for one execution set.
func TestUnknownExecutionOrderingAndIdempotency(t *testing.T) {
	h, key := billingHarness(t)
	reserve := func(id string) Request {
		return h.mustAdmit(AdmitInput{ID: id, TraceID: "t-" + id, KeyID: key.ID, RequestedModel: "m1"})
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
	if cost, unpriced := h.costOf(key.ID); cost != 300 || unpriced != 1 {
		t.Fatalf("order A cost = %d/%d, want 300 with 1 unpriced", cost, unpriced)
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
	if cost, unpriced := h.costOf(key.ID); cost != 600 || unpriced != 2 {
		t.Fatalf("order B cost = %d/%d, want 600 with 2 unpriced", cost, unpriced)
	}
	// Both events are replayable without changing any derived amount.
	h.mustUsage(unknownA)
	h.mustUsage(usageFor("ua2", a, "m1", tokens(100, 0, 0, 100, 0)))
	h.mustUsage(unknownB)
	h.mustUsage(usageFor("ub2", b, "m1", tokens(100, 0, 0, 100, 0)))
	if cost, unpriced := h.costOf(key.ID); cost != 600 || unpriced != 2 {
		t.Fatalf("replay cost = %d/%d, want 600 with 2 unpriced", cost, unpriced)
	}
}

// TestCompletionOutcomes pins down which outcomes may close a request without
// an execution event. A 4xx status is never proof on its own.
func TestCompletionOutcomes(t *testing.T) {
	h, key := billingHarness(t)
	ctx := h.ctx()
	reserve := func(id string) Request {
		return h.mustAdmit(AdmitInput{ID: id, TraceID: "t-" + id, KeyID: key.ID, RequestedModel: "m1"})
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
	// A rejection still prices what was reported.
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
	// stays open for reconciliation instead of silently discarding it.
	rejectedUnknown := reserve("r-unknown")
	ru := usageFor("uru", rejectedUnknown, "m1", Tokens{})
	ru.Quality = QualityUnknown
	h.mustUsage(ru)
	h.mustComplete(Completion{ID: rejectedUnknown.ID, TraceID: rejectedUnknown.TraceID,
		Outcome: OutcomeRejected, StatusCode: 403})
	if got := h.request(rejectedUnknown.ID).State; got != statePending {
		t.Fatalf("rejected with unknown execution state = %q, want pending", got)
	}
	// Only the three trustworthy executions are priced: 100 + 50 + 10.
	if cost, unpriced := h.costOf(key.ID); cost != 160 || unpriced != 2 {
		t.Fatalf("cost = %d/%d, want 160 with 2 unpriced", cost, unpriced)
	}
}

// TestCompleteValidatesRepeatedOutcomeAndStatus rejects a repeated completion
// that disagrees with what was already recorded.
func TestCompleteValidatesRepeatedOutcomeAndStatus(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
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

// TestUsageRejectsCallerSuppliedCost keeps the derived amount from being
// injected: the cost is a read-time projection, never a stored field.
func TestUsageRejectsCallerSuppliedCost(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	u := usageFor("u1", req, "m1", tokens(10, 0, 0, 10, 0))
	cost := int64(1)
	u.CostMicroUSD = &cost
	if err := h.store.RecordUsage(h.ctx(), u); !errors.Is(err, ErrInvalid) {
		t.Fatalf("caller cost err = %v, want ErrInvalid", err)
	}
	if n, err := counterValue(h.ctx(), h.store.db, `SELECT COUNT(*) FROM usages`); err != nil || n != 0 {
		t.Fatalf("usages = %d (%v), want none stored", n, err)
	}
	if got, _ := h.costOf(key.ID); got != 0 {
		t.Fatalf("cost after rejected usage = %d, want 0", got)
	}
}

func TestUsageMatchingRules(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
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

// TestLateUsageCountsTowardAdmissionPeriod verifies the token counters follow
// the admission period, not the time the late event arrived.
func TestLateUsageCountsTowardAdmissionPeriod(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		At: testEpoch})
	// The event arrives in the next month.
	h.clock.set(testEpoch.AddDate(0, 1, 0))
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})

	june, err := tokenCounter(h.ctx(), h.store.db, key.ID, "m1", windowMonth, "2026-06")
	if err != nil {
		t.Fatalf("june counter: %v", err)
	}
	if june != 100 {
		t.Fatalf("june usage = %d, want 100", june)
	}
	july, err := tokenCounter(h.ctx(), h.store.db, key.ID, "m1", windowMonth, "2026-07")
	if err != nil {
		t.Fatalf("july counter: %v", err)
	}
	if july != 0 {
		t.Fatalf("july usage = %d, want 0", july)
	}
	total, err := tokenCounter(h.ctx(), h.store.db, key.ID, "m1", windowTotal, "")
	if err != nil {
		t.Fatalf("total counter: %v", err)
	}
	if total != 100 {
		t.Fatalf("total usage = %d, want 100", total)
	}
	dayCounter, err := tokenCounter(h.ctx(), h.store.db, key.ID, "m1", windowDay, "2026-06-15")
	if err != nil {
		t.Fatalf("day counter: %v", err)
	}
	if dayCounter != 100 {
		t.Fatalf("day usage = %d, want 100", dayCounter)
	}
	weekCounter, err := tokenCounter(h.ctx(), h.store.db, key.ID, "m1", windowWeek, weekPeriod(testEpoch))
	if err != nil {
		t.Fatalf("week counter: %v", err)
	}
	if weekCounter != 100 {
		t.Fatalf("week usage = %d, want 100", weekCounter)
	}
}

// TestTokenDailyLimitResetsAcrossPeriods is the token-window counterpart of the
// former money-period gate: the limit consumes actual tokens of its own UTC day.
func TestTokenDailyLimitResetsAcrossPeriods(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true,
		ModelTokenLimits: []ModelTokenLimit{{Model: "m1", Daily: 100}}})
	ctx := h.ctx()
	spend := func(id, trace string, amount int64) error {
		req, err := h.store.Admit(ctx, AdmitInput{ID: id, TraceID: trace, KeyID: key.ID,
			RequestedModel: "m1"})
		if err != nil {
			return err
		}
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
		ModelTokenLimits: []ModelTokenLimit{{Model: "m1", Daily: 150}}})
	ctx := h.ctx()
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	// A second m1 admission still fits: nothing has been consumed yet.
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1"}); err != nil {
		t.Fatalf("m1 within the token limit: %v", err)
	}
	// Another model is unaffected.
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "r3", TraceID: "t3", KeyID: key.ID, RequestedModel: "m2"}); err != nil {
		t.Fatalf("other model: %v", err)
	}
	// Reported usage moves into durable counters.
	h.mustUsage(usageFor("u1", req, "m1", tokens(150, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	used, err := tokenCounter(ctx, h.store.db, key.ID, "m1", windowDay, dayPeriod(testEpoch))
	if err != nil {
		t.Fatalf("token counter: %v", err)
	}
	if used != 150 {
		t.Fatalf("token counter = %d, want 150", used)
	}
	// The actual usage has reached the limit, so the next m1 admission denies.
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "r4", TraceID: "t4", KeyID: key.ID, RequestedModel: "m1"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("used == limit err = %v, want ErrQuota", err)
	}
	// The next day is a new window.
	h.clock.set(testEpoch.AddDate(0, 0, 1))
	if _, err := h.store.Admit(ctx, AdmitInput{ID: "r5", TraceID: "t5", KeyID: key.ID, RequestedModel: "m1"}); err != nil {
		t.Fatalf("next day reserve: %v", err)
	}
}

func TestConcurrentAdmitHonoursConcurrencyLimit(t *testing.T) {
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
			_, err := h.store.Admit(context.Background(), AdmitInput{
				ID:             "req-" + itoa(i),
				TraceID:        "trace-" + itoa(i),
				KeyID:          key.ID,
				RequestedModel: "m1"})
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

// TestPriceEditRepricesHistory is the core product promise: nothing about a
// price is stored with a request, so adding, editing and deleting a price
// immediately changes the reported cost of the same recorded history.
func TestPriceEditRepricesHistory(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	// 1) Record history before any price exists.
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 100, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled without any price", got.State)
	}
	if cost, unpriced := h.costOf(key.ID); cost != 0 || unpriced != 1 {
		t.Fatalf("unpriced history = %d/%d, want 0 cost and 1 unpriced", cost, unpriced)
	}
	if exec := h.request(req.ID).Executions[0]; exec.CostMicroUSD != nil {
		t.Fatalf("unpriced execution cost = %d, want null", *exec.CostMicroUSD)
	}
	if sum := h.report(key.ID).Summary; sum.CompleteExecutions != 1 {
		t.Fatalf("complete executions = %d, want the complete usage counted independently of price", sum.CompleteExecutions)
	}

	// 2) Adding a price immediately prices the existing history.
	h.mustPrice("m1", Rates{Input: million, Output: 2 * million})
	if cost, unpriced := h.costOf(key.ID); cost != 300 || unpriced != 0 {
		t.Fatalf("after adding a price = %d/%d, want 300 priced", cost, unpriced)
	}

	// 3) Editing the price recomputes the same history.
	h.mustPrice("m1", Rates{Input: 10 * million, Output: 20 * million})
	if cost, _ := h.costOf(key.ID); cost != 3000 {
		t.Fatalf("after editing the price = %d, want 3000", cost)
	}

	// 4) Deleting the price makes it unpriced again: no snapshot survives.
	if _, err := h.store.db.ExecContext(h.ctx(), `DELETE FROM prices WHERE model = ?`, "m1"); err != nil {
		t.Fatalf("delete price: %v", err)
	}
	if cost, unpriced := h.costOf(key.ID); cost != 0 || unpriced != 1 {
		t.Fatalf("after deleting the price = %d/%d, want unpriced again", cost, unpriced)
	}
}

// TestExecutionIsPricedByItsActualModel pins the pricing dimension: the exact
// usage.Model prices an execution, never the request alias and never the
// response model.
func TestExecutionIsPricedByItsActualModel(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("alias", Rates{Input: million})
	h.mustPrice("actual", Rates{Input: 50 * million})
	h.mustPrice("response", Rates{Input: 99 * million})
	key, _ := h.mustKey(KeySpec{Enabled: true})

	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "alias"})
	exec := usageFor("u1", req, "actual", tokens(10, 0, 0, 0, 0))
	exec.ResponseModel = "response"
	h.mustUsage(exec)
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	// 10 tokens at the actual model's 50 microUSD/1M = 500.
	if cost, unpriced := h.costOf(key.ID); cost != 500 || unpriced != 0 {
		t.Fatalf("cost = %d/%d, want 500 priced by the actual model", cost, unpriced)
	}
	// The request keeps its alias for permissions and grouping, and the
	// unpriced alias itself is not what was priced.
	items := h.report(key.ID).Items
	if items[0].Model != "alias" {
		t.Fatalf("request model = %q, want the alias", items[0].Model)
	}
	if items[0].Executions[0].Model != "actual" || items[0].Executions[0].ResponseModel != "response" {
		t.Fatalf("execution = %+v, want the actual model and response model preserved", items[0].Executions[0])
	}
}

// TestFailedAndRetriedExecutionsAreAllPriced pins that every reliable execution
// is priced, including failed attempts of a request that eventually succeeded.
func TestFailedAndRetriedExecutionsAreAllPriced(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	failed := usageFor("u1", req, "m1", tokens(100, 0, 0, 0, 0))
	failed.Failed = true
	h.mustUsage(failed)
	h.mustUsage(usageFor("u2", req, "m1", tokens(0, 0, 0, 100, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	// 100 input tokens = 100 microUSD, 100 output tokens = 200 microUSD.
	if cost, unpriced := h.costOf(key.ID); cost != 300 || unpriced != 0 {
		t.Fatalf("cost = %d/%d, want 300 for both attempts", cost, unpriced)
	}
}

// TestProviderFilteredReportPricesTheSameExecutions keeps an execution filter
// from changing what a selected amount means.
func TestProviderFilteredReportPricesTheSameExecutions(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 0, 0)))
	other := usageFor("u2", req, "m1", tokens(0, 0, 0, 100, 0))
	other.Provider = "other"
	h.mustUsage(other)
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})

	rep, err := h.store.Report(h.ctx(), Filter{KeyID: key.ID, Provider: "openai",
		From: testEpoch.AddDate(-1, 0, 0), To: h.clock.now().Add(time.Hour), PageSize: 10})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Summary.CostMicroUSD != 100 || rep.Summary.Executions != 1 {
		t.Fatalf("filtered summary = %+v, want the openai execution only", rep.Summary)
	}
	if len(rep.Items) != 1 || len(rep.Items[0].Executions) != 1 {
		t.Fatalf("filtered items = %+v", rep.Items)
	}
	if rep.Items[0].Executions[0].CostMicroUSD == nil || *rep.Items[0].Executions[0].CostMicroUSD != 100 {
		t.Fatalf("filtered execution cost = %v, want 100", rep.Items[0].Executions[0].CostMicroUSD)
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
	_, err = h.store.Admit(ctx, AdmitInput{ID: "busy", TraceID: "t-busy", KeyID: key.ID, RequestedModel: "m1"})
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
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	u := usageFor("u1", req, "m1", tokens(10, 0, 0, 10, 0))
	u.RequestID = ""
	h.mustUsage(u)
	h.mustUsage(u) // must be a no-op, not a conflict
	got := h.request(req.ID)
	if len(got.Executions) != 1 {
		t.Fatalf("executions = %d, want 1", len(got.Executions))
	}
	if cost, _ := h.costOf(key.ID); cost != 30 {
		t.Fatalf("cost after replay = %d, want 30", cost)
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
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
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
	h.mustAdmit(AdmitInput{ID: "live", TraceID: "t-live", KeyID: key.ID, RequestedModel: "m1"})

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
// an all-zero rate is a real price, so a complete execution is priced zero while
// an unaccounted one stays unpriced.
func TestZeroRatePriceStillRequiresUsage(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("free", Rates{})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "free"})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("free success without usage state = %q, want pending", got.State)
	}
	// An explicit complete declaration closes it at zero cost.
	u := usageFor("u1", req, "free", Tokens{})
	u.Quality = QualityComplete
	h.mustUsage(u)
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	sum := h.report(key.ID).Summary
	if sum.CostMicroUSD != 0 || sum.UnpricedExecutions != 0 {
		t.Fatalf("free summary = %+v, want a priced zero and no unpriced execution", sum)
	}
	if exec := h.request(req.ID).Executions[0]; exec.CostMicroUSD == nil || *exec.CostMicroUSD != 0 {
		t.Fatalf("free execution cost = %v, want an explicit zero", exec.CostMicroUSD)
	}
}

// TestTokenLimitsUseTheirOwnWindow proves token limits gate on actual token
// usage per window: a reached window denies, the next window admits, and the
// lifetime limit keeps counting usage from every period.
func TestTokenLimitsUseTheirOwnWindow(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	ctx := h.ctx()
	jan := time.Date(2026, time.January, 10, 12, 0, 0, 0, time.UTC)
	feb := time.Date(2026, time.February, 3, 9, 0, 0, 0, time.UTC)
	spend := func(key Key, id, trace string, at time.Time, amount int64) error {
		req, err := h.store.Admit(ctx, AdmitInput{ID: id, TraceID: trace, KeyID: key.ID,
			RequestedModel: "m1", At: at})
		if err != nil {
			return err
		}
		h.mustUsage(usageFor("u-"+id, req, "m1", tokens(amount, 0, 0, 0, 0)))
		h.mustComplete(Completion{ID: req.ID, TraceID: trace, Outcome: OutcomeSucceeded})
		return nil
	}

	// Period windows: consuming January's monthly budget must not starve
	// February, but must deny the rest of January.
	h.clock.set(jan)
	scoped, _ := h.mustKey(KeySpec{Enabled: true,
		ModelTokenLimits: []ModelTokenLimit{{Model: "m1", Total: 400, Monthly: 200}}})
	if err := spend(scoped, "s1", "ts1", jan, 200); err != nil {
		t.Fatalf("january spend: %v", err)
	}
	if err := spend(scoped, "s2", "ts2", jan, 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("same month err = %v, want ErrQuota", err)
	}
	h.clock.set(feb)
	if err := spend(scoped, "s3", "ts3", feb, 200); err != nil {
		t.Fatalf("next month must have its own window: %v", err)
	}

	// Lifetime window: usage from every period counts against total.
	lifetime, _ := h.mustKey(KeySpec{Enabled: true,
		ModelTokenLimits: []ModelTokenLimit{{Model: "m1", Total: 300}}})
	h.clock.set(jan)
	if err := spend(lifetime, "l1", "tl1", jan, 300); err != nil {
		t.Fatalf("lifetime january spend: %v", err)
	}
	h.clock.set(feb)
	if err := spend(lifetime, "l2", "tl2", feb, 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("lifetime err = %v, want ErrQuota", err)
	}
}

// TestTokenCountersNeverOverflowToReal drives the token counters near MaxInt64.
// SQLite would silently store an overflowing INTEGER sum as a REAL, so the
// accumulator must reject the operation and roll the transaction back instead of
// corrupting the admission basis.
func TestTokenCountersNeverOverflowToReal(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("t", Rates{Input: 1})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	const huge = int64(9_000_000_000_000_000_000)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "t"})
	// Raw counters are left empty: this case is about the durable token
	// counters, not about an inconsistent provider payload.
	first := usageFor("u1", req, "t", tokens(huge, 0, 0, 0, 0))
	first.Raw = pluginapi.UsageDetail{}
	h.mustUsage(first)
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
	bad, err := counterValue(h.ctx(), h.store.db,
		`SELECT COUNT(*) FROM key_token_counters WHERE key_id = ? AND typeof(tokens) <> 'integer'`, key.ID)
	if err != nil || bad != 0 {
		t.Fatalf("token counters stored as REAL: %d (%v)", bad, err)
	}
}

// TestRecordUsageSurvivesAnOverflowingPrice proves that pricing can never block
// recording: a rate whose product with the actual tokens exceeds int64 is stored
// fine, and only the derived report treats the execution as unpriced.
func TestRecordUsageSurvivesAnOverflowingPrice(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("big", Rates{Input: 1 << 62})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "big"})
	h.mustUsage(usageFor("u1", req, "big", tokens(1<<40, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	n, err := counterValue(h.ctx(), h.store.db, `SELECT COUNT(*) FROM usages WHERE request_id = ?`, req.ID)
	if err != nil || n != 1 {
		t.Fatalf("usages = %d (%v), want the execution stored", n, err)
	}
	// The unrepresentable amount is reported as unpriced rather than corrupting
	// or failing the whole report.
	cost, unpriced := h.costOf(key.ID)
	if cost != 0 || unpriced != 1 {
		t.Fatalf("cost = %d/%d, want the overflowing execution unpriced", cost, unpriced)
	}
}

// TestDerivedAmountsAreExactIntegers keeps money exact beyond 2^53: a large but
// representable amount must round-trip as an exact integer microUSD value.
func TestDerivedAmountsAreExactIntegers(t *testing.T) {
	h := newHarness(t)
	// 1 microUSD per 1M tokens with 1e16 tokens: 1e16 microUSD exactly, both
	// well beyond 2^53 and still far below MaxInt64.
	h.mustPrice("m1", Rates{Input: 1_000_000})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	const bigTokens = int64(10_000_000_000_000_000)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(bigTokens, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	const want = int64(10_000_000_000_000_000)
	if cost, unpriced := h.costOf(key.ID); cost != want || unpriced != 0 {
		t.Fatalf("cost = %d/%d, want %d exactly", cost, unpriced, want)
	}
	exec := h.request(req.ID).Executions[0]
	if exec.CostMicroUSD == nil || *exec.CostMicroUSD != want {
		t.Fatalf("execution cost = %v, want %d", exec.CostMicroUSD, want)
	}
	// The stored tokens round-trip without loss.
	if exec.Tokens.Input != bigTokens || exec.Tokens.Total != bigTokens {
		t.Fatalf("stored tokens = %+v, want %d preserved", exec.Tokens, bigTokens)
	}
	if bigTokens <= 1<<53 || want <= 1<<53 {
		t.Fatal("the case must exceed 2^53 to be meaningful")
	}
}

// TestLateCompletionAfterRestartKeepsRecordedOutcome proves the restart
// transition and a later completion agree: the request stays pending until its
// usage is trustworthy, a completion that agrees is idempotent, and a
// disagreeing one is refused.
func TestLateCompletionAfterRestartKeepsRecordedOutcome(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()

	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	// A restart turns the in flight execution into pending without an outcome.
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
	// A completion with no trustworthy usage cannot close it.
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded, StatusCode: 200})
	if got := h.request(req.ID); got.State != statePending || got.Outcome != OutcomeSucceeded {
		t.Fatalf("after completion = %+v, want pending with the outcome recorded", got)
	}
	// The late usage then settles it, and the amount is derived, not stored.
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 0, 0)))
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	if cost, _ := h.costOf(key.ID); cost != 100 {
		t.Fatalf("cost = %d, want 100", cost)
	}
	// Repeating the same completion stays idempotent, so a retried native event
	// never turns into a failure.
	if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeSucceeded, StatusCode: 200}); err != nil {
		t.Fatalf("repeated late completion: %v", err)
	}
	// Changing it is still refused.
	if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
		Outcome: OutcomeFailed, StatusCode: 500}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting completion err = %v, want ErrConflict", err)
	}
	if cost, _ := h.costOf(key.ID); cost != 100 {
		t.Fatalf("refused completions changed cost: %d", cost)
	}
}

// TestLateUnaccountedExecutionReopensSettledRequest covers the asynchronous
// case: a closed request receives an execution whose accounting is unknown, so
// it must become pending again instead of silently absorbing the leak.
func TestLateUnaccountedExecutionReopensSettledRequest(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()

	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(9, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	cost := h.report(key.ID).Summary.CostMicroUSD

	late := usageFor("u2", req, "m1", Tokens{})
	late.Quality = QualityUnknown
	h.mustUsage(late)
	got := h.request(req.ID)
	if got.State != statePending {
		t.Fatalf("state = %q, want pending after a late unaccounted execution", got.State)
	}
	if after := h.report(key.ID).Summary.CostMicroUSD; after != cost {
		t.Fatalf("reopening changed cost: %d, want %d", after, cost)
	}
	// A second unknown event is idempotent and adds one more unpriced execution.
	second := usageFor("u3", req, "m1", Tokens{})
	second.Quality = QualityUnknown
	h.mustUsage(second)
	sum := h.report(key.ID).Summary
	if sum.CostMicroUSD != cost || sum.UnpricedExecutions != 2 {
		t.Fatalf("summary after second unknown event = %+v, want cost %d and 2 unpriced", sum, cost)
	}
	if has, err := h.store.HasActive(ctx); err != nil || has {
		t.Fatalf("HasActive = %v/%v, want false for a pending request", has, err)
	}
}

// TestFailedExecutionDoesNotProveSuccess keeps the success outcome honest: a
// reliable but failed lifecycle event never closes a successful request, while
// a failed request with accounted usage still settles.
func TestFailedExecutionDoesNotProveSuccess(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})

	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1"})
	failed := usageFor("u1", req, "m1", tokens(10, 0, 0, 0, 0))
	failed.Failed = true
	h.mustUsage(failed)
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state = %q, want pending: a failed execution cannot prove success", got.State)
	}
	// A reliable successful execution then closes it.
	h.mustUsage(usageFor("u2", req, "m1", tokens(10, 0, 0, 0, 0)))
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}

	// A failed lifecycle with accounted executions is already terminal.
	other := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
		RequestedModel: "m1"})
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
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1"})

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
	if cost, _ := h.costOf(key.ID); cost != 10 {
		t.Fatalf("cost = %d, want a single 10 microUSD charge", cost)
	}
}
