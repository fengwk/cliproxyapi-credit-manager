package credit

import (
	"context"
	"errors"
	"math"
	"testing"
)

// TestTierSelectionAdmitAndSettlement pins the tier contract: settlement applies
// the matching service tier, else the highest context tier reached by the actual
// canonical prompt size, which includes cache read and cache write. Admission
// itself prices nothing.
func TestTierSelectionAdmitAndSettlement(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "tiers", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}},
		Tiers: []PriceTier{
			{Kind: priceTierContext, Threshold: 100, Label: "100K", Rates: Rates{Input: 5 * million}},
			{Kind: priceTierService, Service: "priority", Rates: Rates{Output: 9 * million}}}})
	// The service tier wins over the reached context tier at settlement.
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1", ServiceTier: "PRIORITY"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(150, 0, 0, 10, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if key = h.key(key.ID); key.SpentMicroUSD != 240 {
		t.Fatalf("service-tier settlement spent = %d, want 240", key.SpentMicroUSD)
	}
	// Without a service tier the context tier applies at settlement, and the
	// context size counts uncached input plus cache read plus cache write.
	req2 := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u2", req2, "m1", tokens(50, 60, 0, 10, 0)))
	h.mustComplete(Completion{ID: req2.ID, TraceID: req2.TraceID, Outcome: OutcomeSucceeded})
	if key = h.key(key.ID); key.SpentMicroUSD != 240+270 {
		t.Fatalf("context settlement spent = %d, want 510", key.SpentMicroUSD)
	}
	// An unrecognized service tier falls back to the base rates.
	req3 := h.mustAdmit(AdmitInput{ID: "r3", TraceID: "t3", KeyID: key.ID, RequestedModel: "m1", ServiceTier: "weird"})
	h.mustUsage(usageFor("u3", req3, "m1", tokens(0, 0, 0, 10, 0)))
	h.mustComplete(Completion{ID: req3.ID, TraceID: req3.TraceID, Outcome: OutcomeSucceeded})
	if key = h.key(key.ID); key.SpentMicroUSD != 240+270+20 {
		t.Fatalf("unknown tier spent = %d, want 530", key.SpentMicroUSD)
	}
}

