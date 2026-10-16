package credit

import (
	"testing"
	"time"
)

// TestReportTrendUsesCalendarMonths pins the month bucket to a true UTC
// calendar month rather than a fixed 30-day window: buckets follow unequal month
// lengths including a leap February, a month boundary is decided in UTC, and the
// trend still reconciles with the summary. A request at 00:00 UTC on the first
// of a month belongs to the new month, while the same wall-clock instant in a
// positive UTC offset still belongs to the previous UTC month.
func TestReportTrendUsesCalendarMonths(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million}) // 1 input token = 1 microUSD
	key, _ := h.mustKey(KeySpec{Enabled: true})

	east := time.FixedZone("UTC+9", 9*60*60)
	// 2024 is a leap year, so February has 29 days and the three buckets below
	// have three different lengths (31, 29, 31 days).
	samples := []struct {
		id string
		at time.Time
		in int64
	}{
		{"jan-first", time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC), 1},
		{"jan-last", time.Date(2024, time.January, 31, 23, 59, 59, 0, time.UTC), 2},
		{"feb-leap-day", time.Date(2024, time.February, 29, 23, 59, 59, 0, time.UTC), 4},
		// 2024-03-01T00:30:00+09:00 is 2024-02-29T15:30:00Z: the UTC month, not
		// the local one, decides the bucket.
		{"feb-via-offset", time.Date(2024, time.March, 1, 0, 30, 0, 0, east), 8},
		{"mar-first", time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC), 16},
	}
	for _, s := range samples {
		h.clock.set(s.at)
		req := h.mustReserve(ReserveInput{ID: s.id, TraceID: "t-" + s.id, KeyID: key.ID,
			RequestedModel: "m1", InputEstimate: s.in, At: s.at})
		h.mustUsage(usageFor("u-"+s.id, req, "m1", tokens(s.in, 0, 0, 0, 0)))
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded, At: s.at})
	}

	from := time.Date(2023, time.December, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, time.April, 1, 0, 0, 0, 0, time.UTC)
	rep, err := h.store.Report(h.ctx(), Filter{From: from, To: to, Bucket: "month"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Trend) != 3 {
		t.Fatalf("month buckets = %d, want 3 (Jan, Feb, Mar)", len(rep.Trend))
	}
	wantStarts := []time.Time{
		time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC),
	}
	for i, want := range wantStarts {
		got := rep.Trend[i].Time
		if !got.Equal(want) {
			t.Fatalf("bucket %d time = %s, want %s", i, got, want)
		}
		if got.Location() != time.UTC {
			t.Fatalf("bucket %d time zone = %s, want UTC", i, got.Location())
		}
	}
	// Each request lands in its own UTC calendar month.
	wantRequests := []int64{2, 2, 1}
	wantCosts := []int64{3, 12, 16}
	for i := range wantStarts {
		if got := rep.Trend[i].Summary.Requests; got != wantRequests[i] {
			t.Fatalf("bucket %d requests = %d, want %d", i, got, wantRequests[i])
		}
		if got := rep.Trend[i].Summary.CostMicroUSD; got != wantCosts[i] {
			t.Fatalf("bucket %d cost = %d, want %d", i, got, wantCosts[i])
		}
	}
	// Unequal gaps prove a calendar month: a 30-day window could not produce a
	// 31 day January followed by a 29 day February.
	if gap := rep.Trend[1].Time.Sub(rep.Trend[0].Time); gap != 31*24*time.Hour {
		t.Fatalf("Jan -> Feb gap = %s, want 31 days", gap)
	}
	if gap := rep.Trend[2].Time.Sub(rep.Trend[1].Time); gap != 29*24*time.Hour {
		t.Fatalf("Feb -> Mar gap = %s, want the 29 day leap February", gap)
	}
	// Money and tokens reconcile across buckets and with the summary.
	var cost, tokensTotal int64
	for _, p := range rep.Trend {
		cost += p.Summary.CostMicroUSD
		tokensTotal += p.Summary.Tokens.Total
	}
	if cost != rep.Summary.CostMicroUSD {
		t.Fatalf("bucket cost %d != summary cost %d", cost, rep.Summary.CostMicroUSD)
	}
	if tokensTotal != rep.Summary.Tokens.Total {
		t.Fatalf("bucket tokens %d != summary tokens %d", tokensTotal, rep.Summary.Tokens.Total)
	}
}

