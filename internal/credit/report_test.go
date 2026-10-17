package credit

import (
	"context"
	"sync"
	"testing"
	"time"
)

type dataset struct {
	keys map[string]Key
}

// buildDataset creates a mixed history: settled successes, a rejection, a
// pending request without usage, two providers and two models.
func buildDataset(t *testing.T, h *harness) dataset {
	t.Helper()
	h.mustPrice("m1", Rates{Input: million, Output: 2 * million})
	h.mustPrice("m2", Rates{Input: 3 * million, Output: 3 * million})
	a, _ := h.mustKey(KeySpec{Label: "alpha", Enabled: true})
	b, _ := h.mustKey(KeySpec{Label: "beta", Enabled: true})
	keys := map[string]Key{"a": a, "b": b}

	settle := func(id, trace string, key Key, model, provider, auth string, tk Tokens) {
		req := h.mustAdmit(AdmitInput{ID: id, TraceID: trace, KeyID: key.ID, RequestedModel: model})
		u := usageFor("u-"+id, req, model, tk)
		u.Provider = provider
		u.AuthID = auth
		h.mustUsage(u)
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	}

	settle("r1", "t1", a, "m1", "openai", "auth-1", tokens(100, 0, 0, 100, 0))
	settle("r2", "t2", a, "m2", "openai", "auth-1", tokens(50, 10, 0, 20, 0))

	// A proven rejection settles with no execution.
	rejected := h.mustAdmit(AdmitInput{ID: "r3", TraceID: "t3", KeyID: a.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: rejected.ID, TraceID: rejected.TraceID, Outcome: OutcomeRejected, StatusCode: 403})

	// A completed execution without trustworthy usage stays pending.
	pending := h.mustAdmit(AdmitInput{ID: "r4", TraceID: "t4", KeyID: a.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: pending.ID, TraceID: pending.TraceID, Outcome: OutcomeSucceeded})

	h.clock.set(testEpoch.AddDate(0, 0, 2))
	settle("r5", "t5", b, "m1", "anthropic", "auth-2", tokens(10, 0, 0, 10, 0))
	h.clock.set(testEpoch)
	return dataset{keys: keys}
}

