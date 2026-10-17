package credit

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

// TestSchemaStoresNoExecutionAmounts is the storage half of query pricing:
// no usage amount, request price snapshot or adjustment is persisted, so a
// price change reaches retained execution history. Price configuration remains.
func TestSchemaStoresNoExecutionAmounts(t *testing.T) {
	h := newHarness(t)
	forbiddenColumns := map[string][]string{
		"keys":     {"spent_micro_usd"},
		"requests": {"price_snapshot", "charge_micro_usd", "resolved"},
		"usages":   {"cost_micro_usd", "charged"},
	}
	for table, banned := range forbiddenColumns {
		present := tableColumns(t, h, table)
		for _, column := range banned {
			if _, ok := present[column]; ok {
				t.Fatalf("%s.%s still exists: an amount or price is persisted", table, column)
			}
		}
	}
	for _, table := range []string{"key_money_counters", "key_money_baselines", "adjustments"} {
		if tableExists(t, h, table) {
			t.Fatalf("table %s still exists", table)
		}
	}
	// Keys retain their four display-budget configuration fields.
	for _, column := range []string{"quota_micro_usd", "daily_micro_usd", "weekly_micro_usd", "monthly_micro_usd"} {
		if _, ok := tableColumns(t, h, "keys")[column]; !ok {
			t.Fatalf("display budget column keys.%s is missing", column)
		}
	}
}

func tableColumns(t *testing.T, h *harness, table string) map[string]struct{} {
	t.Helper()
	rows, err := h.store.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("read columns of %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	return out
}

func tableExists(t *testing.T, h *harness, table string) bool {
	t.Helper()
	var name string
	err := h.store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		t.Fatalf("inspect table %s: %v", table, err)
	}
	return true
}

