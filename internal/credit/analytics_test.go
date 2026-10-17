package credit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// addSummary folds s into total for every additive counter shared by the
// summary, the trend points and the per-model rows. Ratios are excluded because
// a cache ratio is recomputed per slice, not summed.
func addSummary(total *Summary, s Summary) {
	total.Requests += s.Requests
	total.Executions += s.Executions
	total.Failed += s.Failed
	total.Pending += s.Pending
	total.CostMicroUSD += s.CostMicroUSD
	total.UnpricedExecutions += s.UnpricedExecutions
	total.CompleteExecutions += s.CompleteExecutions
	total.Tokens.Input += s.Tokens.Input
	total.Tokens.CacheRead += s.Tokens.CacheRead
	total.Tokens.CacheWrite += s.Tokens.CacheWrite
	total.Tokens.Output += s.Tokens.Output
	total.Tokens.Reasoning += s.Tokens.Reasoning
	total.Tokens.Total += s.Tokens.Total
}

func sumModels(models []ModelMetrics) Summary {
	var total Summary
	for _, m := range models {
		addSummary(&total, m.Summary)
	}
	return total
}

func sumTrend(trend []Point) Summary {
	var total Summary
	for _, p := range trend {
		addSummary(&total, p.Summary)
	}
	return total
}

// assertAdditive checks the two conservation invariants the dashboard relies
// on: the per-model rows and the trend points both rebuild the summary exactly.
func assertAdditive(t *testing.T, label string, overview Analytics) {
	t.Helper()
	if got := sumModels(overview.Models); !sameAdditive(got, overview.Summary) {
		t.Fatalf("%s: sum(models)=%+v != summary=%+v", label, got, overview.Summary)
	}
	if got := sumTrend(overview.Trend); !sameAdditive(got, overview.Summary) {
		t.Fatalf("%s: sum(trend)=%+v != summary=%+v", label, got, overview.Summary)
	}
}

func sameAdditive(a, b Summary) bool {
	return a.Requests == b.Requests &&
		a.Executions == b.Executions &&
		a.Failed == b.Failed &&
		a.Pending == b.Pending &&
		a.CostMicroUSD == b.CostMicroUSD &&
		a.UnpricedExecutions == b.UnpricedExecutions &&
		a.CompleteExecutions == b.CompleteExecutions &&
		a.Tokens == b.Tokens
}

func findModel(models []ModelMetrics, name string) *ModelMetrics {
	for i := range models {
		if models[i].Model == name {
			return &models[i]
		}
	}
	return nil
}