func TestReportSummaryFiltersAndItems(t *testing.T) {
	h := newHarness(t)
	ds := buildDataset(t, h)
	from := testEpoch.AddDate(0, 0, -1)
	to := testEpoch.AddDate(0, 0, 5)

	all, err := h.store.Report(h.ctx(), Filter{From: from, To: to})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if all.Total != 5 || all.Summary.Requests != 5 {
		t.Fatalf("total = %d, summary requests = %d, want 5", all.Total, all.Summary.Requests)
	}
	if all.Summary.Executions != 3 {
		t.Fatalf("executions = %d, want 3", all.Summary.Executions)
	}
	if all.Summary.CompleteExecutions != 3 {
		t.Fatalf("known executions = %d, want 3", all.Summary.CompleteExecutions)
	}
	// r1 = 100 + 200, r2 = 150 + 60, r5 = 10 + 20 -> 540 microUSD.
	if all.Summary.CostMicroUSD != 540 {
		t.Fatalf("cost = %d, want 540", all.Summary.CostMicroUSD)
	}
	if all.Summary.Failed != 1 || all.Summary.Pending != 1 {
		t.Fatalf("failed/pending = %d/%d, want 1/1", all.Summary.Failed, all.Summary.Pending)
	}
	if all.Summary.Tokens.Total != 100+100+50+10+20+10+10 {
		t.Fatalf("tokens = %+v", all.Summary.Tokens)
	}
	if all.Summary.CacheReadRatio == nil || *all.Summary.CacheReadRatio <= 0 {
		t.Fatalf("cache ratio = %v, want a positive weighted ratio", all.Summary.CacheReadRatio)
	}
	if len(all.Items) != 5 {
		t.Fatalf("items = %d, want 5", len(all.Items))
	}
	// Requests with no execution must still be listed without an execution filter.
	states := map[string]string{}
	for _, item := range all.Items {
		states[item.ID] = item.State
	}
	if states["r3"] != stateSettled || states["r4"] != statePending {
		t.Fatalf("states = %v", states)
	}
	for _, item := range all.Items {
		if item.ID == "r5" && len(item.Executions) != 1 {
			t.Fatalf("r5 executions = %d, want 1", len(item.Executions))
		}
	}

	cases := []struct {
		name   string
		filter Filter
		want   int64
		wantID string
	}{
		{"by provider", Filter{Provider: "anthropic"}, 1, "r5"},
		{"by auth", Filter{AuthID: "auth-2"}, 1, "r5"},
		{"by model", Filter{Model: "m2"}, 1, "r2"},
		{"by provider and model", Filter{Provider: "openai", Model: "m2"}, 1, "r2"},
		{"by pending", Filter{Result: statePending}, 1, "r4"},
		{"by failed", Filter{Result: OutcomeFailed}, 1, "r3"},
		{"by rejected", Filter{Result: OutcomeRejected}, 1, "r3"},
		{"by canceled", Filter{Result: OutcomeCanceled}, 0, ""},
		{"by succeeded", Filter{Result: OutcomeSucceeded}, 3, ""},
		{"by search", Filter{Search: "r5"}, 1, "r5"},
		{"by label search", Filter{Search: "beta"}, 1, "r5"}}
	for _, tc := range cases {
		tc.filter.From, tc.filter.To = from, to
		rep, err := h.store.Report(h.ctx(), tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rep.Total != tc.want || rep.Summary.Requests != tc.want {
			t.Fatalf("%s: total = %d summary = %d, want %d", tc.name, rep.Total, rep.Summary.Requests, tc.want)
		}
		if tc.wantID != "" {
			if len(rep.Items) != 1 || rep.Items[0].ID != tc.wantID {
				t.Fatalf("%s: items = %+v", tc.name, rep.Items)
			}
		}
		// An execution filter selects only matching executions.
		if tc.filter.Provider != "" || tc.filter.AuthID != "" {
			for _, item := range rep.Items {
				if len(item.Executions) == 0 {
					t.Fatalf("%s: request %s has no matching execution", tc.name, item.ID)
				}
				for _, e := range item.Executions {
					if tc.filter.Provider != "" && e.Provider != tc.filter.Provider {
						t.Fatalf("%s: execution provider = %q", tc.name, e.Provider)
					}
					if tc.filter.AuthID != "" && e.AuthID != tc.filter.AuthID {
						t.Fatalf("%s: execution auth = %q", tc.name, e.AuthID)
					}
				}
			}
		}
	}

	// A key filter narrows the same predicate used by summary and items.
	keyFilter, err := h.store.Report(h.ctx(), Filter{From: from, To: to, KeyID: ds.keys["a"].ID})
	if err != nil {
		t.Fatalf("key filter: %v", err)
	}
	if keyFilter.Total != 4 || keyFilter.Summary.Requests != 4 {
		t.Fatalf("key filter total = %d summary = %d, want 4", keyFilter.Total, keyFilter.Summary.Requests)
	}
	if keyFilter.Summary.CostMicroUSD != 510 {
		t.Fatalf("key filter cost = %d, want 510", keyFilter.Summary.CostMicroUSD)
	}
}

func TestReportPaginationAndTimeRange(t *testing.T) {
	h := newHarness(t)
	buildDataset(t, h)
	from := testEpoch.AddDate(0, 0, -1)
	to := testEpoch.AddDate(0, 0, 5)

	page1, err := h.store.Report(h.ctx(), Filter{From: from, To: to, Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Items) != 2 || page1.Total != 5 || page1.Page != 1 || page1.PageSize != 2 {
		t.Fatalf("page 1 = %d items, total %d", len(page1.Items), page1.Total)
	}
	page3, err := h.store.Report(h.ctx(), Filter{From: from, To: to, Page: 3, PageSize: 2})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(page3.Items) != 1 {
		t.Fatalf("page 3 items = %d, want 1", len(page3.Items))
	}
	seen := map[string]bool{}
	for _, item := range append(append([]Request{}, page1.Items...), page3.Items...) {
		if seen[item.ID] {
			t.Fatalf("request %s appears on two pages", item.ID)
		}
		seen[item.ID] = true
	}
	// Items are ordered newest first.
	if page1.Items[0].StartedAt.Before(page1.Items[1].StartedAt) {
		t.Fatal("items are not ordered by start time descending")
	}
	// The time window is half open and excludes the future.
	early, err := h.store.Report(h.ctx(), Filter{From: from, To: testEpoch.Add(time.Second)})
	if err != nil {
		t.Fatalf("early window: %v", err)
	}
	if early.Total != 4 {
		t.Fatalf("early window total = %d, want 4", early.Total)
	}
	if _, err := h.store.Report(h.ctx(), Filter{From: to, To: from}); err == nil {
		t.Fatal("an inverted range must be rejected")
	}
}

func TestReportTrendBuckets(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	record := func(id string, at time.Time) {
		h.clock.set(at)
		req := h.mustAdmit(AdmitInput{ID: id, TraceID: "t-" + id, KeyID: key.ID, RequestedModel: "m1"})
		h.mustUsage(usageFor("u-"+id, req, "m1", tokens(10, 0, 0, 0, 0)))
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	}
	record("a", testEpoch)
	record("b", testEpoch.Add(30*time.Minute))
	record("c", testEpoch.AddDate(0, 0, 1))
	from := testEpoch.Add(-time.Hour)
	to := testEpoch.AddDate(0, 0, 2)

	byHour, err := h.store.Report(h.ctx(), Filter{From: from, To: to, Bucket: "hour"})
	if err != nil {
		t.Fatalf("hourly: %v", err)
	}
	if len(byHour.Trend) != 2 {
		t.Fatalf("hourly buckets = %d, want 2", len(byHour.Trend))
	}
	if byHour.Trend[0].Summary.Requests != 2 || byHour.Trend[1].Summary.Requests != 1 {
		t.Fatalf("hourly points = %+v", byHour.Trend)
	}
	if !byHour.Trend[0].Time.Equal(testEpoch.Truncate(time.Hour)) {
		t.Fatalf("bucket time = %s", byHour.Trend[0].Time)
	}
	if byHour.Trend[0].Summary.CostMicroUSD != 20 {
		t.Fatalf("hourly cost = %d, want 20", byHour.Trend[0].Summary.CostMicroUSD)
	}

	byDay, err := h.store.Report(h.ctx(), Filter{From: from, To: to, Bucket: "day"})
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if len(byDay.Trend) != 2 {
		t.Fatalf("daily buckets = %d, want 2", len(byDay.Trend))
	}
	if byDay.Trend[0].Summary.Requests != 2 || byDay.Trend[1].Summary.Requests != 1 {
		t.Fatalf("daily points = %+v", byDay.Trend)
	}
	// Trend buckets must sum to the same totals as the summary.
	var totalCases, totalCost int64
	for _, p := range byDay.Trend {
		totalCases += p.Summary.Requests
		totalCost += p.Summary.CostMicroUSD
	}
	if totalCases != byDay.Summary.Requests || totalCost != byDay.Summary.CostMicroUSD {
		t.Fatalf("trend sums %d/%d do not match summary %d/%d",
			totalCases, totalCost, byDay.Summary.Requests, byDay.Summary.CostMicroUSD)
	}
}

func TestReportDefaultsToRetentionWindow(t *testing.T) {
	h := newHarness(t)
	rep, err := h.store.Report(h.ctx(), Filter{})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	want := addMonthsClamped(testEpoch, -3)
	if !rep.RetentionFrom.Equal(want) {
		t.Fatalf("retention from = %s, want %s", rep.RetentionFrom, want)
	}
}

// TestReportSnapshotIsConsistentUnderConcurrentWrites asserts that every derived
// number of one report comes from the same snapshot. Without a single read
// transaction a concurrent writer could make the page, the total and the trend
// disagree.
func TestReportSnapshotIsConsistentUnderConcurrentWrites(t *testing.T) {
	h, key := billingHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const writes = 200
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			id := "w-" + itoa(i)
			req, err := h.store.Admit(ctx, AdmitInput{ID: id, TraceID: "t-" + id, KeyID: key.ID,
				RequestedModel: "m1"})
			if err != nil {
				return
			}
			if i%5 == 0 {
				// Every fifth write uses a model without a price, so the
				// unpriced series is exercised under concurrent writes too.
				// Its tokens still count and its state still settles.
				if err := h.store.RecordUsage(ctx, usageFor("u-"+id, req, "unpriced", tokens(1, 0, 0, 0, 0))); err != nil {
					return
				}
				if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
					Outcome: OutcomeSucceeded}); err != nil {
					return
				}
				continue
			}
			if err := h.store.RecordUsage(ctx, usageFor("u-"+id, req, "m1", tokens(1, 0, 0, 0, 0))); err != nil {
				return
			}
			if err := h.store.Complete(ctx, Completion{ID: req.ID, TraceID: req.TraceID,
				Outcome: OutcomeSucceeded}); err != nil {
				return
			}
		}
	}()

	from, to := testEpoch.AddDate(-1, 0, 0), testEpoch.AddDate(1, 0, 0)
	checks := 0
	for i := 0; i < 120; i++ {
		rep, err := h.store.Report(ctx, Filter{From: from, To: to, PageSize: 5, Bucket: "day"})
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		if rep.Summary.Requests != rep.Total {
			t.Fatalf("summary requests %d != total %d", rep.Summary.Requests, rep.Total)
		}
		var bucketSum, bucketCost, bucketUnpriced, bucketExecutions, bucketTokens int64
		for _, p := range rep.Trend {
			bucketSum += p.Summary.Requests
			bucketCost += p.Summary.CostMicroUSD
			bucketUnpriced += p.Summary.UnpricedExecutions
			bucketExecutions += p.Summary.Executions
			bucketTokens += p.Summary.Tokens.Total
		}
		if bucketSum != rep.Total {
			t.Fatalf("trend requests %d != total %d", bucketSum, rep.Total)
		}
		if bucketCost != rep.Summary.CostMicroUSD {
			t.Fatalf("trend cost %d != summary cost %d", bucketCost, rep.Summary.CostMicroUSD)
		}
		if bucketUnpriced != rep.Summary.UnpricedExecutions {
			t.Fatalf("trend unpriced %d != summary unpriced %d",
				bucketUnpriced, rep.Summary.UnpricedExecutions)
		}
		if bucketExecutions != rep.Summary.Executions || bucketTokens != rep.Summary.Tokens.Total {
			t.Fatalf("trend executions/tokens %d/%d != summary %d/%d", bucketExecutions,
				bucketTokens, rep.Summary.Executions, rep.Summary.Tokens.Total)
		}
		want := rep.Total
		if want > int64(rep.PageSize) {
			want = int64(rep.PageSize)
		}
		if int64(len(rep.Items)) != want {
			t.Fatalf("page size %d != want %d for total %d", len(rep.Items), want, rep.Total)
		}
		checks++
	}
	wg.Wait()
	if checks == 0 {
		t.Fatal("no report was verified")
	}
}

