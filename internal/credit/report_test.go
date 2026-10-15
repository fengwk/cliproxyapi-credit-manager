package credit

import (
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
		req := h.mustReserve(ReserveInput{ID: id, TraceID: trace, KeyID: key.ID, RequestedModel: model,
			InputEstimate: tk.Input, OutputEstimate: tk.Output})
		u := usageFor("u-"+id, req, model, tk)
		u.Provider = provider
		u.AuthID = auth
		h.mustUsage(u)
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	}

	settle("r1", "t1", a, "m1", "openai", "auth-1", tokens(100, 0, 0, 100, 0))
	settle("r2", "t2", a, "m2", "openai", "auth-1", tokens(50, 10, 0, 20, 0))

	// A proven rejection settles with no execution.
	rejected := h.mustReserve(ReserveInput{ID: "r3", TraceID: "t3", KeyID: a.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: rejected.ID, TraceID: rejected.TraceID, Outcome: OutcomeFailed, StatusCode: 403})

	// A completed execution without trustworthy usage stays pending.
	pending := h.mustReserve(ReserveInput{ID: "r4", TraceID: "t4", KeyID: a.ID, RequestedModel: "m1"})
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
		{"by succeeded", Filter{Result: OutcomeSucceeded}, 3, ""},
		{"by search", Filter{Search: "r5"}, 1, "r5"},
		{"by label search", Filter{Search: "beta"}, 1, "r5"},
	}
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
		req := h.mustReserve(ReserveInput{ID: id, TraceID: "t-" + id, KeyID: key.ID, RequestedModel: "m1",
			InputEstimate: 10})
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
