package credit

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Report summarizes and pages request history. Aggregation happens in SQLite;
// only the current page hydrates nested executions.
func (s *Store) Report(ctx context.Context, f Filter) (Report, error) {
	now := s.nowUTC()
	from := f.From
	if from.IsZero() {
		from = s.retentionFrom(now)
	}
	to := f.To
	if to.IsZero() {
		to = now.Add(time.Nanosecond)
	}
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		return Report{}, fmt.Errorf("%w: report range is empty", ErrInvalid)
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
	bucket := bucketSize(f.Bucket)

	// All derived numbers come from one read transaction so a concurrent writer
	// cannot make the page, the total and the trend disagree.
	var out Report
	err = s.withSnapshot(ctx, func(q querier) error {
		summary, err := s.reportSummary(ctx, q, where, args, execWhere, execArgs)
		if err != nil {
			return err
		}
		total, err := counterValue(ctx, q, `SELECT COUNT(*) FROM requests r WHERE `+where, args...)
		if err != nil {
			return err
		}
		trend, err := s.reportTrend(ctx, q, where, args, execWhere, execArgs, bucket)
		if err != nil {
			return err
		}
		items, err := s.reportItems(ctx, q, where, args, execWhere, execArgs, size, offset)
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
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	return out, nil
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

// bucketSize maps the requested bucket to nanoseconds; unknown values mean day.
func bucketSize(bucket string) int64 {
	if strings.EqualFold(strings.TrimSpace(bucket), "hour") {
		return int64(time.Hour)
	}
	return int64(24 * time.Hour)
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

const execAggregate = `COUNT(*),
	COALESCE(SUM(CASE WHEN u.cost_micro_usd IS NOT NULL THEN 1 ELSE 0 END),0),
	COALESCE(SUM(COALESCE(u.cost_micro_usd,0)),0),
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
	known      int64
	cost       int64
	tokens     Tokens
	// completeCacheRead and completeInputTotal are restricted to executions of
	// complete quality. A NULL or partial quality row contributes zero to both,
	// so the ratio below never mixes trustworthy counters with partial ones.
	completeCacheRead  int64
	completeInputTotal int64
}

func scanExecTotals(scan func(...any) error) (execTotals, error) {
	var t execTotals
	err := scan(&t.executions, &t.known, &t.cost, &t.tokens.Input, &t.tokens.CacheRead,
		&t.tokens.CacheWrite, &t.tokens.Output, &t.tokens.Reasoning, &t.tokens.Total,
		&t.completeCacheRead, &t.completeInputTotal)
	return t, err
}

// apply folds one aggregate batch into a summary. The token totals keep their
// documented all-executions meaning; only CacheReadRatio is complete-only.
func (t execTotals) apply(sum *Summary) error {
	sum.Executions = t.executions
	sum.CompleteExecutions = t.known
	sum.CostMicroUSD = t.cost
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
	adjustments, err := adjustmentTotals(ctx, q, where, args)
	if err != nil {
		return Summary{}, err
	}
	sum.AdjustmentMicroUSD = adjustments
	return sum, nil
}

// adjustmentTotals sums manual Resolve charges of the filtered requests. The
// predicate is the request-level one: an adjustment belongs to a request, not
// to a single execution, so it is reported whole whenever the request is
// reported. CostMicroUSD stays execution derived, and cost + adjustments
// reconciles with the key ledger spend for the same request selection.
func adjustmentTotals(ctx context.Context, q querier, where string, args []any) (int64, error) {
	total, err := counterValue(ctx, q, `SELECT COALESCE(SUM(a.amount_micro_usd),0) FROM adjustments a
		JOIN requests r ON r.id = a.request_id WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("summarize adjustments: %w", err)
	}
	return total, nil
}

func (s *Store) reportTrend(ctx context.Context, q querier, where string, args []any, execWhere string, execArgs []any, bucket int64) ([]Point, error) {
	points := map[int64]*Summary{}
	order := []int64{}
	requestRows, err := q.QueryContext(ctx, `SELECT (r.started_at / ?) * ? AS bucket, COUNT(*),
		COALESCE(SUM(CASE WHEN r.outcome IN `+failedOutcomeList+` THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.state IN ('`+stateActive+`','`+statePending+`') THEN 1 ELSE 0 END),0)
		FROM requests r WHERE `+where+` GROUP BY bucket ORDER BY bucket`,
		append([]any{bucket, bucket}, args...)...)
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

	execRows, err := q.QueryContext(ctx, `SELECT (r.started_at / ?) * ? AS bucket, `+execAggregate+`
		FROM usages u JOIN requests r ON r.id = u.request_id
		WHERE `+where+` AND `+execWhere+` GROUP BY bucket ORDER BY bucket`,
		append([]any{bucket, bucket}, append(append([]any{}, args...), execArgs...)...)...)
	if err != nil {
		return nil, fmt.Errorf("trend executions: %w", err)
	}
	defer execRows.Close()
	for execRows.Next() {
		var b int64
		var totals execTotals
		err := execRows.Scan(&b, &totals.executions, &totals.known, &totals.cost, &totals.tokens.Input,
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

	// Adjustments follow the bucket of the request they belong to, so the
	// trend always sums to the summary total.
	adjustRows, err := q.QueryContext(ctx, `SELECT (r.started_at / ?) * ? AS bucket,
		COALESCE(SUM(a.amount_micro_usd),0)
		FROM adjustments a JOIN requests r ON r.id = a.request_id
		WHERE `+where+` GROUP BY bucket ORDER BY bucket`,
		append([]any{bucket, bucket}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("trend adjustments: %w", err)
	}
	defer adjustRows.Close()
	for adjustRows.Next() {
		var (
			b     int64
			total int64
		)
		if err := adjustRows.Scan(&b, &total); err != nil {
			return nil, fmt.Errorf("scan trend adjustments: %w", err)
		}
		point, ok := points[b]
		if !ok {
			point = &Summary{}
			points[b] = point
			order = append(order, b)
		}
		point.AdjustmentMicroUSD = total
	}
	if err := adjustRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trend adjustments: %w", err)
	}
	slices.Sort(order)
	out := make([]Point, 0, len(order))
	for _, b := range order {
		out = append(out, Point{Time: fromUnixNano(b), Summary: *points[b]})
	}
	return out, nil
}

func (s *Store) reportItems(ctx context.Context, q querier, where string, args []any, execWhere string, execArgs []any, size int, offset int64) ([]Request, error) {
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
	// Nested executions are loaded in one batched query for the page only, and
	// honour the same execution filter as the summary.
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
			byRequest[u.requestID] = append(byRequest[u.requestID], u.toUsage())
		}
		if err := execRows.Err(); err != nil {
			return nil, fmt.Errorf("iterate executions: %w", err)
		}
	}
	adjustments := map[string]int64{}
	if len(ids) > 0 {
		adjustRows, err := q.QueryContext(ctx, `SELECT a.request_id, COALESCE(SUM(a.amount_micro_usd),0)
			FROM adjustments a WHERE a.request_id IN (`+placeholders(len(ids))+`) GROUP BY a.request_id`, ids...)
		if err != nil {
			return nil, fmt.Errorf("list adjustments: %w", err)
		}
		defer adjustRows.Close()
		for adjustRows.Next() {
			var (
				id    string
				total int64
			)
			if err := adjustRows.Scan(&id, &total); err != nil {
				return nil, fmt.Errorf("scan adjustments: %w", err)
			}
			adjustments[id] = total
		}
		if err := adjustRows.Err(); err != nil {
			return nil, fmt.Errorf("iterate adjustments: %w", err)
		}
	}
	out := make([]Request, 0, len(entries))
	for _, r := range entries {
		executions := byRequest[r.id]
		if executions == nil {
			executions = []Usage{}
		}
		out = append(out, r.toRequest(executions, adjustments[r.id]))
	}
	return out, nil
}

// Resolve closes an unresolved request by administrator decision. Known usage
// is still charged; the action only decides the remaining hold.
func (s *Store) Resolve(ctx context.Context, id string, resolution Resolution) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: request id is required", ErrInvalid)
	}
	if strings.TrimSpace(resolution.Reason) == "" {
		return fmt.Errorf("%w: a resolution reason is required", ErrInvalid)
	}
	switch resolution.Action {
	case ActionRelease, ActionCharge:
	default:
		return fmt.Errorf("%w: unsupported resolution action %q", ErrInvalid, resolution.Action)
	}
	if resolution.AmountMicroUSD < 0 {
		return fmt.Errorf("%w: resolution amount must not be negative", ErrInvalid)
	}
	amount := resolution.AmountMicroUSD
	if resolution.Action == ActionRelease {
		amount = 0
	}
	at := s.nowUTC()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		req, err := loadRequestByID(ctx, tx, id)
		if err != nil {
			return err
		}
		// Only a pending request may be resolved: an active execution still
		// holds a concurrency slot, and resolving it would bypass MaxConcurrent
		// and HasActive accounting.
		if req.state != statePending {
			return fmt.Errorf("%w: request %s is %s, only pending requests can be resolved",
				ErrConflict, req.id, req.state)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO adjustments
			(request_id, key_id, action, amount_micro_usd, reason, at) VALUES(?,?,?,?,?,?)`,
			req.id, req.keyID, resolution.Action, amount, resolution.Reason, unixNano(at)); err != nil {
			return fmt.Errorf("record adjustment: %w", err)
		}
		if amount > 0 {
			if err := addMoney(ctx, tx, req.keyID, req.startedAt, amount); err != nil {
				return err
			}
			if err := bumpKeyColumn(ctx, tx, req.keyID, "spent_micro_usd", amount); err != nil {
				return err
			}
			if err := bumpRequestCharge(ctx, tx, req.id, amount); err != nil {
				return err
			}
			next, err := checkedAdd(req.charge, amount)
			if err != nil {
				return err
			}
			req.charge = next
		}
		return s.settleRequest(ctx, tx, req, at, resolution.Action)
	})
}