// TestReportPricesOnlyTrustworthyUsage verifies the core report contract: the
// summary amount is derived from the current price book and the trustworthy
// executions only, a pending request contributes nothing, and an execution of
// an unlisted model is counted as unpriced instead of being denied or billed
// at a fabricated rate.
func TestReportPricesOnlyTrustworthyUsage(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million}) // 1 token costs 1 microUSD
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()
	from := testEpoch.AddDate(0, 0, -1)
	to := testEpoch.AddDate(0, 0, 4)

	// A measured execution: 100 microUSD of real, usage derived cost.
	measured := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1"})
	measuredUsage := usageFor("u1", measured, "m1", tokens(100, 0, 0, 0, 0))
	measuredUsage.Provider = "openai"
	h.mustUsage(measuredUsage)
	h.mustComplete(Completion{ID: measured.ID, TraceID: measured.TraceID, Outcome: OutcomeSucceeded})

	// A failed request with no trustworthy usage stays pending and unpriced.
	manual := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
		RequestedModel: "m1"})
	h.mustComplete(Completion{ID: manual.ID, TraceID: manual.TraceID, Outcome: OutcomeFailed, StatusCode: 502})

	// An execution of an unlisted model is still accepted and counted, but it
	// has no rate to derive an amount from.
	unpriced := h.mustAdmit(AdmitInput{ID: "r3", TraceID: "t3", KeyID: key.ID,
		RequestedModel: "m2"})
	unpricedUsage := usageFor("u3", unpriced, "m2", tokens(5, 0, 0, 0, 0))
	unpricedUsage.Provider = "anthropic"
	h.mustUsage(unpricedUsage)
	h.mustComplete(Completion{ID: unpriced.ID, TraceID: unpriced.TraceID, Outcome: OutcomeSucceeded})

	// A third measured request keeps the later day non-empty in the trend.
	h.clock.set(testEpoch.AddDate(0, 0, 2).Add(2 * time.Hour))
	later := h.mustAdmit(AdmitInput{ID: "r4", TraceID: "t4", KeyID: key.ID,
		RequestedModel: "m1"})
	laterUsage := usageFor("u4", later, "m1", tokens(10, 0, 0, 0, 0))
	laterUsage.Provider = "openai"
	h.mustUsage(laterUsage)
	h.mustComplete(Completion{ID: later.ID, TraceID: later.TraceID, Outcome: OutcomeSucceeded})

	rep, err := h.store.Report(ctx, Filter{From: from, To: to, Bucket: "day"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Summary.CostMicroUSD != 110 || rep.Summary.UnpricedExecutions != 1 {
		t.Fatalf("summary cost = %d/%d, want 110 with one unpriced execution",
			rep.Summary.CostMicroUSD, rep.Summary.UnpricedExecutions)
	}
	if rep.Summary.Executions != 3 || rep.Summary.CompleteExecutions != 3 {
		t.Fatalf("executions = %+v, want three trustworthy executions", rep.Summary)
	}
	if rep.Summary.Tokens.Total != 115 {
		t.Fatalf("summary tokens = %d, want every execution counted", rep.Summary.Tokens.Total)
	}
	if rep.Summary.Pending != 1 {
		t.Fatalf("pending = %d, want the unmeasured failed request", rep.Summary.Pending)
	}

	// Items carry the query-time execution cost, including explicit null for
	// the unlisted model.
	items := map[string]Request{}
	for _, item := range rep.Items {
		items[item.ID] = item
	}
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4", len(items))
	}
	if c := items["r1"].Executions[0].CostMicroUSD; c == nil || *c != 100 {
		t.Fatalf("r1 cost = %v, want 100", c)
	}
	if len(items["r2"].Executions) != 0 {
		t.Fatalf("r2 executions = %+v, want none", items["r2"].Executions)
	}
	if c := items["r3"].Executions[0].CostMicroUSD; c != nil {
		t.Fatalf("r3 cost = %v, want null", c)
	}
	if items["r2"].State != statePending || items["r3"].State != stateSettled {
		t.Fatalf("states r2/r3 = %s/%s, want pending/settled", items["r2"].State, items["r3"].State)
	}

	// The trend sums equal the summary: no amount is lost between the page,
	// the buckets and the totals.
	var trendCost, trendUnpriced int64
	for _, p := range rep.Trend {
		trendCost += p.Summary.CostMicroUSD
		trendUnpriced += p.Summary.UnpricedExecutions
	}
	if trendCost != rep.Summary.CostMicroUSD || trendUnpriced != rep.Summary.UnpricedExecutions {
		t.Fatalf("trend sums %d/%d do not match summary %d/%d", trendCost, trendUnpriced,
			rep.Summary.CostMicroUSD, rep.Summary.UnpricedExecutions)
	}
	if len(rep.Trend) != 2 {
		t.Fatalf("trend points = %d, want 2", len(rep.Trend))
	}
	if rep.Trend[0].Summary.CostMicroUSD != 100 || rep.Trend[1].Summary.CostMicroUSD != 10 {
		t.Fatalf("trend costs = %d/%d, want 100/10", rep.Trend[0].Summary.CostMicroUSD,
			rep.Trend[1].Summary.CostMicroUSD)
	}
	if rep.Trend[0].Summary.UnpricedExecutions != 1 || rep.Trend[1].Summary.UnpricedExecutions != 0 {
		t.Fatalf("trend unpriced = %d/%d, want 1/0", rep.Trend[0].Summary.UnpricedExecutions,
			rep.Trend[1].Summary.UnpricedExecutions)
	}

	// A price change reprices every retained execution on the next read; no
	// log row is rewritten and the token counters are untouched.
	h.mustPrice("m1", Rates{Input: 2 * million})
	repriced, err := h.store.Report(ctx, Filter{From: from, To: to, Bucket: "day"})
	if err != nil {
		t.Fatalf("repriced report: %v", err)
	}
	if repriced.Summary.CostMicroUSD != 220 || repriced.Summary.UnpricedExecutions != 1 {
		t.Fatalf("repriced summary = %+v, want 220 and one unpriced", repriced.Summary)
	}
	if repriced.Summary.Tokens.Total != rep.Summary.Tokens.Total {
		t.Fatalf("repricing changed tokens: %d -> %d", rep.Summary.Tokens.Total, repriced.Summary.Tokens.Total)
	}

	// Execution filters retain exactly the matching requests; an unpriced
	// execution is still a matching execution and stays visible as unpriced.
	byProvider, err := h.store.Report(ctx, Filter{From: from, To: to, Provider: "openai"})
	if err != nil {
		t.Fatalf("provider report: %v", err)
	}
	if byProvider.Total != 2 || byProvider.Summary.CostMicroUSD != 220 ||
		byProvider.Summary.UnpricedExecutions != 0 {
		t.Fatalf("openai summary = %+v, want the two measured requests", byProvider.Summary)
	}
	byOther, err := h.store.Report(ctx, Filter{From: from, To: to, Provider: "anthropic"})
	if err != nil {
		t.Fatalf("other provider report: %v", err)
	}
	if byOther.Total != 1 || byOther.Summary.CostMicroUSD != 0 || byOther.Summary.UnpricedExecutions != 1 {
		t.Fatalf("unmatched provider summary = %+v, want the unpriced execution only", byOther.Summary)
	}
	// The same holds for a key filter that excludes the measured requests.
	other, _ := h.mustKey(KeySpec{Enabled: true})
	byKey, err := h.store.Report(ctx, Filter{From: from, To: to, KeyID: other.ID})
	if err != nil {
		t.Fatalf("other key report: %v", err)
	}
	if byKey.Total != 0 || byKey.Summary.CostMicroUSD != 0 || byKey.Summary.UnpricedExecutions != 0 {
		t.Fatalf("unrelated key summary = %+v, want empty", byKey.Summary)
	}
}