// TestServiceTierNormalization pins the canonical billing keys.
func TestServiceTierNormalization(t *testing.T) {
	for input, want := range map[string]string{
		"": "", " ": "", "auto": "", "default": "", "standard": "", "on-demand": "",
		"on_demand": "", "scale": "", "PRIORITY": "priority", "prio": "priority",
		"flex": "flex", " Priority ": "priority"} {
		if got := normalizeServiceTier(input); got != want {
			t.Fatalf("normalizeServiceTier(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestTokenCostRoundsOnceOverTheSum pins the exact integer contract: buckets are
// summed exactly and rounded up once, never per bucket, and a positive priced
// usage is never free.
func TestTokenCostRoundsOnceOverTheSum(t *testing.T) {
	one := Price{Rates: Rates{Input: 1, Output: 1}}
	got, err := computeCost(one, Tokens{Input: 1, Output: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("two single-microUSD buckets = %d, want one rounded microUSD", got)
	}
	single, err := computeCost(one, Tokens{Input: 1})
	if err != nil || single != 1 {
		t.Fatalf("single bucket = %d %v, want 1", single, err)
	}
	if zero, err := computeCost(Price{}, Tokens{Input: 1, Output: 1}); err != nil || zero != 0 {
		t.Fatalf("free price = %d %v, want 0", zero, err)
	}
}

// TestCostOverflowIsRejected protects the integer ledger from silent wraparound.
func TestCostOverflowIsRejected(t *testing.T) {
	if _, err := computeCost(Price{Rates: Rates{Input: math.MaxInt64}}, Tokens{Input: math.MaxInt64}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("token cost overflow err = %v, want ErrInvalid", err)
	}
	if _, err := perImageCost(Price{BillingMode: billingPerImage, PerImageMicroUSD: math.MaxInt64}, 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("per-image overflow err = %v, want ErrInvalid", err)
	}
	// Admission never estimates a cost, so an overflowing price is caught when
	// the actual usage is priced, not at admission.
	h, key := billingHarness(t)
	if err := h.store.PutPrice(context.Background(), Price{Model: "huge", Rates: Rates{Input: math.MaxInt64}}); err != nil {
		t.Fatal(err)
	}
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "huge"})
	// Raw is left empty so the failure comes from pricing the tokens against
	// the huge rate, not from an overflowing raw counter sum.
	u := Usage{ID: "u1", RequestID: req.ID, TraceID: req.TraceID, KeyID: key.ID,
		Model: "huge", Tokens: tokens(math.MaxInt64, 0, 0, 0, 0), Quality: QualityComplete}
	if err := h.store.RecordUsage(h.ctx(), u); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflowing usage err = %v, want ErrInvalid", err)
	}
}

// TestPerImageBillingOnceOnProvenSuccess pins the per-image financial contract:
// the frozen requested count is billed exactly once, on a proven successful
// lifecycle, never per attempt and never for a failed lifecycle.
func TestPerImageBillingOnceOnProvenSuccess(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img", ImageCount: 3, ImageCountDeclared: true})
	// A failed first attempt and a successful retry must still bill one count.
	attempt := usageFor("u-failed", req, "img", tokens(1, 0, 0, 1, 0))
	attempt.Failed = true
	h.mustUsage(attempt)
	h.mustUsage(usageFor("u-ok", req, "img", tokens(1, 0, 0, 1, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if key = h.key(key.ID); key.SpentMicroUSD != 150_000 {
		t.Fatalf("per-image spent=%d, want 150000", key.SpentMicroUSD)
	}
	// A replay of the successful attempt must not double charge.
	h.mustUsage(usageFor("u-ok", req, "img", tokens(1, 0, 0, 1, 0)))
	if key = h.key(key.ID); key.SpentMicroUSD != 150_000 {
		t.Fatalf("replay double charged: spent=%d", key.SpentMicroUSD)
	}
}

// TestPerImageFailedLifecycleIsNotCharged pins that a failed lifecycle never
// bills the requested images.
func TestPerImageFailedLifecycleIsNotCharged(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img", ImageCount: 2, ImageCountDeclared: true})
	h.mustUsage(usageFor("u1", req, "img", tokens(1, 0, 0, 1, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeFailed})
	if key = h.key(key.ID); key.SpentMicroUSD != 0 {
		t.Fatalf("failed lifecycle spent=%d, want 0", key.SpentMicroUSD)
	}
}

// TestPerImageSucceededWithoutUsageStaysPending pins that a successful lifecycle
// with no usage is not written off: the request stays pending until trustworthy
// usage arrives, and only then is the frozen count billed once.
func TestPerImageSucceededWithoutUsageStaysPending(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img", ImageCount: 2, ImageCountDeclared: true})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state = %s, want pending while no usage proves the images", got.State)
	}
	if key = h.key(key.ID); key.SpentMicroUSD != 0 {
		t.Fatalf("pending per-image spent=%d, want 0", key.SpentMicroUSD)
	}
	// A late trustworthy usage books the frozen count once and settles.
	h.mustUsage(usageFor("u-late", req, "img", tokens(1, 0, 0, 1, 0)))
	if key = h.key(key.ID); key.SpentMicroUSD != 100_000 {
		t.Fatalf("late usage spent=%d, want 100000", key.SpentMicroUSD)
	}
}

// TestPerImageUnknownUsageStaysPending pins that untrustworthy accounting never
// pays for images silently.
func TestPerImageUnknownUsageStaysPending(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img", ImageCount: 1, ImageCountDeclared: true})
	unknown := usageFor("u-unknown", req, "img", Tokens{})
	unknown.Quality = QualityUnknown
	h.mustUsage(unknown)
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state = %s, want pending for unknown accounting", got.State)
	}
	if key = h.key(key.ID); key.SpentMicroUSD != 0 {
		t.Fatalf("unknown accounting spent=%d, want 0", key.SpentMicroUSD)
	}
}

// TestUsageServiceTierReplayAndFrozenFallback pins that the billed tier is
// persisted and part of the replay identity, that an omitted tier falls back to
// the tier frozen at admission, and that a mismatching tier conflicts instead of
// silently repricing.
func TestUsageServiceTierReplayAndFrozenFallback(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "svc", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}},
		Tiers: []PriceTier{
			{Kind: priceTierService, Service: "priority", Rates: Rates{Output: 9 * million}},
			{Kind: priceTierService, Service: "flex", Rates: Rates{Output: 4 * million}}}})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1", ServiceTier: "priority"})
	usage := usageFor("u1", req, "m1", tokens(0, 0, 0, 10, 0))
	usage.ServiceTier = "priority"
	h.mustUsage(usage)
	if key = h.key(key.ID); key.SpentMicroUSD != 90 {
		t.Fatalf("priority spent = %d, want 90", key.SpentMicroUSD)
	}
	h.mustUsage(usage) // identical replay
	if key = h.key(key.ID); key.SpentMicroUSD != 90 {
		t.Fatalf("replay changed spend: %d", key.SpentMicroUSD)
	}
	conflict := usage
	conflict.ServiceTier = "flex"
	if err := h.store.RecordUsage(h.ctx(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatching tier replay err = %v, want ErrConflict", err)
	}
	// An omitted tier uses the tier frozen at admission and persists it.
	req2 := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1", ServiceTier: "flex"})
	h.mustUsage(usageFor("u2", req2, "m1", tokens(0, 0, 0, 10, 0)))
	if key = h.key(key.ID); key.SpentMicroUSD != 130 {
		t.Fatalf("frozen flex spent = %d, want 130", key.SpentMicroUSD)
	}
	execs := h.request(req2.ID).Executions
	if len(execs) != 1 || execs[0].ServiceTier != "flex" {
		t.Fatalf("persisted tier = %+v, want flex", execs)
	}
}

