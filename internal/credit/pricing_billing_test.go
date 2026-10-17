package credit

import (
	"errors"
	"math"
	"testing"
)

// TestTierSelectionPricesFromTheLog pins the tier contract at read time: the
// matching service tier wins, else the highest context tier reached by the actual
// canonical prompt size, which includes cache read and cache write.
func TestTierSelectionPricesFromTheLog(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "tiers", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}},
		Tiers: []PriceTier{
			{Kind: priceTierContext, Threshold: 100, Label: "100K", Rates: Rates{Input: 5 * million}},
			{Kind: priceTierService, Service: "priority", Rates: Rates{Output: 9 * million}}}})
	// The service tier wins over the reached context tier.
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1", ServiceTier: "PRIORITY"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(150, 0, 0, 10, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if cost, unpriced := h.costOf(key.ID); cost != 240 || unpriced != 0 {
		t.Fatalf("service-tier cost = %d/%d, want 240", cost, unpriced)
	}
	// Without a service tier the context tier applies, and the context size counts
	// uncached input plus cache read plus cache write.
	req2 := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u2", req2, "m1", tokens(50, 60, 0, 10, 0)))
	h.mustComplete(Completion{ID: req2.ID, TraceID: req2.TraceID, Outcome: OutcomeSucceeded})
	if cost, _ := h.costOf(key.ID); cost != 240+270 {
		t.Fatalf("context-tier cost = %d, want 510", cost)
	}
	// An unrecognized service tier stays below the context threshold and falls
	// back to the base rates.
	req3 := h.mustAdmit(AdmitInput{ID: "r3", TraceID: "t3", KeyID: key.ID, RequestedModel: "m1", ServiceTier: "weird"})
	h.mustUsage(usageFor("u3", req3, "m1", tokens(0, 0, 0, 10, 0)))
	h.mustComplete(Completion{ID: req3.ID, TraceID: req3.TraceID, Outcome: OutcomeSucceeded})
	if cost, _ := h.costOf(key.ID); cost != 240+270+20 {
		t.Fatalf("unknown-tier cost = %d, want 530", cost)
	}
}

