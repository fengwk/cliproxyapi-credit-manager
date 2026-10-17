package credit

import (
	"errors"
	"testing"
)

// mustRule stores one enabled rule unless the test overrides Enabled itself.
func (h *harness) mustRule(r PricingRule) {
	h.t.Helper()
	if err := h.store.PutPricingRule(h.ctx(), r); err != nil {
		h.t.Fatalf("put pricing rule %s: %v", r.ID, err)
	}
}

// TestPricingRuleScanKeepsCanonicalModel pins that a stored rule read back keeps
// its canonical price label: the table has no separate price.model column, so it
// is rebuilt from the pattern instead of being reported empty.
func TestPricingRuleScanKeepsCanonicalModel(t *testing.T) {
	h, _ := billingHarness(t)
	h.mustRule(PricingRule{ID: "r1", MatchKind: MatchGlob, Pattern: "gpt-*", Priority: 1, Enabled: true,
		Price: Price{Rates: Rates{Input: million}}})
	rules, err := h.store.ListPricingRules(h.ctx())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Price.Model != "gpt-*" {
		t.Fatalf("listed rules = %+v, want one rule labelled gpt-*", rules)
	}
}

// reserveRate reserves one model and returns the effective input rate frozen on
// the request, which is what the operator sees as the price that will be used.
func (h *harness) reserveRate(id, trace, model, keyID string) int64 {
	h.t.Helper()
	return h.mustAdmit(AdmitInput{ID: id, TraceID: trace, KeyID: keyID, RequestedModel: model}).Price.Rates.Input
}

// TestPricingRuleSelectionOrder pins the documented deterministic order: the
// exact price book is a priority zero exact candidate, a higher priority rule
// shadows it, an exact match beats a pattern match at the same priority and a
// lower id breaks a remaining tie.
func TestPricingRuleSelectionOrder(t *testing.T) {
	h, key := billingHarness(t)
	// The exact price book entry is used when nothing shadows it.
	if got := h.reserveRate("r1", "t1", "m1", key.ID); got != million {
		t.Fatalf("exact price rate = %d, want %d", got, million)
	}
	// A higher priority glob rule shadows the exact price.
	h.mustRule(PricingRule{ID: "shadow", MatchKind: MatchGlob, Pattern: "m*", Priority: 10, Enabled: true,
		Price: Price{Rates: Rates{Input: 9 * million}}})
	if got := h.reserveRate("r2", "t2", "m1", key.ID); got != 9*million {
		t.Fatalf("shadowing rule rate = %d, want %d", got, 9*million)
	}
	// Two equally prioritized glob rules: the lower id wins.
	h.mustRule(PricingRule{ID: "b-second", MatchKind: MatchGlob, Pattern: "m*", Priority: 10, Enabled: true,
		Price: Price{Rates: Rates{Input: 5 * million}}})
	h.mustRule(PricingRule{ID: "a-first", MatchKind: MatchGlob, Pattern: "m*", Priority: 10, Enabled: true,
		Price: Price{Rates: Rates{Input: 3 * million}}})
	if got := h.reserveRate("r3", "t3", "m1", key.ID); got != 3*million {
		t.Fatalf("id tie break rate = %d, want %d", got, 3*million)
	}
	// An exact match beats a pattern match at the same priority, even with a
	// higher id.
	h.mustRule(PricingRule{ID: "zz-exact", MatchKind: MatchExact, Pattern: "m1", Priority: 10, Enabled: true,
		Price: Price{Rates: Rates{Input: 7 * million}}})
	if got := h.reserveRate("r4", "t4", "m1", key.ID); got != 7*million {
		t.Fatalf("exact-above-glob rate = %d, want %d", got, 7*million)
	}
}

// TestPricingRuleExactPriceBeatsSamePriorityPattern pins the documented
// tie-break against the price book: at priority zero the exact price wins over
// a pattern rule.
func TestPricingRuleExactPriceBeatsSamePriorityPattern(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "glob0", MatchKind: MatchGlob, Pattern: "m*", Priority: 0, Enabled: true,
		Price: Price{Rates: Rates{Input: 9 * million}}})
	if got := h.reserveRate("r1", "t1", "m1", key.ID); got != million {
		t.Fatalf("same-priority glob shadowed the exact price: rate=%d", got)
	}
}

