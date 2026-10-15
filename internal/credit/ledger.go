package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

type reqRow struct {
	id            string
	traceID       string
	keyID         string
	model         string
	state         string
	outcome       string
	statusCode    int
	startedAt     time.Time
	completedAt   *time.Time
	held          int64
	price         Price
	tokenEstimate int64
	charge        int64
	resolved      string
}

func (r *reqRow) toRequest(executions []Usage) Request {
	if executions == nil {
		executions = []Usage{}
	}
	return Request{
		ID:           r.id,
		TraceID:      r.traceID,
		KeyID:        r.keyID,
		Model:        r.model,
		Outcome:      r.outcome,
		State:        r.state,
		StartedAt:    r.startedAt,
		CompletedAt:  r.completedAt,
		HeldMicroUSD: r.held,
		Price:        r.price,
		Executions:   executions,
	}
}

const requestColumns = `id, trace_id, key_id, model, state, outcome, status_code, started_at,
	completed_at, held_micro_usd, price_input, price_output, price_reasoning, price_cache_read,
	price_cache_write, price_version, token_estimate, charge_micro_usd, resolved`

func scanRequest(scan func(...any) error) (*reqRow, error) {
	var (
		r         reqRow
		completed sql.NullInt64
		reasoning sql.NullInt64
		started   int64
		in, out   int64
		cr, cw    int64
		version   int64
	)
	if err := scan(&r.id, &r.traceID, &r.keyID, &r.model, &r.state, &r.outcome, &r.statusCode,
		&started, &completed, &r.held, &in, &out, &reasoning, &cr, &cw, &version,
		&r.tokenEstimate, &r.charge, &r.resolved); err != nil {
		return nil, err
	}
	r.startedAt = fromUnixNano(started)
	r.completedAt = scanNano(completed)
	r.price = priceFromRequestRow(in, out, cr, cw, version, reasoning, r.model)
	return &r, nil
}

func loadRequest(ctx context.Context, q querier, where string, args ...any) (*reqRow, error) {
	row := q.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM requests WHERE `+where, args...)
	r, err := scanRequest(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load request: %w", err)
	}
	return r, nil
}

func loadRequestByID(ctx context.Context, q querier, id string) (*reqRow, error) {
	return loadRequest(ctx, q, "id = ?", id)
}

func loadRequestByTrace(ctx context.Context, q querier, traceID string) (*reqRow, error) {
	return loadRequest(ctx, q, "trace_id = ?", traceID)
}

// addMoney accrues an amount into the durable total/day/week/month counters of
// a key, stamped with the request admission period.
func addMoney(ctx context.Context, q querier, keyID string, at time.Time, delta int64) error {
	if delta == 0 {
		return nil
	}
	for _, win := range []string{windowTotal, windowDay, windowWeek, windowMonth} {
		if _, err := q.ExecContext(ctx, `INSERT INTO key_money_counters(key_id, win, period, micro_usd)
			VALUES(?,?,?,?)
			ON CONFLICT(key_id, win, period) DO UPDATE SET micro_usd = micro_usd + excluded.micro_usd`,
			keyID, win, periodOf(win, at), delta); err != nil {
			return fmt.Errorf("update money counter: %w", err)
		}
	}
	return nil
}

// addTokens accrues consumed tokens for a key/model pair. The counters are the
// durable admission basis and survive history cleanup.
func addTokens(ctx context.Context, q querier, keyID, model string, at time.Time, delta int64) error {
	if delta == 0 {
		return nil
	}
	for _, win := range []string{windowTotal, windowDay, windowWeek, windowMonth} {
		if _, err := q.ExecContext(ctx, `INSERT INTO key_token_counters(key_id, model, win, period, tokens)
			VALUES(?,?,?,?,?)
			ON CONFLICT(key_id, model, win, period) DO UPDATE SET tokens = tokens + excluded.tokens`,
			keyID, model, win, periodOf(win, at), delta); err != nil {
			return fmt.Errorf("update token counter: %w", err)
		}
	}
	return nil
}

func counterValue(ctx context.Context, q querier, query string, args ...any) (int64, error) {
	var v sql.NullInt64
	err := q.QueryRowContext(ctx, query, args...).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // absent counter rows denote zero usage
	}
	if err != nil {
		return 0, fmt.Errorf("read counter: %w", err)
	}
	return v.Int64, nil
}

func moneyCounter(ctx context.Context, q querier, keyID, win, period string) (int64, error) {
	return counterValue(ctx, q, `SELECT micro_usd FROM key_money_counters WHERE key_id=? AND win=? AND period=?`,
		keyID, win, period)
}

func tokenCounter(ctx context.Context, q querier, keyID, model, win, period string) (int64, error) {
	return counterValue(ctx, q, `SELECT tokens FROM key_token_counters WHERE key_id=? AND model=? AND win=? AND period=?`,
		keyID, model, win, period)
}

// reservedTokens sums admission estimates of unresolved requests.
func reservedTokens(ctx context.Context, q querier, keyID, model string) (int64, error) {
	return counterValue(ctx, q, `SELECT COALESCE(SUM(token_estimate),0) FROM requests
		WHERE key_id=? AND model=? AND state IN (?,?)`, keyID, model, stateActive, statePending)
}

type usageAgg struct {
	total    int64
	reliable int64
	unknown  int64
}

func usageStats(ctx context.Context, q querier, requestID string) (usageAgg, error) {
	var a usageAgg
	err := q.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(reliable),0),
		COALESCE(SUM(CASE WHEN cost_micro_usd IS NULL THEN 1 ELSE 0 END),0)
		FROM usages WHERE request_id = ?`, requestID).Scan(&a.total, &a.reliable, &a.unknown)
	if err != nil {
		return a, fmt.Errorf("summarize usages: %w", err)
	}
	return a, nil
}

