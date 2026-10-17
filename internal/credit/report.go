package credit

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Report summarizes and pages request history. Counters and token totals are
// aggregated in SQLite; monetary values are derived from the same executions
// through the price book loaded for this query. Only the current page hydrates
// nested executions.
func (s *Store) Report(ctx context.Context, f Filter) (Report, error) {
	return s.report(ctx, f, false)
}

// LookupReport includes full-selection model metrics without widening the
// paged item query. Both sets are read from the same snapshot.
func (s *Store) LookupReport(ctx context.Context, f Filter) (Report, error) {
	return s.report(ctx, f, true)
}

func (s *Store) report(ctx context.Context, f Filter, includeModels bool) (Report, error) {
	now := s.nowUTC()
	from, to, err := s.reportRange(f, now)
	if err != nil {
		return Report{}, err
	}
	where, args, execWhere, execArgs := buildPredicates(f, from, to)
	page, size := normalizePaging(f)
	// The offset is computed and range checked before any query: an unguarded
	// (page-1)*size wraps negative, and SQLite reads a negative OFFSET as zero,
	// quietly returning the first page under an extreme page number.
	offset, err := pageOffset(page, size)
	if err != nil {
		return Report{}, err
	}
	bucketExpr, bucketArgs := trendBucket(f.Bucket)

	// All derived numbers come from one read transaction so a concurrent writer
	// cannot make the page, the total, the trend and the price book disagree.
	var out Report
	err = s.withSnapshot(ctx, func(q querier) error {
		// The price book is loaded exactly once per query and reused for the
		// summary, the trend, the model breakdown and the page.
		book, err := loadPriceBook(ctx, q)
		if err != nil {
			return err
		}
		summary, err := s.reportSummary(ctx, q, where, args, execWhere, execArgs)
		if err != nil {
			return err
		}
		total, err := counterValue(ctx, q, `SELECT COUNT(*) FROM requests r WHERE `+where, args...)
		if err != nil {
			return err
		}
		costs, err := s.projectCosts(ctx, q, book, where, args, execWhere, execArgs,
			bucketExpr, bucketArgs, includeModels)
		if err != nil {
			return err
		}
		summary.CostMicroUSD = costs.total.cost
		summary.UnpricedExecutions = costs.total.unpriced
		trend, err := s.reportTrend(ctx, q, where, args, execWhere, execArgs, bucketExpr, bucketArgs)
		if err != nil {
			return err
		}
		for i := range trend {
			if bucket, ok := costs.buckets[trend[i].Time.UnixNano()]; ok {
				trend[i].Summary.CostMicroUSD = bucket.cost
				trend[i].Summary.UnpricedExecutions = bucket.unpriced
			}
		}
		items, err := s.reportItems(ctx, q, book, where, args, execWhere, execArgs, size, offset)
		if err != nil {
			return err
		}
		out = Report{
			Summary:       summary,
			Trend:         trend,
			Items:         items,
			Total:         total,
			Page:          page,
			PageSize:      size,
			RetentionFrom: s.retentionFrom(now),
		}
		if includeModels {
			models, err := s.reportModels(ctx, q, where, args, execWhere, execArgs)
			if err != nil {
				return err
			}
			// The per-model cost comes from the same projection as the summary
			// and the trend, so the model rows reconcile exactly with both.
			for i := range models {
				if tally, ok := costs.models[models[i].Model]; ok {
					models[i].Summary.CostMicroUSD = tally.cost
					models[i].Summary.UnpricedExecutions = tally.unpriced
				}
			}
			out.Models = models
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	return out, nil
}

// reportRange resolves and validates the half-open UTC window a selection
// reads. Report and Analytics share it so their defaults (retention floor and
// an exclusive "now") and their overflow / empty-range errors can never
// diverge into two overlapping definitions of the same filter.
func (s *Store) reportRange(f Filter, now time.Time) (time.Time, time.Time, error) {
	from := f.From
	if from.IsZero() {
		from = s.retentionFrom(now)
	}
	to := f.To
	if to.IsZero() {
		to = now.Add(time.Nanosecond)
	}
	from, to = from.UTC(), to.UTC()
	// UnixNano silently wraps instants outside SQLite's int64 timestamp range.
	if !time.Unix(0, from.UnixNano()).Equal(from) || !time.Unix(0, to.UnixNano()).Equal(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: report range is not representable at nanosecond precision", ErrInvalid)
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: report range is empty", ErrInvalid)
	}
	return from, to, nil
}

func normalizePaging(f Filter) (int, int) {
	page := f.Page
	if page < 1 {
		page = 1
	}
	size := f.PageSize
	if size < 1 {
		size = 50
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	return page, size
}

// pageOffset returns the row offset of a normalized page as int64. It rejects a
// page whose offset does not fit an int64 instead of letting (page-1)*size wrap
// to a negative value that SQLite would read as the first page.
func pageOffset(page, size int) (int64, error) {
	p, s := int64(page), int64(size)
	if p < 1 || s < 1 {
		return 0, fmt.Errorf("%w: page and page size must be positive", ErrInvalid)
	}
	if p-1 > math.MaxInt64/s {
		return 0, fmt.Errorf("%w: page is out of range", ErrInvalid)
	}
	return (p - 1) * s, nil
}

// monthBucketExpr groups rows by their true UTC calendar month. SQLite stores
// started_at as nanoseconds since the epoch, so dividing by 1e9 yields whole UTC
// seconds and strftime(..., 'start of month') returns that month's UTC first
// second; scaling back to nanoseconds matches the hour and day bucket encoding,
// which is what Point.Time decodes. A constant width cannot express a calendar
// month, so this is derived from the date rather than from an integer division.
const monthBucketExpr = `CAST(strftime('%s', r.started_at/1000000000, 'unixepoch', 'start of month') AS INTEGER) * 1000000000`

// trendBucket maps the requested bucket to its SQL grouping expression and
// arguments. Hour and day are fixed offsets from the Unix epoch, which is a UTC
// midnight, so integer division already lands on true UTC hour and day starts.
// A calendar month is unequal length and uses the date function above. Any other
// value keeps the day fallback.
func trendBucket(requested string) (string, []any) {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "hour":
		const hour = int64(time.Hour)
		return "(r.started_at / ?) * ?", []any{hour, hour}
	case "month":
		return monthBucketExpr, nil
	default:
		const day = int64(24 * time.Hour)
		return "(r.started_at / ?) * ?", []any{day, day}
	}
}

// failedOutcomeList is the shared definition of a non-successful outcome.
const failedOutcomeList = "('" + OutcomeFailed + "','" + OutcomeCanceled + "','" + OutcomeRejected + "')"

func escapeLike(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(v) + "%"
}

// buildPredicates renders the shared filter as request-level and execution-level
// SQL fragments. Execution filters select a matching execution set through
// EXISTS, so requests without matching executions drop out while requests with
// no executions at all remain visible when no execution filter is set.
func buildPredicates(f Filter, from, to time.Time) (string, []any, string, []any) {
	conds := []string{"r.started_at >= ?", "r.started_at < ?"}
	args := []any{unixNano(from), unixNano(to)}
	if f.KeyID != "" {
		conds = append(conds, "r.key_id = ?")
		args = append(args, f.KeyID)
	}
	if f.Model != "" {
		conds = append(conds, "r.model = ?")
		args = append(args, f.Model)
	}
	// A success only counts once it is settled, so an unresolved "success" is
	// never mistaken for a trustworthy one. Failures and cancellations are
	// reported whether or not they could be closed yet.
	switch f.Result {
	case OutcomeSucceeded:
		conds = append(conds, "r.state = '"+stateSettled+"' AND r.outcome = '"+OutcomeSucceeded+"'")
	case OutcomeFailed:
		conds = append(conds, "r.outcome IN "+failedOutcomeList)
	case OutcomeRejected:
		conds = append(conds, "r.outcome = '"+OutcomeRejected+"'")
	case OutcomeCanceled:
		conds = append(conds, "r.outcome = '"+OutcomeCanceled+"'")
	case stateActive:
		conds = append(conds, "r.state = '"+stateActive+"'")
	case statePending:
		conds = append(conds, "r.state IN ('"+stateActive+"','"+statePending+"')")
	}
	if f.Search != "" {
		pattern := escapeLike(f.Search)
		conds = append(conds, `(r.id LIKE ? ESCAPE '\' OR r.trace_id LIKE ? ESCAPE '\' OR r.model LIKE ? ESCAPE '\'
			OR EXISTS (SELECT 1 FROM keys k WHERE k.id = r.key_id AND k.label LIKE ? ESCAPE '\'))`)
		args = append(args, pattern, pattern, pattern, pattern)
	}
	var exec []string
	var execArgs []any
	if f.Provider != "" {
		exec = append(exec, "u.provider = ?")
		execArgs = append(execArgs, f.Provider)
	}
	if f.AuthID != "" {
		exec = append(exec, "u.auth_id = ?")
		execArgs = append(execArgs, f.AuthID)
	}
	if len(exec) > 0 {
		conds = append(conds, "EXISTS (SELECT 1 FROM usages u WHERE u.request_id = r.id AND "+strings.Join(exec, " AND ")+")")
		args = append(args, execArgs...)
	}
	execWhere := "1=1"
	if len(exec) > 0 {
		execWhere = strings.Join(exec, " AND ")
	}
	return strings.Join(conds, " AND "), args, execWhere, execArgs
}

// execAggregate is the SQLite-side aggregate of a selection's executions:
// counts and token sums. Amounts are deliberately absent, because a cost is
// derived in Go from the current price book, not summed from a persisted
// column.
const execAggregate = `COUNT(*),
	COALESCE(SUM(u.reliable),0),
	COALESCE(SUM(u.tokens_input),0),
	COALESCE(SUM(u.tokens_cache_read),0),
	COALESCE(SUM(u.tokens_cache_write),0),
	COALESCE(SUM(u.tokens_output),0),
	COALESCE(SUM(u.tokens_reasoning),0),
	COALESCE(SUM(u.tokens_total),0),
	COALESCE(SUM(CASE WHEN u.quality = '` + QualityComplete + `' THEN u.tokens_cache_read ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN u.quality = '` + QualityComplete + `' THEN u.tokens_input + u.tokens_cache_write ELSE 0 END),0)`

type execTotals struct {
	executions int64
	// reliable is the count of trustworthy executions. It is the usage-side
	// definition of CompleteExecutions and never depends on a price.
	reliable int64
	tokens   Tokens
	// completeCacheRead and completeInputTotal are restricted to executions of
	// complete quality. A NULL or partial quality row contributes zero to both,
	// so the ratio below never mixes trustworthy counters with partial ones.
	completeCacheRead  int64
	completeInputTotal int64
}

func scanExecTotals(scan func(...any) error) (execTotals, error) {
	var t execTotals
	err := scan(&t.executions, &t.reliable, &t.tokens.Input, &t.tokens.CacheRead,
		&t.tokens.CacheWrite, &t.tokens.Output, &t.tokens.Reasoning, &t.tokens.Total,
		&t.completeCacheRead, &t.completeInputTotal)
	return t, err
}

// apply folds one aggregate batch into a summary. The token totals keep their
// documented all-executions meaning; only CacheReadRatio is complete-only. The
// monetary fields stay untouched: the caller fills them from the cost
// projection of the same selection.
func (t execTotals) apply(sum *Summary) error {
	sum.Executions = t.executions
	sum.CompleteExecutions = t.reliable
	sum.Tokens = t.tokens
	canonical, err := checkedAdd(t.completeInputTotal, t.completeCacheRead)
	if err != nil {
		return err
	}
	if canonical > 0 {
		ratio := float64(t.completeCacheRead) / float64(canonical)
		sum.CacheReadRatio = &ratio
	}
	return nil
}

func (s *Store) reportSummary(ctx context.Context, q querier, where string, args []any, execWhere string, execArgs []any) (Summary, error) {
	var sum Summary
	err := q.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN r.outcome IN `+failedOutcomeList+` THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.state IN ('`+stateActive+`','`+statePending+`') THEN 1 ELSE 0 END),0)
		FROM requests r WHERE `+where, args...).Scan(&sum.Requests, &sum.Failed, &sum.Pending)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize requests: %w", err)
	}
	row := q.QueryRowContext(ctx, `SELECT `+execAggregate+`
		FROM usages u JOIN requests r ON r.id = u.request_id
		WHERE `+where+` AND `+execWhere, append(append([]any{}, args...), execArgs...)...)
	totals, err := scanExecTotals(row.Scan)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize executions: %w", err)
	}
	if err := totals.apply(&sum); err != nil {
		return Summary{}, err
	}
	return sum, nil
}

// costTally accumulates the query-time cost of a set of executions: the strict
// sum of the priced ones and the count of the unpriced ones.
type costTally struct {
	cost     int64
	unpriced int64
}

func (t *costTally) add(cost int64, priced bool) error {
	if !priced {
		t.unpriced++
		return nil
	}
	next, err := checkedAdd(t.cost, cost)
	if err != nil {
		return fmt.Errorf("sum execution costs: %w", err)
	}
	t.cost = next
	return nil
}

// selectionCosts is the whole-selection cost projection: the total, the
// per-trend-bucket totals and, when requested, the per-request-model totals.
type selectionCosts struct {
	total   costTally
	buckets map[int64]*costTally
	models  map[string]*costTally
}

// projectCosts streams the minimal execution projection of the selection once
// and prices every execution in Go against the price book loaded for this
// query.
//
// It is a single pass with a per-actual-model match cache, so it never issues a
// price query per execution. Results accumulate into maps keyed by trend bucket
// and request model, so memory grows with the number of groups and not with the
// number of executions, and the page size never limits what the summary, the
// trend or the model breakdown covers. Per execution the amount is computed and
// rounded as an exact integer and then summed strictly; a single execution whose
// product cannot be represented is counted as unpriced instead of corrupting
// the total.
func (s *Store) projectCosts(ctx context.Context, q querier, book *priceBook,
	where string, args []any, execWhere string, execArgs []any,
	bucketExpr string, bucketArgs []any, includeModels bool) (selectionCosts, error) {
	costs := selectionCosts{buckets: map[int64]*costTally{}, models: map[string]*costTally{}}
	query := `SELECT ` + bucketExpr + ` AS bucket, r.model, u.model, u.reliable, u.service_tier,
		u.tokens_input, u.tokens_cache_read, u.tokens_cache_write, u.tokens_output, u.tokens_reasoning
		FROM usages u JOIN requests r ON r.id = u.request_id
		WHERE ` + where + ` AND ` + execWhere
	queryArgs := append(append([]any{}, bucketArgs...), append(append([]any{}, args...), execArgs...)...)
	rows, err := q.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return costs, fmt.Errorf("project execution costs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			bucket                    int64
			requestModel, model, tier string
			reliable                  int
			tokens                    Tokens
		)
		if err := rows.Scan(&bucket, &requestModel, &model, &reliable, &tier,
			&tokens.Input, &tokens.CacheRead, &tokens.CacheWrite, &tokens.Output,
			&tokens.Reasoning); err != nil {
			return costs, fmt.Errorf("scan execution cost: %w", err)
		}
		cost, priced, err := book.executionCost(model, tokens, tier, reliable != 0)
		if err != nil {
			return costs, err
		}
		if err := costs.total.add(cost, priced); err != nil {
			return costs, err
		}
		tally, ok := costs.buckets[bucket]
		if !ok {
			tally = &costTally{}
			costs.buckets[bucket] = tally
		}
		if err := tally.add(cost, priced); err != nil {
			return costs, err
		}
		if includeModels {
			modelTally, ok := costs.models[requestModel]
			if !ok {
				modelTally = &costTally{}
				costs.models[requestModel] = modelTally
			}
			if err := modelTally.add(cost, priced); err != nil {
				return costs, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return costs, fmt.Errorf("iterate execution costs: %w", err)
	}
	return costs, nil
}

func (s *Store) reportTrend(ctx context.Context, q querier, where string, args []any, execWhere string, execArgs []any, bucketExpr string, bucketArgs []any) ([]Point, error) {
	points := map[int64]*Summary{}
	order := []int64{}
	requestRows, err := q.QueryContext(ctx, `SELECT `+bucketExpr+` AS bucket, COUNT(*),
		COALESCE(SUM(CASE WHEN r.outcome IN `+failedOutcomeList+` THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.state IN ('`+stateActive+`','`+statePending+`') THEN 1 ELSE 0 END),0)
		FROM requests r WHERE `+where+` GROUP BY bucket ORDER BY bucket`,
		append(append([]any{}, bucketArgs...), args...)...)
	if err != nil {
		return nil, fmt.Errorf("trend requests: %w", err)
	}
	for requestRows.Next() {
		var (
			b                         int64
			requests, failed, pending int64
		)
		if err := requestRows.Scan(&b, &requests, &failed, &pending); err != nil {
			requestRows.Close()
			return nil, fmt.Errorf("scan trend requests: %w", err)
		}
		points[b] = &Summary{Requests: requests, Failed: failed, Pending: pending}
		order = append(order, b)
	}
	if err := requestRows.Err(); err != nil {
		requestRows.Close()
		return nil, fmt.Errorf("iterate trend requests: %w", err)
	}
	requestRows.Close()

	execRows, err := q.QueryContext(ctx, `SELECT `+bucketExpr+` AS bucket, `+execAggregate+`
		FROM usages u JOIN requests r ON r.id = u.request_id
		WHERE `+where+` AND `+execWhere+` GROUP BY bucket ORDER BY bucket`,
		append(append([]any{}, bucketArgs...), append(append([]any{}, args...), execArgs...)...)...)
	if err != nil {
		return nil, fmt.Errorf("trend executions: %w", err)
	}
	defer execRows.Close()
	for execRows.Next() {
		var b int64
		var totals execTotals
		err := execRows.Scan(&b, &totals.executions, &totals.reliable, &totals.tokens.Input,
			&totals.tokens.CacheRead, &totals.tokens.CacheWrite, &totals.tokens.Output,
			&totals.tokens.Reasoning, &totals.tokens.Total,
			&totals.completeCacheRead, &totals.completeInputTotal)
		if err != nil {
			return nil, fmt.Errorf("scan trend executions: %w", err)
		}
		point, ok := points[b]
		if !ok {
			point = &Summary{}
			points[b] = point
			order = append(order, b)
		}
		if err := totals.apply(point); err != nil {
			return nil, err
		}
	}
	if err := execRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trend executions: %w", err)
	}

	// The monetary fields of every point are filled by the caller from the
	// single cost projection of the same selection, so a trend point can never
	// disagree with the summary it sums to.
	slices.Sort(order)
	out := make([]Point, 0, len(order))
	for _, b := range order {
		out = append(out, Point{Time: fromUnixNano(b), Summary: *points[b]})
	}
	return out, nil
}

func (s *Store) reportItems(ctx context.Context, q querier, book *priceBook, where string, args []any, execWhere string, execArgs []any, size int, offset int64) ([]Request, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+requestColumns+` FROM requests r WHERE `+where+`
		ORDER BY r.started_at DESC, r.id DESC LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), size, offset)...)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	defer rows.Close()
	var entries []*reqRow
	ids := []any{}
	for rows.Next() {
		r, err := scanRequest(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan request: %w", err)
		}
		entries = append(entries, r)
		ids = append(ids, r.id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate requests: %w", err)
	}
	// Nested executions are loaded in one batched query for the page only,
	// honour the same execution filter as the summary, and are priced with the
	// query's price book.
	byRequest := map[string][]Usage{}
	if len(ids) > 0 {
		query := `SELECT ` + usageColumns + ` FROM usages u WHERE u.request_id IN (` + placeholders(len(ids)) + `) AND ` + execWhere + `
			ORDER BY u.at, u.id`
		execRows, err := q.QueryContext(ctx, query, append(append([]any{}, ids...), execArgs...)...)
		if err != nil {
			return nil, fmt.Errorf("list executions: %w", err)
		}
		defer execRows.Close()
		for execRows.Next() {
			u, err := scanUsage(execRows.Scan)
			if err != nil {
				return nil, fmt.Errorf("scan execution: %w", err)
			}
			if err := book.applyCost(u); err != nil {
				return nil, err
			}
			byRequest[u.requestID] = append(byRequest[u.requestID], u.toUsage())
		}
		if err := execRows.Err(); err != nil {
			return nil, fmt.Errorf("iterate executions: %w", err)
		}
	}
	out := make([]Request, 0, len(entries))
	for _, r := range entries {
		executions := byRequest[r.id]
		if executions == nil {
			executions = []Usage{}
		}
		out = append(out, r.toRequest(executions))
	}
	return out, nil
}