// TestCacheReadRatioUsesOnlyCompleteExecutions keeps the documented cache read
// ratio honest: the canonical token totals still sum every execution, but the
// ratio only measures executions whose accounting is complete, so a partial
// execution cannot pollute it.
func TestCacheReadRatioUsesOnlyCompleteExecutions(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million, CacheRead: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()
	from := testEpoch.AddDate(0, 0, -1)
	to := testEpoch.AddDate(0, 0, 2)

	trusted := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", trusted, "m1", tokens(100, 100, 0, 0, 0))) // quality complete
	h.mustComplete(Completion{ID: trusted.ID, TraceID: trusted.TraceID, Outcome: OutcomeSucceeded})

	sloppy := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: key.ID,
		RequestedModel: "m1"})
	partial := usageFor("u2", sloppy, "m1", tokens(100, 900, 0, 0, 0))
	partial.Quality = QualityPartial
	h.mustUsage(partial)

	rep, err := h.store.Report(ctx, Filter{From: from, To: to, Bucket: "day"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	// Canonical totals keep counting every execution: 200 input, 1000 cache read.
	if rep.Summary.Tokens.Total != 1200 || rep.Summary.Tokens.Input != 200 || rep.Summary.Tokens.CacheRead != 1000 {
		t.Fatalf("summary tokens = %+v, want every execution counted", rep.Summary.Tokens)
	}
	if rep.Summary.Executions != 2 {
		t.Fatalf("executions = %d, want 2", rep.Summary.Executions)
	}
	if rep.Summary.CacheReadRatio == nil {
		t.Fatal("ratio = nil, want the complete execution ratio")
	}
	// Only the complete execution is measured: 100 / (100 input + 100 cache read).
	if got := *rep.Summary.CacheReadRatio; got != 0.5 {
		t.Fatalf("ratio = %v, want 0.5", got)
	}
	if len(rep.Trend) != 1 {
		t.Fatalf("trend points = %d, want 1", len(rep.Trend))
	}
	if rep.Trend[0].Summary.CacheReadRatio == nil || *rep.Trend[0].Summary.CacheReadRatio != 0.5 {
		t.Fatalf("trend ratio = %v, want the same complete-only ratio", rep.Trend[0].Summary.CacheReadRatio)
	}

	// An execution set without a single complete execution reports no ratio at
	// all instead of dividing partial counters by themselves.
	other, _ := h.mustKey(KeySpec{Enabled: true})
	untrusted := h.mustAdmit(AdmitInput{ID: "r3", TraceID: "t3", KeyID: other.ID,
		RequestedModel: "m1"})
	unknown := usageFor("u3", untrusted, "m1", tokens(100, 900, 0, 0, 0))
	unknown.Quality = QualityPartial
	h.mustUsage(unknown)
	onlyPartial, err := h.store.Report(ctx, Filter{From: from, To: to, KeyID: other.ID})
	if err != nil {
		t.Fatalf("report other key: %v", err)
	}
	if onlyPartial.Summary.CacheReadRatio != nil {
		t.Fatalf("ratio = %v, want none without complete executions", *onlyPartial.Summary.CacheReadRatio)
	}
	if onlyPartial.Summary.Tokens.Total != 1000 {
		t.Fatalf("partial tokens = %d, want them still reported", onlyPartial.Summary.Tokens.Total)
	}
}

// TestReportRejectsAggregateOverflow pins the accepted behaviour for sums that
// no int64 can hold: the aggregate is rejected with an error instead of being
// returned as a silently truncated or floating point total.
func TestReportRejectsAggregateOverflow(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("big", Rates{Input: 1_000_000_000_000})
	from := testEpoch.AddDate(0, 0, -1)
	to := testEpoch.AddDate(0, 0, 1)
	const tokensNearMax = 9_000_000_000_000 // * rate / 1M = 9e18 microUSD

	keys := make([]Key, 0, 2)
	for i := 0; i < 2; i++ {
		key, _ := h.mustKey(KeySpec{Label: "big-" + itoa(i), Enabled: true})
		keys = append(keys, key)
		req := h.mustAdmit(AdmitInput{ID: "r" + itoa(i), TraceID: "t" + itoa(i), KeyID: key.ID,
			RequestedModel: "big"})
		h.mustUsage(usageFor("u"+itoa(i), req, "big", tokens(tokensNearMax, 0, 0, 0, 0)))
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	}
	// Each key alone is representable and stays exact.
	single, err := h.store.Report(h.ctx(), Filter{From: from, To: to, KeyID: keys[0].ID})
	if err != nil {
		t.Fatalf("single key report: %v", err)
	}
	if single.Summary.CostMicroUSD != 9_000_000_000_000_000_000 {
		t.Fatalf("single key cost = %d, want the exact near limit value", single.Summary.CostMicroUSD)
	}
	// The unrepresentable sum of both keys is refused rather than truncated.
	if _, err := h.store.Report(h.ctx(), Filter{From: from, To: to}); err == nil {
		t.Fatal("cross key report: want an explicit overflow error, got a summary")
	}
}