// TestPricingRuleDisabledWinningRuleDenies pins the fail-closed rule: a
// matching disabled rule that wins denies the model instead of falling through.
func TestPricingRuleDisabledWinningRuleDenies(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "off", MatchKind: MatchGlob, Pattern: "m*", Priority: 10, Enabled: false})
	_, err := h.store.Admit(h.ctx(), AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	if !errors.Is(err, ErrUnpriced) {
		t.Fatalf("disabled winning rule err = %v, want ErrUnpriced", err)
	}
	// A losing disabled rule must not deny a price that legitimately wins: with
	// the winning rule gone, the exact price book beats a same-priority pattern
	// rule whether or not that rule is enabled.
	if err := h.store.DeletePricingRule(h.ctx(), "off"); err != nil {
		t.Fatalf("delete winning rule: %v", err)
	}
	h.mustRule(PricingRule{ID: "off-glob", MatchKind: MatchGlob, Pattern: "m*", Priority: 0, Enabled: false})
	if got := h.reserveRate("r2", "t2", "m1", key.ID); got != million {
		t.Fatalf("disabled losing rule denied the exact price: rate=%d", got)
	}
}

// TestPricingRuleValidation rejects every malformed rule before it is stored.
func TestPricingRuleValidation(t *testing.T) {
	h, _ := billingHarness(t)
	valid := PricingRule{ID: "ok", MatchKind: MatchGlob, Pattern: "m*", Priority: 1, Enabled: true}
	cases := []struct {
		name string
		edit func(*PricingRule)
	}{
		{"empty-id", func(r *PricingRule) { r.ID = " " }},
		{"empty-pattern", func(r *PricingRule) { r.Pattern = "" }},
		{"bad-kind", func(r *PricingRule) { r.MatchKind = "fuzzy" }},
		{"bad-glob", func(r *PricingRule) { r.MatchKind = MatchGlob; r.Pattern = "[" }},
		{"bad-regexp", func(r *PricingRule) { r.MatchKind = MatchRegexp; r.Pattern = "(" }},
		{"bad-billing", func(r *PricingRule) { r.Price.BillingMode = "flat" }},
		{"per-image-without-mode", func(r *PricingRule) { r.Price.PerImageMicroUSD = 5 }},
		{"negative-rate", func(r *PricingRule) { r.Price.Rates.Input = -1 }},
		{"context-threshold-zero", func(r *PricingRule) {
			r.Tiers = []PriceTier{{Kind: priceTierContext, Rates: Rates{Input: 1}}}
		}},
		{"service-name-missing", func(r *PricingRule) {
			r.Tiers = []PriceTier{{Kind: priceTierService, Rates: Rates{Input: 1}}}
		}},
		{"service-threshold-set", func(r *PricingRule) {
			r.Tiers = []PriceTier{{Kind: priceTierService, Service: "priority", Threshold: 1}}
		}},
		{"unknown-tier-kind", func(r *PricingRule) {
			r.Tiers = []PriceTier{{Kind: "volume", Threshold: 1}}
		}},
		{"too-many-tiers", func(r *PricingRule) {
			r.Tiers = make([]PriceTier, maxPriceTiers+1)
			for i := range r.Tiers {
				r.Tiers[i] = PriceTier{Kind: priceTierContext, Threshold: int64(i + 1)}
			}
		}}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := valid
			tc.edit(&rule)
			if err := h.store.PutPricingRule(h.ctx(), rule); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
	// The valid rule was never stored by the rejected upserts.
	rules, err := h.store.ListPricingRules(h.ctx())
	if err != nil || len(rules) != 0 {
		t.Fatalf("rejected rules were stored: %+v %v", rules, err)
	}
}

// TestPricingRuleCRUD covers the management contract: upsert, ordered listing,
// idempotent overwrite and deletion.
func TestPricingRuleCRUD(t *testing.T) {
	h, _ := billingHarness(t)
	h.mustRule(PricingRule{ID: "low", MatchKind: MatchGlob, Pattern: "a*", Priority: 1, Enabled: true})
	h.mustRule(PricingRule{ID: "high", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: true})
	h.mustRule(PricingRule{ID: "mid", MatchKind: MatchRegexp, Pattern: "^m", Priority: 1, Enabled: true})
	rules, err := h.store.ListPricingRules(h.ctx())
	if err != nil {
		t.Fatal(err)
	}
	got := []string{rules[0].ID, rules[1].ID, rules[2].ID}
	if got[0] != "high" || got[1] != "low" || got[2] != "mid" {
		t.Fatalf("list order = %v, want [high low mid]", got)
	}
	// Overwriting an existing rule keeps the count stable and updates fields.
	overwritten := PricingRule{ID: "high", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: false,
		Price: Price{Rates: Rates{Input: million}}}
	h.mustRule(overwritten)
	rules, err = h.store.ListPricingRules(h.ctx())
	if err != nil || len(rules) != 3 {
		t.Fatalf("overwrite changed rule count: %d %v", len(rules), err)
	}
	if rules[0].Enabled || rules[0].Price.Rates.Input != million {
		t.Fatalf("overwrite did not persist: %+v", rules[0])
	}
	if err := h.store.DeletePricingRule(h.ctx(), "high"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := h.store.DeletePricingRule(h.ctx(), "high"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
	if err := h.store.DeletePricingRule(h.ctx(), " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty delete err = %v, want ErrInvalid", err)
	}
}

// TestPricingRuleSnapshotSurvivesEditAndDelete pins that an admitted request is
// never repriced by a later rule edit or deletion.
func TestPricingRuleSnapshotSurvivesEditAndDelete(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "rule", MatchKind: MatchGlob, Pattern: "m*", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}}})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	if req.Price.Rates.Input != million || req.Price.Rates.Output != 2*million {
		t.Fatalf("reserved price = %+v", req.Price.Rates)
	}
	// Edit the rule to a wildly different price and add a matcher, then delete it.
	h.mustRule(PricingRule{ID: "rule", MatchKind: MatchGlob, Pattern: "m*", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: 900 * million, Output: 900 * million}}})
	if err := h.store.DeletePricingRule(h.ctx(), "rule"); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	// Settlement uses the frozen price: 10 input + 5 output.
	h.mustUsage(usageFor("u1", req, "m1", tokens(10, 0, 0, 5, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	key = h.key(key.ID)
	if key.SpentMicroUSD != 20 {
		t.Fatalf("spent=%d, want the frozen 20 charge", key.SpentMicroUSD)
	}
	// A new request now falls back to the exact price book.
	if got := h.reserveRate("r2", "t2", "m1", key.ID); got != million {
		t.Fatalf("post-delete rate = %d, want the exact price %d", got, million)
	}
}

// TestPricePreviewSelectsTiersAndResolves covers the preview endpoint helper.
func TestPricePreviewSelectsTiersAndResolves(t *testing.T) {
	h, _ := billingHarness(t)
	h.mustRule(PricingRule{ID: "tiered", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}},
		Tiers: []PriceTier{
			{Kind: priceTierContext, Threshold: 100, Label: "100K", Rates: Rates{Input: 5 * million}},
			{Kind: priceTierService, Service: "priority", Rates: Rates{Input: 9 * million}}}})
	ctx := h.ctx()
	got, err := h.store.PreviewPrice(ctx, "m1", "", 150)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rates.Input != 5*million || got.CostMicroUSD != 750 {
		t.Fatalf("context preview = %+v", got)
	}
	got, err = h.store.PreviewPrice(ctx, "m1", "priority", 150)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rates.Input != 9*million || got.CostMicroUSD != 1350 || got.ServiceTier != "priority" {
		t.Fatalf("service preview = %+v", got)
	}
	if _, err := h.store.PreviewPrice(ctx, "unknown", "", 0); !errors.Is(err, ErrUnpriced) {
		t.Fatalf("unknown model preview err = %v, want ErrUnpriced", err)
	}
	if _, err := h.store.PreviewPrice(ctx, "", "", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty model preview err = %v, want ErrInvalid", err)
	}
	if _, err := h.store.PreviewPrice(ctx, "m1", "", -1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative tokens preview err = %v, want ErrInvalid", err)
	}
	// A per-image price previews one image.
	if err := h.store.PutPrice(ctx, Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	img, err := h.store.PreviewPrice(ctx, "img", "", 0)
	if err != nil || img.CostMicroUSD != 50_000 || img.BillingMode != billingPerImage {
		t.Fatalf("per-image preview = %+v %v", img, err)
	}
}

// TestPricingRuleFreeModelIsNotMissing pins that an explicit all-zero price is
// free, never "missing", and that a genuinely unknown model still denies.
func TestPricingRuleFreeModelIsNotMissing(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "free", MatchKind: MatchExact, Pattern: "free-model", Priority: 5, Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "free-model"})
	h.mustUsage(usageFor("u1", req, "free-model", tokens(10, 0, 0, 5, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if key = h.key(key.ID); key.SpentMicroUSD != 0 {
		t.Fatalf("free model spent=%d, want 0", key.SpentMicroUSD)
	}
	if _, err := h.store.Admit(h.ctx(), AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
		RequestedModel: "no-such-model"}); !errors.Is(err, ErrUnpriced) {
		t.Fatalf("missing price err = %v, want ErrUnpriced", err)
	}
}
