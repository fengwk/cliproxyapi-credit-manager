package credit

import (
	"strings"
	"testing"
	"time"
)

// Audit storage is part of the mutation transaction, not a best-effort side
// effect: a failed audit write must roll back the price and rule change, and it
// must never leave a phantom lifecycle change behind.
func TestPricingManagementAuditIsTransactional(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx()
	key, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustPrice("m", Rates{Input: 1_000_000})
	rule := PricingRule{ID: "r", MatchKind: MatchExact, Pattern: "m", Enabled: true, Price: Price{Rates: Rates{Input: 2_000_000}}}
	if err := h.store.PutPricingRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	req := h.mustAdmit(AdmitInput{ID: "pending", TraceID: "trace", KeyID: key.ID, RequestedModel: "m"})
	h.mustComplete(Completion{ID: req.ID, Outcome: OutcomeSucceeded})
	for _, action := range []string{ActionPriceUpdate, ActionRuleUpdate, ActionRuleDelete} {
		t.Run(action, func(t *testing.T) {
			if _, err := h.store.db.ExecContext(ctx,
				`CREATE TEMP TRIGGER audit_failure BEFORE INSERT ON audit_events WHEN NEW.action = '`+action+`'
				BEGIN SELECT RAISE(ABORT, 'synthetic audit failure'); END`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := h.store.db.ExecContext(ctx, `DROP TRIGGER audit_failure`); err != nil {
					t.Fatal(err)
				}
			}()
			var err error
			switch action {
			case ActionPriceUpdate:
				err = h.store.PutPrice(ctx, Price{Model: "m", Rates: Rates{Input: 9_000_000}})
			case ActionRuleUpdate:
				modified := rule
				modified.Price.Rates.Input = 9_000_000
				err = h.store.PutPricingRule(ctx, modified)
			case ActionRuleDelete:
				err = h.store.DeletePricingRule(ctx, rule.ID)
			}
			if err == nil || !strings.Contains(err.Error(), "audit") {
				t.Fatalf("audit failure was ignored: %v", err)
			}
			prices, err := h.store.ListPrices(ctx)
			if err != nil || len(prices) != 1 || prices[0].Rates.Input != 1_000_000 {
				t.Fatalf("failed audit changed price: %+v %v", prices, err)
			}
			rules, err := h.store.ListPricingRules(ctx)
			if err != nil || len(rules) != 1 || rules[0].Price.Rates.Input != 2_000_000 {
				t.Fatalf("failed audit changed rule: %+v %v", rules, err)
			}
			// Nothing durable moved: the request is still pending and the
			// derived report is still empty.
			if h.request(req.ID).State != statePending {
				t.Fatal("failed audit closed the unresolved request")
			}
			if sum := h.report(key.ID).Summary; sum.CostMicroUSD != 0 || sum.UnpricedExecutions != 0 {
				t.Fatalf("failed audit changed the derived report: %+v", sum)
			}
		})
	}
	page, err := h.store.Audit(ctx, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, event := range page.Items {
		count[event.Action]++
	}
	if count[ActionPriceUpdate] != 1 || count[ActionRuleUpdate] != 1 || count[ActionRuleDelete] != 0 {
		t.Fatalf("phantom or missing audit events: %+v", count)
	}
}

// Throughput's numerator must cover only executions whose measured latency
// contributes to its denominator; unmeasured output cannot inflate the rate. The
// per-model cost of the same slice is priced by each execution's actual model,
// which here has no price at all.
func TestModelEfficiencyPairsOutputWithMeasuredLatency(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustPrice("m", Rates{Output: 1_000_000})
	req := h.mustAdmit(AdmitInput{ID: "r", TraceID: "t", KeyID: key.ID, RequestedModel: "m"})
	measured := usageFor("measured", req, "alias", tokens(0, 0, 0, 20, 0))
	measured.LatencyMS, measured.TTFTMS = 1000, 12
	h.mustUsage(measured)
	unmeasured := usageFor("unmeasured", req, "alias", tokens(0, 0, 0, 100, 0))
	h.mustUsage(unmeasured)
	h.mustComplete(Completion{ID: req.ID, Outcome: OutcomeSucceeded})
	overview, err := h.store.Analytics(h.ctx(), Filter{KeyID: key.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Models) != 1 {
		t.Fatalf("models=%+v", overview.Models)
	}
	m := overview.Models[0]
	if m.OutputTokens != 120 || m.MeasuredOutputTokens != 20 || m.LatencyMS != 1000 || m.LatencySamples != 1 || m.TTFTMS != 12 || m.TTFTSamples != 1 {
		t.Fatalf("inconsistent throughput sample: %+v", m)
	}
	// The requested model is the grouping dimension, and the executions are
	// unpriced because their actual model has no price.
	if m.Model != "m" || m.CostMicroUSD != 0 || m.UnpricedExecutions != 2 {
		t.Fatalf("model cost = %+v, want the alias executions unpriced under the requested model", m)
	}
}

// Reset metadata follows the same retention rule as its period counters. The
// lifetime token baseline remains durable after old detail and periods
// disappear, and no monetary table is involved at all.
func TestCleanupPrunesPeriodResetBaselinesNotLifetime(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true,
		ModelTokenLimits: []ModelTokenLimit{{Model: "m", Total: 10, Daily: 10, Weekly: 10, Monthly: 10}}})
	h.mustPrice("m", Rates{Input: 1_000_000})
	req := h.mustAdmit(AdmitInput{ID: "r", TraceID: "t", KeyID: key.ID, RequestedModel: "m"})
	h.mustUsage(usageFor("u", req, "m", tokens(10, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, Outcome: OutcomeSucceeded})
	if _, err := h.store.ResetTokens(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Daily: true, Weekly: true, Monthly: true, Reason: "new allocation"}); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(365 * 24 * time.Hour)
	if _, err := h.store.Cleanup(h.ctx()); err != nil {
		t.Fatal(err)
	}
	n, err := counterValue(h.ctx(), h.store.db,
		`SELECT COUNT(*) FROM key_token_baselines WHERE win != ?`, windowTotal)
	if err != nil || n != 0 {
		t.Fatalf("aged period baselines retained: %d %v", n, err)
	}
	lifetime, err := counterValue(h.ctx(), h.store.db,
		`SELECT COUNT(*) FROM key_token_baselines WHERE win = ?`, windowTotal)
	if err != nil || lifetime != 1 {
		t.Fatalf("lifetime baseline = %d (%v), want it kept", lifetime, err)
	}
	// The durable lifetime counter survived the cleanup, so the effective usage
	// the reset removed is still exactly the reset baseline.
	total, err := tokenCounter(h.ctx(), h.store.db, key.ID, "m", windowTotal, "")
	if err != nil || total != 10 {
		t.Fatalf("lifetime token counter = %d (%v), want 10 kept", total, err)
	}
	effective, err := effectiveToken(h.ctx(), h.store.db, key.ID, "m", windowTotal, "", total)
	if err != nil || effective != 0 {
		t.Fatalf("effective usage after reset = %d (%v), want 0", effective, err)
	}
}
