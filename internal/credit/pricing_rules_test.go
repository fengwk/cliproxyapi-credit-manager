package credit

import (
	"errors"
	"testing"
)

// mustRule stores one rule unless the test overrides Enabled itself.
func (h *harness) mustRule(r PricingRule) {
	h.t.Helper()
	if err := h.store.PutPricingRule(h.ctx(), r); err != nil {
		h.t.Fatalf("put pricing rule %s: %v", r.ID, err)
	}
}

// effectiveRate reports the input rate a model currently resolves to. The price
// book is the only source of that decision, so the preview is also the operator
// facing view of what would price history today.
func (h *harness) effectiveRate(model string) int64 {
	h.t.Helper()
	match, err := h.store.PreviewPrice(h.ctx(), model, "", 0)
	if err != nil {
		h.t.Fatalf("preview %s: %v", model, err)
	}
	return match.Rates.Input
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

// TestPricingRuleSelectionOrder pins the documented deterministic order: the
// exact price book is a priority zero exact candidate, a higher priority rule
// shadows it, an exact match beats a pattern match at the same priority and a
// lower id breaks a remaining tie.
func TestPricingRuleSelectionOrder(t *testing.T) {
	h, _ := billingHarness(t)
	// The exact price book entry is used when nothing shadows it.
	if got := h.effectiveRate("m1"); got != million {
		t.Fatalf("exact price rate = %d, want %d", got, million)
	}
	// A higher priority glob rule shadows the exact price.
	h.mustRule(PricingRule{ID: "shadow", MatchKind: MatchGlob, Pattern: "m*", Priority: 10, Enabled: true,
		Price: Price{Rates: Rates{Input: 9 * million}}})
	if got := h.effectiveRate("m1"); got != 9*million {
		t.Fatalf("shadowing rule rate = %d, want %d", got, 9*million)
	}
	// Two equally prioritized glob rules: the lower id wins.
	h.mustRule(PricingRule{ID: "b-second", MatchKind: MatchGlob, Pattern: "m*", Priority: 10, Enabled: true,
		Price: Price{Rates: Rates{Input: 5 * million}}})
	h.mustRule(PricingRule{ID: "a-first", MatchKind: MatchGlob, Pattern: "m*", Priority: 10, Enabled: true,
		Price: Price{Rates: Rates{Input: 3 * million}}})
	if got := h.effectiveRate("m1"); got != 3*million {
		t.Fatalf("id tie break rate = %d, want %d", got, 3*million)
	}
	// An exact match beats a pattern match at the same priority, even with a
	// higher id.
	h.mustRule(PricingRule{ID: "zz-exact", MatchKind: MatchExact, Pattern: "m1", Priority: 10, Enabled: true,
		Price: Price{Rates: Rates{Input: 7 * million}}})
	if got := h.effectiveRate("m1"); got != 7*million {
		t.Fatalf("exact-above-glob rate = %d, want %d", got, 7*million)
	}
}

// TestPricingRuleExactPriceBeatsSamePriorityPattern pins the documented
// tie-break against the price book: at priority zero the exact price wins over
// a pattern rule.
func TestPricingRuleExactPriceBeatsSamePriorityPattern(t *testing.T) {
	h, _ := billingHarness(t)
	h.mustRule(PricingRule{ID: "glob0", MatchKind: MatchGlob, Pattern: "m*", Priority: 0, Enabled: true,
		Price: Price{Rates: Rates{Input: 9 * million}}})
	if got := h.effectiveRate("m1"); got != million {
		t.Fatalf("same-priority glob shadowed the exact price: rate=%d", got)
	}
}

// TestPricingRuleDisabledRuleIsSkipped pins the disabled-rule decision: a
// disabled rule takes no part in pricing. It neither denies the model nor acts
// as a permission filter; the next candidate (here the exact price book entry)
// wins, and a model that only a disabled rule would price is simply unpriced.
func TestPricingRuleDisabledRuleIsSkipped(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "off", MatchKind: MatchGlob, Pattern: "m*", Priority: 10, Enabled: false,
		Price: Price{Rates: Rates{Input: 900 * million}}})
	if got := h.effectiveRate("m1"); got != million {
		t.Fatalf("disabled winning rule changed the price: rate=%d, want the exact price %d", got, million)
	}
	// A model that only the disabled rule matches is admitted and stays
	// unpriced; it is never rejected because a disabled rule would have matched.
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m-only"})
	h.mustUsage(usageFor("u1", req, "m-only", tokens(10, 0, 0, 5, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled", got.State)
	}
	if cost, unpriced := h.costOf(key.ID); cost != 0 || unpriced != 1 {
		t.Fatalf("cost = %d/%d, want the disabled-rule model unpriced", cost, unpriced)
	}
	if _, err := h.store.PreviewPrice(h.ctx(), "m-only", "", 0); !errors.Is(err, ErrUnpriced) {
		t.Fatalf("preview err = %v, want ErrUnpriced", err)
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

// TestPricingRuleEditAndDeleteRepriceHistory is the rule-side counterpart of the
// price book regression: because nothing is frozen on a request, editing and
// deleting a rule immediately changes the reported cost of the same history.
func TestPricingRuleEditAndDeleteRepriceHistory(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "rule", MatchKind: MatchGlob, Pattern: "m*", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}}})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(10, 0, 0, 5, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	// 10 input at 1 + 5 output at 2 = 20.
	if cost, _ := h.costOf(key.ID); cost != 20 {
		t.Fatalf("rule cost = %d, want 20", cost)
	}
	// Editing the rule recomputes the same history.
	h.mustRule(PricingRule{ID: "rule", MatchKind: MatchGlob, Pattern: "m*", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: 900 * million, Output: 900 * million}}})
	if cost, _ := h.costOf(key.ID); cost != 900*15 {
		t.Fatalf("edited-rule cost = %d, want %d", cost, 900*15)
	}
	// Deleting the rule falls back to the exact price book entry.
	if err := h.store.DeletePricingRule(h.ctx(), "rule"); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	if cost, _ := h.costOf(key.ID); cost != 20 {
		t.Fatalf("post-delete cost = %d, want the exact price 20", cost)
	}
	if got := h.effectiveRate("m1"); got != million {
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
// free, never "missing": the execution is priced zero while a genuinely unknown
// model is admitted but reported as unpriced.
func TestPricingRuleFreeModelIsNotMissing(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "free", MatchKind: MatchExact, Pattern: "free-model", Priority: 5, Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "free-model"})
	h.mustUsage(usageFor("u1", req, "free-model", tokens(10, 0, 0, 5, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if cost, unpriced := h.costOf(key.ID); cost != 0 || unpriced != 0 {
		t.Fatalf("free model cost = %d/%d, want a priced zero", cost, unpriced)
	}
	unknown := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "no-such-model"})
	h.mustUsage(usageFor("u2", unknown, "no-such-model", tokens(10, 0, 0, 5, 0)))
	h.mustComplete(Completion{ID: unknown.ID, TraceID: unknown.TraceID, Outcome: OutcomeSucceeded})
	if cost, unpriced := h.costOf(key.ID); cost != 0 || unpriced != 1 {
		t.Fatalf("unknown model cost = %d/%d, want one unpriced execution", cost, unpriced)
	}
}