// TestReportTrendMonthReconcilesSummaryWithAdjustments verifies the month bucket
// preserves the existing report semantics: every request appears in exactly one
// bucket (including unresolved and execution-less ones), manual adjustments stay
// attributed to the request's own month even when booked later, and the trend
// totals reconcile with the summary and the key ledger.
func TestReportTrendMonthReconcilesSummaryWithAdjustments(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million, Output: 2 * million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()

	record := func(id string, at time.Time, in, out int64) {
		h.clock.set(at)
		req := h.mustReserve(ReserveInput{ID: id, TraceID: "t-" + id, KeyID: key.ID,
			RequestedModel: "m1", InputEstimate: in, OutputEstimate: out, At: at})
		h.mustUsage(usageFor("u-"+id, req, "m1", tokens(in, 0, 0, out, 0)))
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded, At: at})
	}
	// Costs: a = 10 + 5*2 = 20, b = 30 + 10*2 = 50, c = 7 + 1*2 = 9.
	record("a", time.Date(2024, time.January, 10, 8, 0, 0, 0, time.UTC), 10, 5)
	record("b", time.Date(2024, time.February, 20, 8, 0, 0, 0, time.UTC), 30, 10)
	record("c", time.Date(2024, time.March, 5, 8, 0, 0, 0, time.UTC), 7, 1)

	// A failed request with no trustworthy usage stays pending, then an
	// administrator charges it in a later month.
	feb25 := time.Date(2024, time.February, 25, 8, 0, 0, 0, time.UTC)
	h.clock.set(feb25)
	manual := h.mustReserve(ReserveInput{ID: "manual", TraceID: "t-manual", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 100, At: feb25})
	h.mustComplete(Completion{ID: manual.ID, TraceID: manual.TraceID, Outcome: OutcomeFailed, StatusCode: 502, At: feb25})
	h.clock.set(time.Date(2024, time.March, 20, 8, 0, 0, 0, time.UTC))
	if err := h.store.Resolve(ctx, "manual", Resolution{Action: ActionCharge,
		AmountMicroUSD: 500, Reason: "provider billed out of band"}); err != nil {
		t.Fatalf("resolve charge: %v", err)
	}

	// An unresolved request started in March must stay visible, never dropped.
	h.clock.set(time.Date(2024, time.March, 28, 8, 0, 0, 0, time.UTC))
	h.mustReserve(ReserveInput{ID: "pending", TraceID: "t-pending", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 3, At: time.Date(2024, time.March, 28, 8, 0, 0, 0, time.UTC)})
	// A request with no execution at all keeps the January bucket honest.
	h.clock.set(time.Date(2024, time.January, 20, 8, 0, 0, 0, time.UTC))
	h.mustReserve(ReserveInput{ID: "noexec", TraceID: "t-noexec", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 1, At: time.Date(2024, time.January, 20, 8, 0, 0, 0, time.UTC)})

	from := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, time.April, 1, 0, 0, 0, 0, time.UTC)
	rep, err := h.store.Report(ctx, Filter{From: from, To: to, Bucket: "month"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Trend) != 3 {
		t.Fatalf("month buckets = %d, want 3", len(rep.Trend))
	}
	if rep.Total != 6 || rep.Summary.Requests != 6 {
		t.Fatalf("total = %d, summary requests = %d, want 6", rep.Total, rep.Summary.Requests)
	}

	var requests, cost, adjustments, tokensTotal int64
	for _, p := range rep.Trend {
		requests += p.Summary.Requests
		cost += p.Summary.CostMicroUSD
		adjustments += p.Summary.AdjustmentMicroUSD
		tokensTotal += p.Summary.Tokens.Total
	}
	// Nothing is dropped: every request sits in exactly one month bucket.
	if requests != rep.Total || requests != rep.Summary.Requests {
		t.Fatalf("bucket requests %d != total %d / summary %d", requests, rep.Total, rep.Summary.Requests)
	}
	if cost != rep.Summary.CostMicroUSD {
		t.Fatalf("bucket cost %d != summary cost %d", cost, rep.Summary.CostMicroUSD)
	}
	if adjustments != rep.Summary.AdjustmentMicroUSD {
		t.Fatalf("bucket adjustments %d != summary adjustments %d", adjustments, rep.Summary.AdjustmentMicroUSD)
	}
	if tokensTotal != rep.Summary.Tokens.Total {
		t.Fatalf("bucket tokens %d != summary tokens %d", tokensTotal, rep.Summary.Tokens.Total)
	}

	// Per-month attribution: two requests each month, with the 500 microUSD
	// charge in February (the request's month), not March (when it was booked).
	wantRequests := []int64{2, 2, 2}
	wantCost := []int64{20, 50, 9}
	wantAdjust := []int64{0, 500, 0}
	for i := range wantRequests {
		if got := rep.Trend[i].Summary.Requests; got != wantRequests[i] {
			t.Fatalf("bucket %d requests = %d, want %d", i, got, wantRequests[i])
		}
		if got := rep.Trend[i].Summary.CostMicroUSD; got != wantCost[i] {
			t.Fatalf("bucket %d cost = %d, want %d", i, got, wantCost[i])
		}
		if got := rep.Trend[i].Summary.AdjustmentMicroUSD; got != wantAdjust[i] {
			t.Fatalf("bucket %d adjustments = %d, want %d", i, got, wantAdjust[i])
		}
	}

	// The unresolved request is reported as pending, not silently dropped.
	if rep.Summary.Pending == 0 {
		t.Fatal("summary reports no pending request although two are unresolved")
	}
	// Cost plus adjustments reconstitutes the ledger spend exactly.
	ledger := h.key(key.ID)
	if ledger.SpentMicroUSD != rep.Summary.CostMicroUSD+rep.Summary.AdjustmentMicroUSD {
		t.Fatalf("ledger %d != cost %d + adjustments %d", ledger.SpentMicroUSD,
			rep.Summary.CostMicroUSD, rep.Summary.AdjustmentMicroUSD)
	}
}

