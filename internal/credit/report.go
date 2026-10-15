package credit

import (
	"context"
	"database/sql"
	"fmt"
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
	bucket := bucketSize(f.Bucket)

	summary, err := s.reportSummary(ctx, where, args, execWhere, execArgs)
	if err != nil {
		return Report{}, err
	}
	total, err := counterValue(ctx, s.db, `SELECT COUNT(*) FROM requests r WHERE `+where, args...)
	if err != nil {
		return Report{}, err
	}
	trend, err := s.reportTrend(ctx, where, args, execWhere, execArgs, bucket)
	if err != nil {
		return Report{}, err
	}
	items, err := s.reportItems(ctx, where, args, execWhere, execArgs, page, size)
	if err != nil {
		return Report{}, err
	}
	return Report{
		Summary:       summary,
		Trend:         trend,
		Items:         items,
		Total:         total,
		Page:          page,
		PageSize:      size,
		RetentionFrom: s.retentionFrom(now),
	}, nil
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

// bucketSize maps the requested bucket to nanoseconds; unknown values mean day.
func bucketSize(bucket string) int64 {
	if strings.EqualFold(strings.TrimSpace(bucket), "hour") {
		return int64(time.Hour)
	}
	return int64(24 * time.Hour)
}

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
	switch f.Result {
	case OutcomeSucceeded:
		conds = append(conds, "r.state = '"+stateSettled+"' AND r.outcome = '"+OutcomeSucceeded+"'")
	case OutcomeFailed:
		conds = append(conds, "r.state = '"+stateSettled+"' AND r.outcome = '"+OutcomeFailed+"'")
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
	COALESCE(SUM(u.tokens_total),0)`

type execTotals struct {
	executions int64
	known      int64
	cost       int64
	tokens     Tokens
}

func scanExecTotals(scan func(...any) error) (execTotals, error) {
	var t execTotals
	err := scan(&t.executions, &t.known, &t.cost, &t.tokens.Input, &t.tokens.CacheRead,
		&t.tokens.CacheWrite, &t.tokens.Output, &t.tokens.Reasoning, &t.tokens.Total)
	return t, err
}

func (t execTotals) apply(sum *Summary) {
	sum.Executions = t.executions
	sum.CompleteExecutions = t.known
	sum.CostMicroUSD = t.cost
	sum.Tokens = t.tokens
	canonical := t.tokens.Input + t.tokens.CacheRead + t.tokens.CacheWrite
	if canonical > 0 {
		ratio := float64(t.tokens.CacheRead) / float64(canonical)
		sum.CacheReadRatio = &ratio
	}
}

func (s *Store) reportSummary(ctx context.Context, where string, args []any, execWhere string, execArgs []any) (Summary, error) {
	var sum Summary
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN r.state = '`+stateSettled+`' AND r.outcome = '`+OutcomeFailed+`' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.state IN ('`+stateActive+`','`+statePending+`') THEN 1 ELSE 0 END),0)
		FROM requests r WHERE `+where, args...).Scan(&sum.Requests, &sum.Failed, &sum.Pending)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize requests: %w", err)
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+execAggregate+`
		FROM usages u JOIN requests r ON r.id = u.request_id
		WHERE `+where+` AND `+execWhere, append(append([]any{}, args...), execArgs...)...)
	totals, err := scanExecTotals(row.Scan)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize executions: %w", err)
	}
	totals.apply(&sum)
	return sum, nil
}

func (s *Store) reportTrend(ctx context.Context, where string, args []any, execWhere string, execArgs []any, bucket int64) ([]Point, error) {
	points := map[int64]*Summary{}
	order := []int64{}
	requestRows, err := s.db.QueryContext(ctx, `SELECT (r.started_at / ?) * ? AS bucket, COUNT(*),
		COALESCE(SUM(CASE WHEN r.state = '`+stateSettled+`' AND r.outcome = '`+OutcomeFailed+`' THEN 1 ELSE 0 END),0),
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

	execRows, err := s.db.QueryContext(ctx, `SELECT (r.started_at / ?) * ? AS bucket, `+execAggregate+`
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
			&totals.tokens.Reasoning, &totals.tokens.Total)
		if err != nil {
			return nil, fmt.Errorf("scan trend executions: %w", err)
		}
		point, ok := points[b]
		if !ok {
			point = &Summary{}
			points[b] = point
			order = append(order, b)
		}
		totals.apply(point)
	}
	if err := execRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trend executions: %w", err)
	}
	slices.Sort(order)
	out := make([]Point, 0, len(order))
	for _, b := range order {
		out = append(out, Point{Time: fromUnixNano(b), Summary: *points[b]})
	}
	return out, nil
}

func (s *Store) reportItems(ctx context.Context, where string, args []any, execWhere string, execArgs []any, page, size int) ([]Request, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestColumns+` FROM requests r WHERE `+where+`
		ORDER BY r.started_at DESC, r.id DESC LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), size, (page-1)*size)...)
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
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		query := `SELECT ` + usageColumns + ` FROM usages u WHERE u.request_id IN (` + placeholders + `) AND ` + execWhere + `
			ORDER BY u.at, u.id`
		execRows, err := s.db.QueryContext(ctx, query, append(append([]any{}, ids...), execArgs...)...)
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
		if req.state == stateSettled {
			return fmt.Errorf("%w: request is already settled", ErrConflict)
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
			if _, err := tx.ExecContext(ctx, `UPDATE keys SET spent_micro_usd = spent_micro_usd + ? WHERE id = ?`,
				amount, req.keyID); err != nil {
				return fmt.Errorf("update key spend: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE requests SET charge_micro_usd = charge_micro_usd + ? WHERE id = ?`,
				amount, req.id); err != nil {
				return fmt.Errorf("update request charge: %w", err)
			}
			req.charge += amount
		}
		return s.settleRequest(ctx, tx, req, at, resolution.Action)
	})
}