// TestDerivedCostFollowsTheStoredLog proves the amount is a projection of the
// recorded executions rather than a stored number: rewriting the token counters
// of a stored execution changes the reported cost without touching any price.
func TestDerivedCostFollowsTheStoredLog(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(100, 0, 0, 100, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if cost, _ := h.costOf(key.ID); cost != 300 {
		t.Fatalf("cost = %d, want 300", cost)
	}
	// A direct rewrite of the log (bypassing the validating writer) is visible
	// in the derived amount: there is no shadow copy to disagree with it.
	if _, err := h.store.db.Exec(`UPDATE usages SET tokens_output = ? WHERE id = ?`, 500, "u1"); err != nil {
		t.Fatalf("rewrite log: %v", err)
	}
	if cost, _ := h.costOf(key.ID); cost != 100+1000 {
		t.Fatalf("cost after rewriting the log = %d, want 1100", cost)
	}
}

// TestPerImagePriceIsUnpricedFromTheLog pins the image-count decision: the host
// records tokens, not real image counts, so a per-image price can never price a
// logged execution. The request carries no image count either.
func TestPerImagePriceIsUnpricedFromTheLog(t *testing.T) {
	h := newHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{
		Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img"})
	h.mustUsage(usageFor("u1", req, "img", tokens(10, 0, 0, 10, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != stateSettled {
		t.Fatalf("state = %q, want settled: an unpriced execution is not pending", got.State)
	}
	if cost, unpriced := h.costOf(key.ID); cost != 0 || unpriced != 1 {
		t.Fatalf("per-image cost = %d/%d, want 0 cost and 1 unpriced", cost, unpriced)
	}
	if exec := h.request(req.ID).Executions[0]; exec.CostMicroUSD != nil {
		t.Fatalf("per-image execution cost = %d, want null", *exec.CostMicroUSD)
	}
}

// TestRecordedRequestHasNoPriceFields pins the JSON contract of a request: the
// frozen pricing metadata and the manual adjustment are gone, while the request
// facts remain.
func TestRecordedRequestHasNoPriceFields(t *testing.T) {
	h, key := billingHarness(t)
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		ServiceTier: "priority"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(10, 0, 0, 10, 0)))
	raw, err := json.Marshal(h.request(req.ID))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	for _, banned := range []string{"price", "pricing_rule_id", "price_tiers", "image_count", "adjustment_micro_usd"} {
		if _, ok := decoded[banned]; ok {
			t.Fatalf("request JSON still exposes %q: %s", banned, raw)
		}
	}
	for _, want := range []string{"id", "trace_id", "key_id", "model", "state", "executions", "service_tier"} {
		if _, ok := decoded[want]; !ok {
			t.Fatalf("request JSON is missing %q: %s", want, raw)
		}
	}
	executions, ok := decoded["executions"]
	if !ok {
		t.Fatal("request JSON carries no executions")
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(executions, &list); err != nil {
		t.Fatalf("decode executions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("executions = %d, want 1", len(list))
	}
	// The derived amount is always present as a key, explicitly null-able.
	if _, ok := list[0]["cost_micro_usd"]; !ok {
		t.Fatalf("execution JSON omits cost_micro_usd: %s", executions)
	}
	// The key no longer publishes a currency ledger.
	keyRaw, err := json.Marshal(h.key(key.ID))
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	var keyDecoded map[string]json.RawMessage
	if err := json.Unmarshal(keyRaw, &keyDecoded); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	for _, banned := range []string{
		"spent_micro_usd", "quota_used_micro_usd", "daily_used_micro_usd",
		"weekly_used_micro_usd", "monthly_used_micro_usd"} {
		if _, ok := keyDecoded[banned]; ok {
			t.Fatalf("key JSON still exposes %q: %s", banned, keyRaw)
		}
	}
	spec, ok := keyDecoded["spec"]
	if !ok {
		t.Fatalf("key JSON has no spec: %s", keyRaw)
	}
	var specDecoded map[string]json.RawMessage
	if err := json.Unmarshal(spec, &specDecoded); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	for _, want := range []string{"quota_micro_usd", "daily_micro_usd", "weekly_micro_usd", "monthly_micro_usd"} {
		if _, ok := specDecoded[want]; !ok {
			t.Fatalf("key spec is missing the display budget %q: %s", want, spec)
		}
	}
}

// TestExplicitZeroPriceIsDistinctFromUnpriced keeps the two null-ish cases
// apart: an explicit all-zero rate card is a priced zero, a missing price is
// null with an unpriced count.
func TestExplicitZeroPriceIsDistinctFromUnpriced(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("free", Rates{})
	key, _ := h.mustKey(KeySpec{Enabled: true})

	free := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "free"})
	h.mustUsage(usageFor("u1", free, "free", tokens(10, 0, 0, 10, 0)))
	h.mustComplete(Completion{ID: free.ID, TraceID: free.TraceID, Outcome: OutcomeSucceeded})
	missing := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "nope"})
	h.mustUsage(usageFor("u2", missing, "nope", tokens(10, 0, 0, 10, 0)))
	h.mustComplete(Completion{ID: missing.ID, TraceID: missing.TraceID, Outcome: OutcomeSucceeded})

	sum := h.report(key.ID).Summary
	if sum.CostMicroUSD != 0 || sum.UnpricedExecutions != 1 {
		t.Fatalf("summary = %+v, want a priced zero plus exactly one unpriced execution", sum)
	}
	if sum.Executions != 2 || sum.CompleteExecutions != 2 {
		t.Fatalf("summary = %+v, want both complete executions counted", sum)
	}
	byID := map[string]*int64{}
	for _, item := range h.report(key.ID).Items {
		byID[item.Executions[0].ID] = item.Executions[0].CostMicroUSD
	}
	if c, ok := byID["u1"]; !ok || c == nil || *c != 0 {
		t.Fatalf("free execution cost = %v, want an explicit 0", c)
	}
	if c, ok := byID["u2"]; !ok || c != nil {
		t.Fatalf("unpriced execution cost = %v, want null", c)
	}
}