// TestReportTrendBucketNamesAndFallback verifies the bucket name handling stays
// unchanged around the new month value: hour and day keep their fixed UTC
// buckets, month is matched case-insensitively with surrounding space trimmed,
// and any unknown value still falls back to the day bucket.
func TestReportTrendBucketNamesAndFallback(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	for i, at := range []time.Time{
		time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, time.February, 29, 12, 0, 0, 0, time.UTC),
		time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC),
	} {
		id := "r" + itoa(i)
		h.clock.set(at)
		req := h.mustReserve(ReserveInput{ID: id, TraceID: "t-" + id, KeyID: key.ID,
			RequestedModel: "m1", InputEstimate: 1, At: at})
		h.mustUsage(usageFor("u-"+id, req, "m1", tokens(1, 0, 0, 0, 0)))
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded, At: at})
	}
	from := time.Date(2023, time.December, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, time.April, 1, 0, 0, 0, 0, time.UTC)

	points := func(bucket string) []Point {
		rep, err := h.store.Report(h.ctx(), Filter{From: from, To: to, Bucket: bucket})
		if err != nil {
			t.Fatalf("bucket %q: %v", bucket, err)
		}
		return rep.Trend
	}
	// hour and day are unchanged fixed buckets.
	if got := len(points("hour")); got != 3 {
		t.Fatalf("hour buckets = %d, want 3", got)
	}
	if got := points("day"); len(got) != 3 || !got[0].Time.Equal(time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day buckets = %+v", got)
	}
	// month is recognized regardless of case or surrounding space.
	want := points("month")
	if len(want) != 2 {
		t.Fatalf("month buckets = %d, want 2", len(want))
	}
	for _, alias := range []string{"MONTH", " Month "} {
		got := points(alias)
		if len(got) != len(want) {
			t.Fatalf("bucket %q buckets = %d, want %d", alias, len(got), len(want))
		}
		for i := range want {
			if !got[i].Time.Equal(want[i].Time) {
				t.Fatalf("bucket %q point %d = %s, want %s", alias, i, got[i].Time, want[i].Time)
			}
		}
	}
	// An unknown bucket name keeps the day fallback.
	unknown := points("week")
	day := points("day")
	if len(unknown) != len(day) {
		t.Fatalf("unknown bucket points = %d, want the day fallback of %d", len(unknown), len(day))
	}
	for i := range day {
		if !unknown[i].Time.Equal(day[i].Time) {
			t.Fatalf("unknown bucket point %d = %s, want day %s", i, unknown[i].Time, day[i].Time)
		}
	}
}