// buildOverviewDataset is a mixed history that exercises every grouping rule:
// a request whose executions carry a provider alias (priced by its own actual
// model, hence unpriced here), a multi-attempt request, a rejection without
// execution, and two unresolved requests with no trustworthy usage.
func buildOverviewDataset(t *testing.T, h *harness) (k1, k2 Key) {
	t.Helper()
	h.mustPrice("m1", Rates{Input: million, Output: 2 * million})
	h.mustPrice("m2", Rates{Input: 3 * million, Output: 3 * million})
	k1, _ = h.mustKey(KeySpec{Label: "alpha", Enabled: true})
	k2, _ = h.mustKey(KeySpec{Label: "beta", Enabled: true})

	// r1: one client model, two executions, one under a provider alias.
	h.clock.set(testEpoch)
	r1 := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: k1.ID, RequestedModel: "m1"})
	alias := usageFor("u1a", r1, "m1-fallback-alias", tokens(10, 0, 0, 5, 0))
	alias.LatencyMS, alias.TTFTMS = 1000, 100
	h.mustUsage(alias)
	retry := usageFor("u1b", r1, "m1", tokens(20, 0, 0, 10, 0))
	retry.LatencyMS = 500
	h.mustUsage(retry)
	h.mustComplete(Completion{ID: r1.ID, TraceID: r1.TraceID, Outcome: OutcomeSucceeded})

	// r2: a second model on another provider, with cache read and reasoning.
	h.clock.set(testEpoch.Add(time.Hour))
	r2 := h.mustAdmit(AdmitInput{ID: "r2", TraceID: "t2", KeyID: k2.ID, RequestedModel: "m2"})
	second := usageFor("u2", r2, "m2", tokens(50, 10, 0, 20, 5))
	second.Provider, second.AuthID = "anthropic", "auth-2"
	h.mustUsage(second)
	h.mustComplete(Completion{ID: r2.ID, TraceID: r2.TraceID, Outcome: OutcomeSucceeded})

	// r3: a proven rejection that never executed.
	h.clock.set(testEpoch.Add(2 * time.Hour))
	r3 := h.mustAdmit(AdmitInput{ID: "r3", TraceID: "t3", KeyID: k1.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: r3.ID, TraceID: r3.TraceID, Outcome: OutcomeRejected, StatusCode: 403})

	// r4: a completed lifecycle without trustworthy usage stays pending.
	h.clock.set(testEpoch.Add(3 * time.Hour))
	r4 := h.mustAdmit(AdmitInput{ID: "r4", TraceID: "t4", KeyID: k1.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: r4.ID, TraceID: r4.TraceID, Outcome: OutcomeSucceeded})

	// r5: a failed lifecycle whose usage never arrived stays pending. There is
	// no manual amount to book: only recorded usage can be priced.
	h.clock.set(testEpoch.Add(4 * time.Hour))
	r5 := h.mustAdmit(AdmitInput{ID: "r5", TraceID: "t5", KeyID: k1.ID, RequestedModel: "m1"})
	h.mustComplete(Completion{ID: r5.ID, TraceID: r5.TraceID, Outcome: OutcomeFailed, StatusCode: 502})
	h.clock.set(testEpoch)
	return k1, k2
}

