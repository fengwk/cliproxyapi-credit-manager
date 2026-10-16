package credit

import (
	"strings"
	"testing"
	"time"
)

// Audit storage is part of the mutation transaction, not a best-effort side
// effect: failed audit writes must roll back money, rules and resolutions.
func TestFinancialManagementAuditIsTransactional(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx()
	key, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustPrice("m", Rates{Input: 1_000_000})
	rule := PricingRule{ID: "r", MatchKind: MatchExact, Pattern: "m", Enabled: true, Price: Price{Rates: Rates{Input: 2_000_000}}}
	if err := h.store.PutPricingRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	req := h.mustReserve(ReserveInput{ID: "pending", TraceID: "trace", KeyID: key.ID, RequestedModel: "m", InputEstimate: 5})
	h.mustComplete(Completion{ID: req.ID, Outcome: OutcomeSucceeded})
	for _, action := range []string{ActionPriceUpdate, ActionRuleUpdate, ActionRuleDelete, ActionResolve} {
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
			case ActionResolve:
				err = h.store.Resolve(ctx, req.ID, Resolution{Action: ActionCharge, AmountMicroUSD: 123, Reason: "review"})
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
			if got := h.key(key.ID); got.SpentMicroUSD != 0 || got.HeldMicroUSD != 10 {
				t.Fatalf("failed audit changed ledger: %+v", got)
			}
			if h.request(req.ID).State != statePending || h.report(key.ID).Summary.AdjustmentMicroUSD != 0 {
				t.Fatal("failed audit closed the unresolved request or left a phantom adjustment")
			}
		})
	}
	if err := h.store.Resolve(ctx, req.ID, Resolution{Action: ActionCharge, AmountMicroUSD: 123, Reason: "review"}); err != nil {
		t.Fatal(err)
	}
	page, err := h.store.Audit(ctx, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, event := range page.Items {
		count[event.Action]++
	}
	if count[ActionPriceUpdate] != 1 || count[ActionRuleUpdate] != 1 || count[ActionResolve] != 1 || count[ActionRuleDelete] != 0 {
		t.Fatalf("phantom or missing audit events: %+v", count)
	}
}

// Throughput's numerator must cover only executions whose measured latency
// contributes to its denominator; unmeasured output cannot inflate the rate.
func TestModelEfficiencyPairsOutputWithMeasuredLatency(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustPrice("m", Rates{Output: 1_000_000})
	req := h.mustReserve(ReserveInput{ID: "r", TraceID: "t", KeyID: key.ID, RequestedModel: "m"})
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
}

// Reset metadata follows the same retention rule as its period counters.
// Lifetime baselines remain durable after old detail and periods disappear.
func TestCleanupPrunesPeriodResetBaselinesNotLifetime(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustPrice("m", Rates{Input: 1_000_000})
	req := h.mustReserve(ReserveInput{ID: "r", TraceID: "t", KeyID: key.ID, RequestedModel: "m"})
	h.mustUsage(usageFor("u", req, "m", tokens(10, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, Outcome: OutcomeSucceeded})
	_, err := h.store.ResetSpend(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Daily: true, Weekly: true, Monthly: true, Reason: "new allocation"})
	if err != nil {
		t.Fatal(err)
	}
	h.clock.advance(365 * 24 * time.Hour)
	if _, err := h.store.Cleanup(h.ctx()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"key_money_baselines", "key_token_baselines"} {
		n, err := counterValue(h.ctx(), h.store.db, `SELECT COUNT(*) FROM `+table+` WHERE win != ?`, windowTotal)
		if err != nil || n != 0 {
			t.Fatalf("%s retained aged periods: %d %v", table, n, err)
		}
	}
	if k := h.key(key.ID); k.SpentMicroUSD != 10 || k.QuotaUsedMicroUSD != 0 {
		t.Fatalf("cleanup erased lifetime reset: %+v", k)
	}
}