// TestUsageReportedDefaultTierStaysBase pins the presence semantics of the
// reported service tier: an explicitly reported tier that means "no special
// tier" must bill the base rate instead of inheriting the tier frozen at
// admission. Inheriting the frozen priority tier would overcharge a request
// whose upstream answered on the default tier, and it would also make the
// persisted replay identity lie about what was billed.
func TestUsageReportedDefaultTierStaysBase(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "svc", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}},
		Tiers: []PriceTier{{Kind: priceTierService, Service: "priority", Rates: Rates{Output: 9 * million}}}})

	// The request asks for priority, so priority is frozen on the snapshot.
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1", ServiceTier: "priority"})

	// The upstream explicitly reports the default tier: ten output tokens bill
	// at the base rate (20), not at the frozen priority rate (90).
	usage := usageFor("u1", req, "m1", tokens(0, 0, 0, 10, 0))
	usage.ServiceTier = "default"
	h.mustUsage(usage)
	if key = h.key(key.ID); key.SpentMicroUSD != 20 {
		t.Fatalf("reported default tier spent = %d, want 20: a reported default must not inherit frozen priority",
			key.SpentMicroUSD)
	}
	if tier := h.request(req.ID).Executions[0].ServiceTier; tier != "" {
		t.Fatalf("persisted tier = %q, want the normalized none", tier)
	}
	// The normalized value is the replay identity: another default alias
	// normalizes to the same none, so a replay must still be idempotent.
	replay := usage
	replay.ServiceTier = "auto"
	h.mustUsage(replay)
	if key = h.key(key.ID); key.SpentMicroUSD != 20 {
		t.Fatalf("normalized alias replay charged again: %d", key.SpentMicroUSD)
	}
	// An omitted tier is a different statement: it falls back to the frozen one.
	req2 := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
		RequestedModel: "m1", ServiceTier: "priority"})
	h.mustUsage(usageFor("u2", req2, "m1", tokens(0, 0, 0, 10, 0)))
	if key = h.key(key.ID); key.SpentMicroUSD != 110 {
		t.Fatalf("omitted-tier spent = %d, want 110 (20 base + 90 frozen priority)", key.SpentMicroUSD)
	}
	if tier := h.request(req2.ID).Executions[0].ServiceTier; tier != "priority" {
		t.Fatalf("persisted frozen tier = %q, want priority", tier)
	}
}

