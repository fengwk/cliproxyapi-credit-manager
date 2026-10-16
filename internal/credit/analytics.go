package credit

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// ModelMetrics is one model's slice of a filtered selection, grouped by the
// model the client requested (requests.model) and never by an execution alias
// (usages.model). A provider alias, fallback or retry therefore cannot split one
// client model into several rows or move its tokens and cost to another model.
//
// Summary is embedded so every per-model row exposes the same keys and meanings
// as the report summary: requests, executions, failed, pending, cost_micro_usd,
// adjustment_micro_usd, tokens, complete_executions and cache_read_ratio. Those
// additive counters reconcile exactly with the whole-selection summary.
//
// The remaining fields are integer sums, never ratios: cost stays integer
// microUSD and the token counters are whole tokens, so a client renders unit
// cost from counts instead of importing money as a float.
//
// LatencyMS and TTFTMS are summed milliseconds; LatencySamples and TTFTSamples
// count only executions that recorded a positive measurement, so an undefined
// mean is a zero sample count the caller shows as null rather than a fabricated
// 0 averaged in from unmeasured executions.
//
// OutputTokens sums the generated tokens (output + reasoning). Paired with
// LatencyMS it yields an aggregate generated-tokens-per-second rate for the
// whole slice; it is deliberately not presented as an instantaneous streaming
// throughput.
type ModelMetrics struct {
	Model string `json:"model"`
	Summary
	LatencyMS            int64 `json:"latency_ms"`
	LatencySamples       int64 `json:"latency_samples"`
	TTFTMS               int64 `json:"ttft_ms"`
	TTFTSamples          int64 `json:"ttft_samples"`
	OutputTokens         int64 `json:"output_tokens"`
	MeasuredOutputTokens int64 `json:"measured_output_tokens"`
}

// Analytics is the management overview of one filtered selection: the same
// summary and trend Report computes for that selection, read from one snapshot,
// plus a per-model breakdown. Page and page size are intentionally ignored: the
// model distribution aggregates the full matched set, so the shares and ranking
// the dashboard draws are never limited to one page of detail rows.
type Analytics struct {
	Summary       Summary        `json:"summary"`
	Trend         []Point        `json:"trend"`
	Models        []ModelMetrics `json:"models"`
	RetentionFrom time.Time      `json:"retention_from"`
}

// Analytics aggregates a filter over its whole matched set. Summary and trend
// are the exact report numbers for the same selection, resolved through the
// shared report range and predicate builder, and all four results are read
// under one snapshot so a concurrent writer cannot make the summary, the trend
// and the model breakdown disagree.
func (s *Store) Analytics(ctx context.Context, f Filter) (Analytics, error) {
	now := s.nowUTC()
	from, to, err := s.reportRange(f, now)
	if err != nil {
		return Analytics{}, err
	}
	where, args, execWhere, execArgs := buildPredicates(f, from, to)
	bucketExpr, bucketArgs := trendBucket(f.Bucket)

	var out Analytics
	err = s.withSnapshot(ctx, func(q querier) error {
		summary, err := s.reportSummary(ctx, q, where, args, execWhere, execArgs)
		if err != nil {
			return err
		}
		trend, err := s.reportTrend(ctx, q, where, args, execWhere, execArgs, bucketExpr, bucketArgs)
		if err != nil {
			return err
		}
		models, err := s.reportModels(ctx, q, where, args, execWhere, execArgs)
		if err != nil {
			return err
		}
		out = Analytics{
			Summary:       summary,
			Trend:         trend,
			Models:        models,
			RetentionFrom: s.retentionFrom(now),
		}
		return nil
	})
	if err != nil {
		return Analytics{}, err
	}
	return out, nil
}

// modelExecAggregate extends execAggregate with the per-model latency counters.
// The sample counts are restricted to executions with a positive measurement so
// a mean is defined exactly when its sample count is positive; an unmeasured
// execution contributes zero to the sum without dragging down the mean.
const modelExecAggregate = execAggregate + `,
	COALESCE(SUM(CASE WHEN u.latency_ms > 0 THEN u.latency_ms ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN u.latency_ms > 0 THEN 1 ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN u.ttft_ms > 0 THEN u.ttft_ms ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN u.ttft_ms > 0 THEN 1 ELSE 0 END),0),
	COALESCE(SUM(u.tokens_output + u.tokens_reasoning),0),
	COALESCE(SUM(CASE WHEN u.latency_ms > 0 THEN u.tokens_output + u.tokens_reasoning ELSE 0 END),0)`