// TestAnalyticsSummaryTrendModelsConserve pins the core contract: models are
// grouped by the client requested model (never an execution alias), the
// per-model rows and the trend rebuild the summary exactly, group cost is the
// sum of each execution priced by its own actual model, and the overview is
// independent of the requested page.
func TestAnalyticsSummaryTrendModelsConserve(t *testing.T) {
	h := newHarness(t)
	k1, _ := buildOverviewDataset(t, h)
	from := testEpoch.AddDate(0, 0, -1)
	to := testEpoch.AddDate(0, 0, 5)

	overview, err := h.store.Analytics(h.ctx(), Filter{From: from, To: to, Bucket: "hour"})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	assertAdditive(t, "whole dataset", overview)

	report, err := h.store.Report(h.ctx(), Filter{From: from, To: to, Bucket: "hour"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	// The overview summary is the report summary, from the same predicates.
	if !sameAdditive(overview.Summary, report.Summary) {
		t.Fatalf("overview summary=%+v != report summary=%+v", overview.Summary, report.Summary)
	}
	if overview.Summary.Requests != 5 || overview.Summary.Executions != 3 {
		t.Fatalf("summary = %+v, want 5 requests / 3 executions", overview.Summary)
	}

	if len(overview.Models) != 2 || overview.Models[0].Model != "m1" || overview.Models[1].Model != "m2" {
		t.Fatalf("models = %+v, want a stable m1/m2 order", overview.Models)
	}
	if alias := findModel(overview.Models, "m1-fallback-alias"); alias != nil {
		t.Fatal("an execution alias became its own model row")
	}

	m1 := findModel(overview.Models, "m1")
	// Two failures: the rejection r3 and the failed r5. Both r4 and r5 stay
	// pending because neither reported trustworthy usage.
	if m1.Requests != 4 || m1.Failed != 2 || m1.Pending != 2 {
		t.Fatalf("m1 counters = %+v, want 4 requests / 2 failed / 2 pending", m1.Summary)
	}
	if m1.Executions != 2 || m1.CompleteExecutions != 2 {
		t.Fatalf("m1 executions = %+v, want 2 complete executions", m1.Summary)
	}
	// The alias execution is priced by its actual model id, which has no price:
	// it is unpriced. The retry bills the requested model's rates: 20 + 10*2.
	if m1.CostMicroUSD != 40 || m1.UnpricedExecutions != 1 {
		t.Fatalf("m1 cost = %d/%d, want 40 with the alias execution unpriced", m1.CostMicroUSD, m1.UnpricedExecutions)
	}
	if want := (Tokens{Input: 30, Output: 15, Total: 45}); m1.Tokens != want {
		t.Fatalf("m1 tokens = %+v, want %+v", m1.Tokens, want)
	}
	// Only executions that reported a measurement count as samples.
	if m1.LatencyMS != 1500 || m1.LatencySamples != 2 {
		t.Fatalf("m1 latency = %d/%d, want 1500/2", m1.LatencyMS, m1.LatencySamples)
	}
	if m1.TTFTMS != 100 || m1.TTFTSamples != 1 {
		t.Fatalf("m1 ttft = %d/%d, want 100/1", m1.TTFTMS, m1.TTFTSamples)
	}
	if m1.OutputTokens != 15 {
		t.Fatalf("m1 output tokens = %d, want the generated output 15", m1.OutputTokens)
	}

	m2 := findModel(overview.Models, "m2")
	if m2.Requests != 1 || m2.Executions != 1 || m2.CostMicroUSD != 225 {
		t.Fatalf("m2 = %+v, want 1 request / 1 execution / 225 microUSD", m2.Summary)
	}
	if want := (Tokens{Input: 50, CacheRead: 10, Output: 20, Reasoning: 5, Total: 85}); m2.Tokens != want {
		t.Fatalf("m2 tokens = %+v, want %+v", m2.Tokens, want)
	}
	// Unmeasured latency is zero samples, never a fabricated mean.
	if m2.LatencyMS != 0 || m2.LatencySamples != 0 || m2.TTFTMS != 0 || m2.TTFTSamples != 0 {
		t.Fatalf("m2 latency = %d/%d ttft = %d/%d, want all zero", m2.LatencyMS, m2.LatencySamples, m2.TTFTMS, m2.TTFTSamples)
	}
	if m2.OutputTokens != 25 {
		t.Fatalf("m2 output tokens = %d, want 20 output + 5 reasoning", m2.OutputTokens)
	}

	// A key-scoped selection derives the same amounts from the same log: the
	// summary is the report summary for that key and there is no durable
	// currency ledger to reconcile against.
	keyReport, err := h.store.Analytics(h.ctx(), Filter{From: from, To: to, KeyID: k1.ID})
	if err != nil {
		t.Fatalf("key analytics: %v", err)
	}
	if keyReport.Summary.CostMicroUSD != 40 || keyReport.Summary.UnpricedExecutions != 1 {
		t.Fatalf("key summary = %+v, want 40 cost and one unpriced execution", keyReport.Summary)
	}
	keyPlain, err := h.store.Report(h.ctx(), Filter{From: from, To: to, KeyID: k1.ID})
	if err != nil {
		t.Fatalf("key report: %v", err)
	}
	if !sameAdditive(keyReport.Summary, keyPlain.Summary) {
		t.Fatalf("key analytics %+v != key report %+v", keyReport.Summary, keyPlain.Summary)
	}

	// A page number and a tiny page size must not shrink the aggregates.
	paged, err := h.store.Analytics(h.ctx(), Filter{From: from, To: to, Bucket: "hour", Page: 3, PageSize: 1})
	if err != nil {
		t.Fatalf("paged analytics: %v", err)
	}
	if !sameAdditive(paged.Summary, overview.Summary) || len(paged.Models) != len(overview.Models) {
		t.Fatalf("paged overview = %+v, want the full dataset", paged.Summary)
	}
}

// TestAnalyticsFilterParity checks that every request-level and execution-level
// selector used by the report narrows the overview identically.
func TestAnalyticsFilterParity(t *testing.T) {
	h := newHarness(t)
	buildOverviewDataset(t, h)
	from := testEpoch.AddDate(0, 0, -1)
	to := testEpoch.AddDate(0, 0, 5)

	cases := []struct {
		name      string
		filter    Filter
		requests  int64
		wantModel string
	}{
		{"by provider", Filter{Provider: "anthropic"}, 1, "m2"},
		{"by auth", Filter{AuthID: "auth-2"}, 1, "m2"},
		{"by model", Filter{Model: "m2"}, 1, "m2"},
		{"by pending", Filter{Result: statePending}, 2, "m1"},
		{"by failed", Filter{Result: OutcomeFailed}, 2, "m1"},
		{"by rejected", Filter{Result: OutcomeRejected}, 1, "m1"},
		{"by search", Filter{Search: "beta"}, 1, "m2"}}
	for _, tc := range cases {
		tc.filter.From, tc.filter.To = from, to
		overview, err := h.store.Analytics(h.ctx(), tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		report, err := h.store.Report(h.ctx(), tc.filter)
		if err != nil {
			t.Fatalf("%s: report: %v", tc.name, err)
		}
		if !sameAdditive(overview.Summary, report.Summary) {
			t.Fatalf("%s: overview summary %+v != report summary %+v", tc.name, overview.Summary, report.Summary)
		}
		if overview.Summary.Requests != tc.requests {
			t.Fatalf("%s: requests = %d, want %d", tc.name, overview.Summary.Requests, tc.requests)
		}
		if len(overview.Models) != 1 || overview.Models[0].Model != tc.wantModel {
			t.Fatalf("%s: models = %+v, want only %s", tc.name, overview.Models, tc.wantModel)
		}
		assertAdditive(t, tc.name, overview)
	}
}

// TestAnalyticsExecutionlessModelKeepsNullRatios verifies that a model known
// only through execution-less requests keeps explicit undefined values: a nil
// cache ratio and zero sample counts, so a client renders null instead of a
// fabricated 0.
func TestAnalyticsExecutionlessModelKeepsNullRatios(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	from, to := testEpoch.AddDate(0, 0, -1), testEpoch.AddDate(0, 0, 1)

	overview, err := h.store.Analytics(h.ctx(), Filter{From: from, To: to})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	if len(overview.Models) != 1 {
		t.Fatalf("models = %+v, want one row for an execution-less model", overview.Models)
	}
	m := overview.Models[0]
	if m.Requests != 1 || m.Executions != 0 || m.CompleteExecutions != 0 {
		t.Fatalf("counters = %+v, want 1 request and no execution", m.Summary)
	}
	if m.CacheReadRatio != nil {
		t.Fatalf("cache ratio = %v, want nil when nothing completed", *m.CacheReadRatio)
	}
	if m.LatencySamples != 0 || m.TTFTSamples != 0 || m.OutputTokens != 0 || m.Tokens.Total != 0 {
		t.Fatalf("metrics = %+v, want undefined latency and empty tokens", m)
	}
	assertAdditive(t, "executionless", overview)
}

// TestAnalyticsLatencySamplesAndOutputTokens pins the exact meaning of the new
// counters: samples count only positive measurements, and output tokens are the
// generated output plus reasoning.
func TestAnalyticsLatencySamplesAndOutputTokens(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million, Output: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	// Three executions: one measured, two unmeasured. The mean must be the
	// single measurement, not a diluted 3000/3.
	measured := usageFor("u1", req, "m1", tokens(1, 0, 0, 4, 6))
	measured.LatencyMS, measured.TTFTMS = 3000, 300
	h.mustUsage(measured)
	h.mustUsage(usageFor("u2", req, "m1", tokens(1, 0, 0, 2, 1)))
	h.mustUsage(usageFor("u3", req, "m1", tokens(1, 0, 0, 3, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})

	overview, err := h.store.Analytics(h.ctx(), Filter{From: testEpoch.Add(-time.Hour), To: testEpoch.Add(time.Hour)})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	m := overview.Models[0]
	if m.LatencySamples != 1 || m.LatencyMS != 3000 {
		t.Fatalf("latency = %d/%d, want 3000 over a single sample", m.LatencyMS, m.LatencySamples)
	}
	if m.TTFTSamples != 1 || m.TTFTMS != 300 {
		t.Fatalf("ttft = %d/%d, want 300 over a single sample", m.TTFTMS, m.TTFTSamples)
	}
	// Generated tokens are output + reasoning of all three executions.
	if m.OutputTokens != 4+6+2+1+3+0 {
		t.Fatalf("output tokens = %d, want output+reasoning of every execution", m.OutputTokens)
	}
	// A zero-token selection would leave unit cost undefined; here it is defined
	// from integer counts only.
	if m.Tokens.Total == 0 || m.CostMicroUSD == 0 {
		t.Fatalf("counters = %+v, want positive tokens and cost", m.Summary)
	}
}

// TestAnalyticsRandomizedInvariants drives many filters over a random history
// and asserts the invariants that make the overview trustworthy: the summary is
// the report summary, the paged request set never limits the aggregates, and
// the model rows and trend always rebuild the summary.
func TestAnalyticsRandomizedInvariants(t *testing.T) {
	h := newHarness(t)
	models := []string{"m1", "m2", "m3"}
	for _, model := range models {
		h.mustPrice(model, Rates{Input: million, Output: 2 * million})
	}
	keys := make([]Key, 0, 2)
	for _, label := range []string{"alpha", "beta"} {
		k, _ := h.mustKey(KeySpec{Label: label, Enabled: true})
		keys = append(keys, k)
	}
	providers := []string{"openai", "anthropic"}
	rng := rand.New(rand.NewSource(20261016))
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("r%02d", i)
		model := models[rng.Intn(len(models))]
		key := keys[rng.Intn(len(keys))]
		at := testEpoch.Add(time.Duration(rng.Intn(72)) * time.Hour)
		h.clock.set(at)
		req := h.mustAdmit(AdmitInput{ID: id, TraceID: "t-" + id, KeyID: key.ID, RequestedModel: model, At: at})
		for j := 0; j < rng.Intn(3); j++ {
			alias := model
			if rng.Intn(3) == 0 {
				alias = model + "-alias"
			}
			u := usageFor(fmt.Sprintf("u-%s-%d", id, j), req, alias,
				tokens(int64(rng.Intn(20)), 0, 0, int64(rng.Intn(20)), int64(rng.Intn(5))))
			u.Provider = providers[rng.Intn(len(providers))]
			u.AuthID = fmt.Sprintf("auth-%d", rng.Intn(2)+1)
			u.LatencyMS = int64(rng.Intn(3)) * 250
			u.TTFTMS = int64(rng.Intn(3)) * 50
			if rng.Intn(4) == 0 {
				u.Quality = QualityPartial
			}
			h.mustUsage(u)
		}
		outcomes := []string{OutcomeSucceeded, OutcomeFailed, OutcomeRejected, OutcomeCanceled}
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: outcomes[rng.Intn(len(outcomes))], StatusCode: 200 + rng.Intn(5)})
	}
	h.clock.set(testEpoch)

	from, to := testEpoch.Add(-time.Hour), testEpoch.Add(100*time.Hour)
	resultOptions := []string{OutcomeSucceeded, OutcomeFailed, statePending, OutcomeRejected, OutcomeCanceled}
	buckets := []string{"", "hour", "day", "month"}
	requested := map[string]bool{}
	for _, m := range models {
		requested[m] = true
	}
	for iter := 0; iter < 30; iter++ {
		filter := Filter{From: from, To: to, Bucket: buckets[rng.Intn(len(buckets))]}
		if rng.Intn(2) == 0 {
			filter.KeyID = keys[rng.Intn(len(keys))].ID
		}
		if rng.Intn(3) == 0 {
			filter.Model = models[rng.Intn(len(models))]
		}
		if rng.Intn(3) == 0 {
			filter.Provider = providers[rng.Intn(len(providers))]
		}
		if rng.Intn(4) == 0 {
			filter.AuthID = fmt.Sprintf("auth-%d", rng.Intn(2)+1)
		}
		if rng.Intn(3) == 0 {
			filter.Result = resultOptions[rng.Intn(len(resultOptions))]
		}
		if rng.Intn(5) == 0 {
			filter.Search = "r1"
		}
		// Paging must never influence the aggregates.
		filter.Page = rng.Intn(5) + 1
		filter.PageSize = rng.Intn(3) + 1

		overview, err := h.store.Analytics(h.ctx(), filter)
		if err != nil {
			t.Fatalf("iter %d: analytics: %v", iter, err)
		}
		report, err := h.store.Report(h.ctx(), filter)
		if err != nil {
			t.Fatalf("iter %d: report: %v", iter, err)
		}
		if !sameAdditive(overview.Summary, report.Summary) {
			t.Fatalf("iter %d: overview summary %+v != report summary %+v for %+v", iter, overview.Summary, report.Summary, filter)
		}
		assertAdditive(t, fmt.Sprintf("iter %d %+v", iter, filter), overview)
		if filter.Model != "" && (len(overview.Models) > 1 || (len(overview.Models) == 1 && overview.Models[0].Model != filter.Model)) {
			t.Fatalf("iter %d: model filter leaked models %+v", iter, overview.Models)
		}
		for _, m := range overview.Models {
			if !requested[m.Model] {
				t.Fatalf("iter %d: model row %q is an execution alias, not a requested model", iter, m.Model)
			}
		}
	}
}

// TestAnalyticsOverflowFailsNotCorrupts proves that a token sum beyond int64
// surfaces as an error instead of a silently REAL-rounded aggregate.
func TestAnalyticsOverflowFailsNotCorrupts(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "ovf", TraceID: "t-ovf", KeyID: key.ID, RequestedModel: "m1"})

	// Insert executions whose per-row counters fit int64 but whose sum does
	// not, bypassing the validating writer to model a legacy row set.
	const insert = `INSERT INTO usages (id, request_id, trace_id, key_id, model, tokens_input,
		tokens_cache_read, tokens_cache_write, tokens_output, tokens_reasoning, tokens_total,
		raw_input, raw_output, raw_reasoning, raw_cached, raw_cache_read, raw_cache_create,
		raw_total, at) VALUES (?,?,?,?,?,?,0,0,0,0,?,0,0,0,0,0,0,0,?)`
	huge := int64(math.MaxInt64 - 1)
	for _, id := range []string{"ovf-a", "ovf-b"} {
		if _, err := h.store.db.ExecContext(h.ctx(), insert, id, req.ID, req.TraceID, key.ID, "m1", huge, huge, unixNano(testEpoch)); err != nil {
			t.Fatalf("seed overflow row: %v", err)
		}
	}
	_, err := h.store.Analytics(h.ctx(), Filter{From: testEpoch.Add(-time.Hour), To: testEpoch.Add(time.Hour)})
	if err == nil {
		t.Fatal("overflowing token sum was aggregated instead of failing")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "overflow") {
		t.Fatalf("error %q does not name the overflow", err)
	}
}