// TestServiceTierNormalization pins the canonical tier keys.
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
// summed exactly and rounded up once, never per bucket, and an all-zero rate card
// is an explicit free price.
func TestTokenCostRoundsOnceOverTheSum(t *testing.T) {
	one := Rates{Input: 1, Output: 1}
	got, err := ratesCost(one, Tokens{Input: 1, Output: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("two single-microUSD buckets = %d, want one rounded microUSD", got)
	}
	single, err := ratesCost(one, Tokens{Input: 1})
	if err != nil || single != 1 {
		t.Fatalf("single bucket = %d %v, want 1", single, err)
	}
	if zero, err := ratesCost(Rates{}, Tokens{Input: 1, Output: 1}); err != nil || zero != 0 {
		t.Fatalf("free price = %d %v, want 0", zero, err)
	}
}

// TestCostOverflowIsRejected protects the derived integer arithmetic from silent
// wraparound.
func TestCostOverflowIsRejected(t *testing.T) {
	if _, err := ratesCost(Rates{Input: math.MaxInt64}, Tokens{Input: math.MaxInt64}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("token cost overflow err = %v, want ErrInvalid", err)
	}
	if _, err := perImageCost(Price{BillingMode: billingPerImage, PerImageMicroUSD: math.MaxInt64}, 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("per-image overflow err = %v, want ErrInvalid", err)
	}
	// The step sum itself is overflow checked, so a selection whose priced
	// executions exceed int64 fails loudly instead of wrapping.
	tally := costTally{}
	if err := tally.add(math.MaxInt64, true); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := tally.add(math.MaxInt64, true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("sum overflow err = %v, want ErrInvalid", err)
	}
}

// TestPerImagePriceIsPreviewedButNeverLogged makes the image decision explicit:
// the operator can still define and preview a per-image price, but recorded
// executions of that model stay unpriced because the host reports no real image
// count.
func TestPerImagePriceIsPreviewedButNeverLogged(t *testing.T) {
	h := newHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img"})
	// A failed first attempt and a successful retry are both recorded, and both
	// stay unpriced: no per-execution image count can be proven from the log.
	attempt := usageFor("u-failed", req, "img", tokens(1, 0, 0, 1, 0))
	attempt.Failed = true
	h.mustUsage(attempt)
	h.mustUsage(usageFor("u-ok", req, "img", tokens(1, 0, 0, 1, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %s, want settled", got.State)
	}
	if cost, unpriced := h.costOf(key.ID); cost != 0 || unpriced != 2 {
		t.Fatalf("per-image cost = %d/%d, want 0 cost and 2 unpriced executions", cost, unpriced)
	}
	for _, exec := range h.request(req.ID).Executions {
		if exec.CostMicroUSD != nil {
			t.Fatalf("per-image execution %s cost = %d, want null", exec.ID, *exec.CostMicroUSD)
		}
	}
	// The preview still describes the price exactly.
	match, err := h.store.PreviewPrice(h.ctx(), "img", "", 0)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if match.BillingMode != billingPerImage || match.CostMicroUSD != 50_000 {
		t.Fatalf("preview = %+v, want a per-image price of 50000", match)
	}
}

// TestUsageServiceTierReplayAndFallback pins that the reported tier is persisted
// and part of the replay identity, that an omitted tier falls back to the request
// tier, and that a mismatching tier conflicts instead of silently repricing.
func TestUsageServiceTierReplayAndFallback(t *testing.T) {
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
	if cost, _ := h.costOf(key.ID); cost != 90 {
		t.Fatalf("priority cost = %d, want 90", cost)
	}
	h.mustUsage(usage) // identical replay
	if cost, _ := h.costOf(key.ID); cost != 90 {
		t.Fatalf("replay changed cost: %d", cost)
	}
	conflict := usage
	conflict.ServiceTier = "flex"
	if err := h.store.RecordUsage(h.ctx(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatching tier replay err = %v, want ErrConflict", err)
	}
	// An omitted tier uses the request tier and persists it.
	req2 := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1", ServiceTier: "flex"})
	h.mustUsage(usageFor("u2", req2, "m1", tokens(0, 0, 0, 10, 0)))
	if cost, _ := h.costOf(key.ID); cost != 130 {
		t.Fatalf("inherited flex cost = %d, want 130", cost)
	}
	execs := h.request(req2.ID).Executions
	if len(execs) != 1 || execs[0].ServiceTier != "flex" {
		t.Fatalf("persisted tier = %+v, want flex", execs)
	}
}

// TestUsageReportedDefaultTierStaysBase pins the presence semantics of the
// reported service tier: an explicitly reported tier that means "no special
// tier" must price at the base rate instead of inheriting the request tier.
// Inheriting it would overcharge a request whose upstream answered on the
// default tier, and it would also make the persisted replay identity lie.
func TestUsageReportedDefaultTierStaysBase(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "svc", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}},
		Tiers: []PriceTier{{Kind: priceTierService, Service: "priority", Rates: Rates{Output: 9 * million}}}})

	// The request asks for priority, so priority is the request tier fact.
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1", ServiceTier: "priority"})

	// The upstream explicitly reports the default tier: ten output tokens price
	// at the base rate (20), not at the request's priority rate (90).
	usage := usageFor("u1", req, "m1", tokens(0, 0, 0, 10, 0))
	usage.ServiceTier = "default"
	h.mustUsage(usage)
	if cost, _ := h.costOf(key.ID); cost != 20 {
		t.Fatalf("reported default tier cost = %d, want 20: a reported default must not inherit the request tier", cost)
	}
	if tier := h.request(req.ID).Executions[0].ServiceTier; tier != "" {
		t.Fatalf("persisted tier = %q, want the normalized none", tier)
	}
	// The normalized value is the replay identity: another default alias
	// normalizes to the same none, so a replay must still be idempotent.
	replay := usage
	replay.ServiceTier = "auto"
	h.mustUsage(replay)
	if cost, _ := h.costOf(key.ID); cost != 20 {
		t.Fatalf("normalized alias replay charged again: %d", cost)
	}
	// An omitted tier is a different statement: it falls back to the request tier.
	req2 := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
		RequestedModel: "m1", ServiceTier: "priority"})
	h.mustUsage(usageFor("u2", req2, "m1", tokens(0, 0, 0, 10, 0)))
	if cost, _ := h.costOf(key.ID); cost != 110 {
		t.Fatalf("omitted-tier cost = %d, want 110 (20 base + 90 inherited priority)", cost)
	}
	if tier := h.request(req2.ID).Executions[0].ServiceTier; tier != "priority" {
		t.Fatalf("persisted inherited tier = %q, want priority", tier)
	}
}

// TestDisabledRuleIsSkippedForPricing pins that a disabled rule simply stops
// participating in pricing: the exact price book entry behind it wins instead of
// the model becoming unpriced or denied.
func TestDisabledRuleIsSkippedForPricing(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "off", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: false,
		Price: Price{Rates: Rates{Input: 500 * million}}})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(0, 0, 0, 100, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	// The base price book entry (output 2 microUSD/1M) applies: 100 tokens = 200.
	if cost, unpriced := h.costOf(key.ID); cost != 200 || unpriced != 0 {
		t.Fatalf("cost = %d/%d, want 200 priced by the price book entry", cost, unpriced)
	}
}