// reportModels aggregates three model-keyed result sets inside the caller's
// snapshot: request-level counters, execution-level counters and adjustments.
// Every query groups and filters in SQLite, so the full matched set is
// aggregated without listing request detail rows and without one query per
// model. Request-level rows always sum to the summary because every request has
// exactly one client model; execution-level rows do too because every execution
// belongs to exactly one request.
func (s *Store) reportModels(ctx context.Context, q querier, where string, args []any, execWhere string, execArgs []any) ([]ModelMetrics, error) {
	byModel := map[string]*ModelMetrics{}
	order := []string{}
	ensure := func(model string) *ModelMetrics {
		metrics, ok := byModel[model]
		if !ok {
			metrics = &ModelMetrics{Model: model}
			byModel[model] = metrics
			order = append(order, model)
		}
		return metrics
	}

	requestRows, err := q.QueryContext(ctx, `SELECT r.model, COUNT(*),
		COALESCE(SUM(CASE WHEN r.outcome IN `+failedOutcomeList+` THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.state IN ('`+stateActive+`','`+statePending+`') THEN 1 ELSE 0 END),0)
		FROM requests r WHERE `+where+` GROUP BY r.model`, args...)
	if err != nil {
		return nil, fmt.Errorf("group requests by model: %w", err)
	}
	for requestRows.Next() {
		var (
			model                     string
			requests, failed, pending int64
		)
		if err := requestRows.Scan(&model, &requests, &failed, &pending); err != nil {
			requestRows.Close()
			return nil, fmt.Errorf("scan model requests: %w", err)
		}
		metrics := ensure(model)
		metrics.Requests = requests
		metrics.Failed = failed
		metrics.Pending = pending
	}
	if err := requestRows.Err(); err != nil {
		requestRows.Close()
		return nil, fmt.Errorf("iterate model requests: %w", err)
	}
	// The cursor is closed before the next query: the snapshot holds a single
	// connection, so only one statement may be open at a time.
	requestRows.Close()

	execRows, err := q.QueryContext(ctx, `SELECT r.model, `+modelExecAggregate+`
		FROM usages u JOIN requests r ON r.id = u.request_id
		WHERE `+where+` AND `+execWhere+` GROUP BY r.model`,
		append(append([]any{}, args...), execArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("group executions by model: %w", err)
	}
	for execRows.Next() {
		var model string
		var totals execTotals
		var latencyMS, latencySamples, ttftMS, ttftSamples, outputTokens, measuredOutputTokens int64
		if err := execRows.Scan(&model, &totals.executions, &totals.known, &totals.cost,
			&totals.tokens.Input, &totals.tokens.CacheRead, &totals.tokens.CacheWrite,
			&totals.tokens.Output, &totals.tokens.Reasoning, &totals.tokens.Total,
			&totals.completeCacheRead, &totals.completeInputTotal,
			&latencyMS, &latencySamples, &ttftMS, &ttftSamples, &outputTokens, &measuredOutputTokens); err != nil {
			execRows.Close()
			return nil, fmt.Errorf("scan model executions: %w", err)
		}
		metrics := ensure(model)
		if err := totals.apply(&metrics.Summary); err != nil {
			execRows.Close()
			return nil, err
		}
		metrics.LatencyMS = latencyMS
		metrics.LatencySamples = latencySamples
		metrics.TTFTMS = ttftMS
		metrics.TTFTSamples = ttftSamples
		metrics.OutputTokens = outputTokens
		metrics.MeasuredOutputTokens = measuredOutputTokens
	}
	if err := execRows.Err(); err != nil {
		execRows.Close()
		return nil, fmt.Errorf("iterate model executions: %w", err)
	}
	execRows.Close()

	adjustRows, err := q.QueryContext(ctx, `SELECT r.model, COALESCE(SUM(a.amount_micro_usd),0)
		FROM adjustments a JOIN requests r ON r.id = a.request_id
		WHERE `+where+` GROUP BY r.model`, args...)
	if err != nil {
		return nil, fmt.Errorf("group adjustments by model: %w", err)
	}
	for adjustRows.Next() {
		var (
			model string
			total int64
		)
		if err := adjustRows.Scan(&model, &total); err != nil {
			adjustRows.Close()
			return nil, fmt.Errorf("scan model adjustments: %w", err)
		}
		ensure(model).AdjustmentMicroUSD = total
	}
	if err := adjustRows.Err(); err != nil {
		adjustRows.Close()
		return nil, fmt.Errorf("iterate model adjustments: %w", err)
	}
	adjustRows.Close()

	// A stable model-name order keeps the response reproducible; the client
	// ranks by the metric it shows.
	slices.Sort(order)
	out := make([]ModelMetrics, 0, len(order))
	for _, model := range order {
		out = append(out, *byModel[model])
	}
	return out, nil
}