// TestAnalyticsJSONShape pins the exact wire keys the dashboard builds on, so a
// renamed or re-nested field is caught before the frontend breaks.
func TestAnalyticsJSONShape(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u1", req, "m1", tokens(3, 0, 0, 4, 1)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})

	overview, err := h.store.Analytics(context.Background(), Filter{From: testEpoch.Add(-time.Hour), To: testEpoch.Add(time.Hour)})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	raw, err := json.Marshal(overview)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Summary       map[string]json.RawMessage   `json:"summary"`
		Trend         []any                        `json:"trend"`
		Models        []map[string]json.RawMessage `json:"models"`
		RetentionFrom any                          `json:"retention_from"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Summary == nil || decoded.Trend == nil || decoded.Models == nil || decoded.RetentionFrom == nil {
		t.Fatalf("overview envelope = %s", raw)
	}
	if len(decoded.Models) != 1 {
		t.Fatalf("models = %s", raw)
	}
	wantSummaryKeys := []string{"requests", "executions", "failed", "pending", "cost_micro_usd",
		"unpriced_executions", "tokens", "complete_executions"}
	for _, k := range wantSummaryKeys {
		if _, ok := decoded.Summary[k]; !ok {
			t.Fatalf("summary is missing %q in %s", k, raw)
		}
	}
	wantModelKeys := append(append([]string{}, wantSummaryKeys...), "model", "latency_ms",
		"latency_samples", "ttft_ms", "ttft_samples", "output_tokens")
	for _, k := range wantModelKeys {
		if _, ok := decoded.Models[0][k]; !ok {
			t.Fatalf("model metrics is missing %q in %s", k, raw)
		}
	}
}