// applyCharge books a known cost against the key and marks the usage charged.
func (s *Store) applyCharge(ctx context.Context, tx *sql.Tx, req *reqRow, usageID string, amount int64) error {
	if err := addMoney(ctx, tx, req.keyID, req.startedAt, amount); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE keys SET spent_micro_usd = spent_micro_usd + ? WHERE id = ?`,
		amount, req.keyID); err != nil {
		return fmt.Errorf("update key spend: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usages SET charged = 1 WHERE id = ?`, usageID); err != nil {
		return fmt.Errorf("mark usage charged: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET charge_micro_usd = charge_micro_usd + ? WHERE id = ?`,
		amount, req.id); err != nil {
		return fmt.Errorf("update request charge: %w", err)
	}
	req.charge += amount
	return nil
}

// settleRequest closes an unresolved request: every known usage is charged
// exactly once and the monetary hold is released.
func (s *Store) settleRequest(ctx context.Context, tx *sql.Tx, req *reqRow, at time.Time, resolved string) error {
	if req.state == stateSettled {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, cost_micro_usd FROM usages
		WHERE request_id = ? AND charged = 0 AND cost_micro_usd IS NOT NULL`, req.id)
	if err != nil {
		return fmt.Errorf("load chargeable usages: %w", err)
	}
	type pending struct {
		id     string
		amount int64
	}
	var chargeable []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.amount); err != nil {
			rows.Close()
			return fmt.Errorf("scan chargeable usage: %w", err)
		}
		chargeable = append(chargeable, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate chargeable usages: %w", err)
	}
	rows.Close()
	for _, p := range chargeable {
		if err := s.applyCharge(ctx, tx, req, p.id, p.amount); err != nil {
			return err
		}
	}
	if req.held != 0 {
		res, err := tx.ExecContext(ctx, `UPDATE keys SET held_micro_usd = held_micro_usd - ?
			WHERE id = ? AND held_micro_usd >= ?`, req.held, req.keyID, req.held)
		if err != nil {
			return fmt.Errorf("release hold: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("release hold: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("%w: hold accounting is inconsistent for key %s", ErrConflict, req.keyID)
		}
		req.held = 0
	}
	completed := req.completedAt
	if completed == nil {
		completed = &at
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET state = ?, resolved = ?, completed_at = ?,
		held_micro_usd = 0 WHERE id = ?`,
		stateSettled, resolved, unixNano(*completed), req.id); err != nil {
		return fmt.Errorf("close request: %w", err)
	}
	req.state = stateSettled
	req.resolved = resolved
	req.completedAt = completed
	return nil
}

// evaluateSettle closes a completed request only when its accounting is
// trustworthy; otherwise the request stays pending with its hold intact.
func (s *Store) evaluateSettle(ctx context.Context, tx *sql.Tx, req *reqRow, at time.Time) error {
	if req.state == stateSettled || req.outcome == "" {
		return nil
	}
	agg, err := usageStats(ctx, tx, req.id)
	if err != nil {
		return err
	}
	switch req.outcome {
	case OutcomeSucceeded:
		if agg.reliable == 0 {
			return nil // asset accounting not yet reported: never bill a fake zero
		}
	case OutcomeFailed:
		if agg.unknown > 0 {
			return nil
		}
		if agg.total == 0 && !isRejectionStatus(req.statusCode) {
			return nil
		}
	default:
		return nil
	}
	return s.settleRequest(ctx, tx, req, at, resolvedAuto)
}

// isRejectionStatus reports a proven pre-execution rejection (4xx upstream).
func isRejectionStatus(code int) bool { return code >= 400 && code < 500 }

func checkedAdd(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("%w: integer overflow", ErrInvalid)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("%w: integer overflow", ErrInvalid)
	}
	return a + b, nil
}
