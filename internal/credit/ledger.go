package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// reqRow is one persisted request. It carries no price and no amount: a request
// is a lifecycle record with a key, a requested model, a state, an outcome and
// the request-time service tier fact.
type reqRow struct {
	id          string
	traceID     string
	keyID       string
	model       string
	state       string
	outcome     string
	statusCode  int
	startedAt   time.Time
	completedAt *time.Time
	serviceTier string
}

func (r *reqRow) toRequest(executions []Usage) Request {
	if executions == nil {
		executions = []Usage{}
	}
	return Request{
		ID:          r.id,
		TraceID:     r.traceID,
		KeyID:       r.keyID,
		Model:       r.model,
		Outcome:     r.outcome,
		State:       r.state,
		StartedAt:   r.startedAt,
		CompletedAt: r.completedAt,
		Executions:  executions,
		ServiceTier: r.serviceTier,
	}
}

const requestColumns = `id, trace_id, key_id, model, state, outcome, status_code, started_at,
	completed_at, service_tier`

func scanRequest(scan func(...any) error) (*reqRow, error) {
	var (
		r         reqRow
		completed sql.NullInt64
		started   int64
	)
	if err := scan(&r.id, &r.traceID, &r.keyID, &r.model, &r.state, &r.outcome, &r.statusCode,
		&started, &completed, &r.serviceTier); err != nil {
		return nil, err
	}
	r.startedAt = fromUnixNano(started)
	r.completedAt = scanNano(completed)
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

// addTokens accrues consumed tokens for a key/model pair. The counters are the
// durable admission basis and survive history cleanup.
func addTokens(ctx context.Context, q querier, keyID, model string, at time.Time, delta int64) error {
	if delta == 0 {
		return nil
	}
	for _, win := range []string{windowTotal, windowDay, windowWeek, windowMonth} {
		if err := bumpTokenCounter(ctx, q, keyID, model, win, periodOf(win, at), delta); err != nil {
			return err
		}
	}
	return nil
}

// bumpTokenCounter adds delta to one token counter row. SQLite silently turns an
// overflowing INTEGER sum into a REAL, which would corrupt the admission basis,
// so the sum is computed in Go with an overflow check and written back as an
// exact integer inside the caller transaction.
func bumpTokenCounter(ctx context.Context, q querier, keyID, model, win, period string, delta int64) error {
	cur, err := tokenCounter(ctx, q, keyID, model, win, period)
	if err != nil {
		return err
	}
	next, err := checkedAdd(cur, delta)
	if err != nil {
		return fmt.Errorf("token counter %s: %w", win, err)
	}
	if next < 0 {
		return fmt.Errorf("%w: negative token counter %s", ErrConflict, win)
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO key_token_counters(key_id, model, win, period, tokens)
		VALUES(?,?,?,?,?)
		ON CONFLICT(key_id, model, win, period) DO UPDATE SET tokens = excluded.tokens`,
		keyID, model, win, period, next); err != nil {
		return fmt.Errorf("update token counter: %w", err)
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

func tokenCounter(ctx context.Context, q querier, keyID, model, win, period string) (int64, error) {
	return counterValue(ctx, q, `SELECT tokens FROM key_token_counters WHERE key_id=? AND model=? AND win=? AND period=?`,
		keyID, model, win, period)
}

type usageAgg struct {
	total    int64
	reliable int64
	unknown  int64
	// success counts reliable executions that did not fail: only those prove a
	// successful request lifecycle.
	success int64
}

func usageStats(ctx context.Context, q querier, requestID string) (usageAgg, error) {
	var a usageAgg
	// "unknown" is usage that is not trustworthy, which is exactly what the
	// reliable flag encodes. It is deliberately not derived from a cost column:
	// there is no persisted cost, and an unpriced execution is not the same as
	// an unreliable one.
	err := q.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(reliable),0),
		COALESCE(SUM(CASE WHEN reliable = 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN reliable = 1 AND failed = 0 THEN 1 ELSE 0 END),0)
		FROM usages WHERE request_id = ?`, requestID).Scan(&a.total, &a.reliable, &a.unknown, &a.success)
	if err != nil {
		return a, fmt.Errorf("summarize usages: %w", err)
	}
	return a, nil
}

// settleRequest closes an unresolved request whose usage is trustworthy. No
// amount is booked or released: admission never held any.
func (s *Store) settleRequest(ctx context.Context, tx *sql.Tx, req *reqRow, at time.Time) error {
	if req.state == stateSettled {
		return nil
	}
	completed := req.completedAt
	if completed == nil {
		completed = &at
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET state = ?, completed_at = ?
		WHERE id = ?`,
		stateSettled, unixNano(*completed), req.id); err != nil {
		return fmt.Errorf("close request: %w", err)
	}
	req.state = stateSettled
	req.completedAt = completed
	return nil
}

// evaluateSettle closes a completed request only when its usage is trustworthy;
// otherwise the request stays pending. A price is never consulted: an unpriced
// execution with complete usage settles normally.
//
// A rejection is the only outcome that may close without any execution event.
// Anything else needs a trustworthy usage record, and a single unknown
// execution keeps the request open so a missing usage event stays visible.
func (s *Store) evaluateSettle(ctx context.Context, tx *sql.Tx, req *reqRow, at time.Time) error {
	if req.state == stateSettled || req.outcome == "" {
		return nil
	}
	agg, err := usageStats(ctx, tx, req.id)
	if err != nil {
		return err
	}
	switch req.outcome {
	case OutcomeRejected:
		// Proven rejection before any execution: no usage is expected. An
		// unaccounted execution still contradicts that claim, so it stays open.
		if agg.unknown > 0 {
			return nil
		}
	case OutcomeSucceeded:
		// Only a successful execution whose accounting is complete and not
		// failed can prove a success; a failed lifecycle event never does.
		if agg.success == 0 || agg.unknown > 0 {
			return nil
		}
	case OutcomeFailed, OutcomeCanceled:
		if agg.total == 0 || agg.unknown > 0 {
			return nil
		}
	default:
		return nil
	}
	return s.settleRequest(ctx, tx, req, at)
}

// reopenRequest returns a settled request to pending. It is used when a late
// execution with untrustworthy usage arrives: the outstanding work must be
// visible again instead of being written off silently. It moves no amount.
func (s *Store) reopenRequest(ctx context.Context, tx *sql.Tx, req *reqRow) error {
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET state = ? WHERE id = ?`,
		statePending, req.id); err != nil {
		return fmt.Errorf("reopen request: %w", err)
	}
	req.state = statePending
	return nil
}

func checkedAdd(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("%w: integer overflow", ErrInvalid)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("%w: integer overflow", ErrInvalid)
	}
	return a + b, nil
}

// baselineToken reads the reset baseline of one model token window/period.
func baselineToken(ctx context.Context, q querier, keyID, model, win, period string) (int64, error) {
	return counterValue(ctx, q, `SELECT tokens FROM key_token_baselines WHERE key_id=? AND model=? AND win=? AND period=?`,
		keyID, model, win, period)
}

// effectiveUsage is the quota view of a durable counter: the part accrued after
// the last reset. A reset never zeroes the durable counter, so this is always
// the durable value minus its baseline, floored at zero so a stale or repeated
// baseline can never produce a negative usage or a refund.
func effectiveUsage(durable, baseline int64) int64 {
	if durable <= baseline {
		return 0
	}
	return durable - baseline
}

// effectiveToken resolves the effective token usage of one model window/period.
func effectiveToken(ctx context.Context, q querier, keyID, model, win, period string, durable int64) (int64, error) {
	base, err := baselineToken(ctx, q, keyID, model, win, period)
	if err != nil {
		return 0, err
	}
	return effectiveUsage(durable, base), nil
}