// TestPerImageChargeReconcilesReportAndLedger pins the accounting conservation
// contract: every per-image attempt is recorded as a known cost (zero for an
// attempt), exactly one successful execution carries the single lifecycle
// charge, and the report cost plus manual adjustments reconciles with the key
// ledger that was actually charged.
func TestPerImageChargeReconcilesReportAndLedger(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img",
		ImageCount: 2, ImageCountDeclared: true})
	failed := usageFor("u-failed", req, "img", tokens(1, 0, 0, 1, 0))
	failed.Failed = true
	h.mustUsage(failed)
	h.mustUsage(usageFor("u-ok", req, "img", tokens(1, 0, 0, 1, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})

	execs := h.request(req.ID).Executions
	if len(execs) != 2 {
		t.Fatalf("executions = %d, want 2", len(execs))
	}
	costs := map[string]*int64{}
	for i := range execs {
		costs[execs[i].ID] = execs[i].CostMicroUSD
	}
	if got := costs["u-failed"]; got == nil || *got != 0 {
		t.Fatalf("failed attempt cost = %v, want a known 0", got)
	}
	if got := costs["u-ok"]; got == nil || *got != 100_000 {
		t.Fatalf("successful execution cost = %v, want the booked 100000", got)
	}
	report := h.report(key.ID)
	if report.Summary.CostMicroUSD != 100_000 || report.Summary.AdjustmentMicroUSD != 0 {
		t.Fatalf("report cost=%d adjustments=%d, want 100000/0",
			report.Summary.CostMicroUSD, report.Summary.AdjustmentMicroUSD)
	}
	if got := h.key(key.ID).SpentMicroUSD; got != report.Summary.CostMicroUSD+report.Summary.AdjustmentMicroUSD {
		t.Fatalf("ledger spend %d != report cost %d + adjustments %d", got,
			report.Summary.CostMicroUSD, report.Summary.AdjustmentMicroUSD)
	}
	// A replay of the successful attempt must not move the charge again.
	h.mustUsage(usageFor("u-ok", req, "img", tokens(1, 0, 0, 1, 0)))
	if got := h.key(key.ID).SpentMicroUSD; got != 100_000 {
		t.Fatalf("replay changed the booked charge: %d", got)
	}
	// A manual charge on a still-pending per-image request stays an adjustment:
	// it is neither hidden inside the execution cost nor double counted.
	req2 := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "img",
		ImageCount: 1, ImageCountDeclared: true})
	h.mustComplete(Completion{ID: req2.ID, TraceID: req2.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req2.ID).State; got != statePending {
		t.Fatalf("succeeded-without-usage state = %s, want pending", got)
	}
	if err := h.store.Resolve(h.ctx(), req2.ID, Resolution{
		Action: ActionCharge, AmountMicroUSD: 50_000, Reason: "manual adjustment"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	report = h.report(key.ID)
	if report.Summary.CostMicroUSD != 100_000 || report.Summary.AdjustmentMicroUSD != 50_000 {
		t.Fatalf("report after adjustment cost=%d adjustments=%d, want 100000/50000",
			report.Summary.CostMicroUSD, report.Summary.AdjustmentMicroUSD)
	}
	if got := h.key(key.ID).SpentMicroUSD; got != report.Summary.CostMicroUSD+report.Summary.AdjustmentMicroUSD {
		t.Fatalf("ledger spend after adjustment %d != report cost %d + adjustments %d", got,
			report.Summary.CostMicroUSD, report.Summary.AdjustmentMicroUSD)
	}
}

// TestImageCountFailClosedAtAdmission pins that an unsupported explicit image
// count is refused for a per-image price instead of being clamped to a billable
// number, that an omitted count means one image, and that the same hint on a
// token price is irrelevant rather than a rejection.
func TestImageCountFailClosedAtAdmission(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	for i, count := range []int64{0, -1, 33, 100, math.MaxInt64} {
		if _, err := h.store.Admit(h.ctx(), AdmitInput{ID: "bad" + string(rune('a'+i)), TraceID: "bt",
			KeyID: key.ID, RequestedModel: "img", ImageCount: count, ImageCountDeclared: true}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("declared image count %d err = %v, want ErrInvalid", count, err)
		}
	}
	if got := h.key(key.ID); got.SpentMicroUSD != 0 {
		t.Fatalf("refused requests charged %d, want 0", got.SpentMicroUSD)
	}
	// The supported boundaries are accepted and billed exactly once proven.
	boundaries := []struct {
		id    string
		count int64
	}{{"one", 1}, {"max", 32}}
	var want int64
	for _, tc := range boundaries {
		req := h.mustAdmit(AdmitInput{ID: tc.id, TraceID: "tr-" + tc.id, KeyID: key.ID,
			RequestedModel: "img", ImageCount: tc.count, ImageCountDeclared: true})
		h.mustUsage(usageFor("u-"+tc.id, req, "img", tokens(1, 0, 0, 1, 0)))
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
		want += tc.count * 50_000
		if got := h.key(key.ID).SpentMicroUSD; got != want {
			t.Fatalf("image count %d billed total = %d, want %d", tc.count, got, want)
		}
	}
	// An omitted count is one image, not a rejection, and bills once proven.
	req := h.mustAdmit(AdmitInput{ID: "r-omit", TraceID: "t-omit", KeyID: key.ID, RequestedModel: "img"})
	h.mustUsage(usageFor("u-omit", req, "img", tokens(1, 0, 0, 1, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	want += 50_000
	if got := h.key(key.ID).SpentMicroUSD; got != want {
		t.Fatalf("omitted-count billed total = %d, want %d", got, want)
	}
	// A token price ignores the hint entirely: an image hint is not a billing
	// input there, so it must never turn into a rejection.
	if _, err := h.store.Admit(h.ctx(), AdmitInput{ID: "r-tok", TraceID: "t-tok", KeyID: key.ID,
		RequestedModel: "m1", ImageCount: 100, ImageCountDeclared: true}); err != nil {
		t.Fatalf("token reserve with an irrelevant image hint err = %v, want nil", err)
	}
}
