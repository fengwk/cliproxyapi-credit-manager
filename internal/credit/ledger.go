package credit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// priceSnapshot is the frozen pricing decision of one request. It captures the
// complete effective price (including its tiers and billing mode) together with
// the resolution facts needed to settle late usage without ever re-reading a
// rule that may have been edited or deleted after admission.
type priceSnapshot struct {
	Price       Price       `json:"price"`
	Tiers       []PriceTier `json:"tiers,omitempty"`
	RuleID      string      `json:"rule_id,omitempty"`
	ServiceTier string      `json:"service_tier,omitempty"`
	ImageCount  int64       `json:"image_count,omitempty"`
}

func encodePriceSnapshot(s priceSnapshot) (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("%w: encode price snapshot", ErrInvalid)
	}
	return string(raw), nil
}

// decodePriceSnapshot returns nil for an empty column, which is how requests
// that predate the snapshot column are recognized.
func decodePriceSnapshot(raw string) (*priceSnapshot, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var s priceSnapshot
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("%w: decode price snapshot", ErrInvalid)
	}
	return &s, nil
}

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
	// initialHold is the pre-authorization hold taken at admission. It is kept
	// after settlement so a late unaccounted execution can restore it.
	initialHold int64
	// snapshot is the frozen pricing decision, nil for a legacy request whose
	// price is rebuilt from the stored rate columns.
	snapshot *priceSnapshot
}

// imageCount is the frozen requested image count of a per-image request. The
// stored value is authoritative and is never clamped: it was validated at
// admission, so a corrupted value must fail settlement instead of being
// silently billed as a different number.
func (r *reqRow) imageCount() int64 {
	if r.snapshot == nil {
		return 1
	}
	return r.snapshot.ImageCount
}

// frozenServiceTier is the normalized service tier frozen at admission, used
// when a usage record reports none of its own.
func (r *reqRow) frozenServiceTier() string {
	if r.snapshot == nil {
		return ""
	}
	return normalizeServiceTier(r.snapshot.ServiceTier)
}

// tiers is the frozen tier list of the request price.
func (r *reqRow) tiers() []PriceTier {
	if r.snapshot == nil {
		return nil
	}
	return r.snapshot.Tiers
}

func (r *reqRow) toRequest(executions []Usage, adjustment int64) Request {
	if executions == nil {
		executions = []Usage{}
	}
	return Request{
		ID:                 r.id,
		TraceID:            r.traceID,
		KeyID:              r.keyID,
		Model:              r.model,
		Outcome:            r.outcome,
		State:              r.state,
		StartedAt:          r.startedAt,
		CompletedAt:        r.completedAt,
		HeldMicroUSD:       r.held,
		AdjustmentMicroUSD: adjustment,
		Price:              r.price,
		Executions:         executions,
	}
}

const requestColumns = `id, trace_id, key_id, model, state, outcome, status_code, started_at,
	completed_at, held_micro_usd, price_input, price_output, price_reasoning, price_cache_read,
	price_cache_write, price_version, token_estimate, charge_micro_usd, resolved,
	initial_hold_micro_usd, price_snapshot`

func scanRequest(scan func(...any) error) (*reqRow, error) {
	var (
		r            reqRow
		completed    sql.NullInt64
		reasoning    sql.NullInt64
		started      int64
		in, out      int64
		cr, cw       int64
		version      int64
		snapshotJSON sql.NullString
	)
	if err := scan(&r.id, &r.traceID, &r.keyID, &r.model, &r.state, &r.outcome, &r.statusCode,
		&started, &completed, &r.held, &in, &out, &reasoning, &cr, &cw, &version,
		&r.tokenEstimate, &r.charge, &r.resolved, &r.initialHold, &snapshotJSON); err != nil {
		return nil, err
	}
	r.startedAt = fromUnixNano(started)
	r.completedAt = scanNano(completed)
	snapshot, err := decodePriceSnapshot(snapshotJSON.String)
	if err != nil {
		return nil, err
	}
	r.snapshot = snapshot
	if snapshot != nil {
		// The frozen snapshot is authoritative; the legacy rate columns are
		// only a downgrade-compatible mirror of the same price.
		r.price = snapshot.Price
	} else {
		r.price = priceFromRequestRow(in, out, cr, cw, version, reasoning, r.model)
	}
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

// periodBounds returns the half open UTC range of a counter window that
// contains at. Every window uses the same concrete range in SQL so admission
// and accrual always agree on which period a value belongs to.
func periodBounds(win string, at time.Time) (time.Time, time.Time) {
	u := at.UTC()
	switch win {
	case windowDay:
		start := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 0, 1)
	case windowWeek:
		day := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		offset := (int(day.Weekday()) + 6) % 7 // Monday starts an ISO week
		start := day.AddDate(0, 0, -offset)
		return start, start.AddDate(0, 0, 7)
	case windowMonth:
		start := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	default:
		return time.Time{}, time.Time{}
	}
}

// addMoney accrues an amount into the durable total/day/week/month counters of
// a key, stamped with the request admission period.
func addMoney(ctx context.Context, q querier, keyID string, at time.Time, delta int64) error {
	if delta == 0 {
		return nil
	}
	for _, win := range []string{windowTotal, windowDay, windowWeek, windowMonth} {
		if err := bumpMoneyCounter(ctx, q, keyID, win, periodOf(win, at), delta); err != nil {
			return err
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
		if err := bumpTokenCounter(ctx, q, keyID, model, win, periodOf(win, at), delta); err != nil {
			return err
		}
	}
	return nil
}

// bumpMoneyCounter adds delta to one money counter row. SQLite silently turns an
// overflowing INTEGER sum into a REAL, which would corrupt money, so the sum is
// computed in Go with an overflow check and written back as an exact integer
// inside the caller transaction.
func bumpMoneyCounter(ctx context.Context, q querier, keyID, win, period string, delta int64) error {
	cur, err := moneyCounter(ctx, q, keyID, win, period)
	if err != nil {
		return err
	}
	next, err := checkedAdd(cur, delta)
	if err != nil {
		return fmt.Errorf("money counter %s: %w", win, err)
	}
	if next < 0 {
		return fmt.Errorf("%w: negative money counter %s", ErrConflict, win)
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO key_money_counters(key_id, win, period, micro_usd)
		VALUES(?,?,?,?)
		ON CONFLICT(key_id, win, period) DO UPDATE SET micro_usd = excluded.micro_usd`,
		keyID, win, period, next); err != nil {
		return fmt.Errorf("update money counter: %w", err)
	}
	return nil
}

// bumpTokenCounter is the token counterpart of bumpMoneyCounter.
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

// bumpKeyColumn applies a checked delta to one monetary column of a key row.
// column is always a package constant.
func bumpKeyColumn(ctx context.Context, q querier, keyID, column string, delta int64) error {
	cur, err := counterValue(ctx, q, `SELECT `+column+` FROM keys WHERE id = ?`, keyID)
	if err != nil {
		return err
	}
	next, err := checkedAdd(cur, delta)
	if err != nil {
		return fmt.Errorf("key %s: %w", column, err)
	}
	if next < 0 {
		return fmt.Errorf("%w: negative %s for key %s", ErrConflict, column, keyID)
	}
	res, err := q.ExecContext(ctx, `UPDATE keys SET `+column+` = ? WHERE id = ?`, next, keyID)
	if err != nil {
		return fmt.Errorf("update key %s: %w", column, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update key %s: %w", column, err)
	}
	if n != 1 {
		return fmt.Errorf("%w: key %s not found for %s", ErrConflict, keyID, column)
	}
	return nil
}

// bumpRequestCharge accrues the booked cost of a request the same overflow safe
// way as the key ledger.
func bumpRequestCharge(ctx context.Context, q querier, requestID string, delta int64) error {
	cur, err := counterValue(ctx, q, `SELECT charge_micro_usd FROM requests WHERE id = ?`, requestID)
	if err != nil {
		return err
	}
	next, err := checkedAdd(cur, delta)
	if err != nil {
		return fmt.Errorf("request charge: %w", err)
	}
	if _, err := q.ExecContext(ctx, `UPDATE requests SET charge_micro_usd = ? WHERE id = ?`, next, requestID); err != nil {
		return fmt.Errorf("update request charge: %w", err)
	}
	return nil
}

// periodHold sums the still open holds of unresolved requests admitted inside
// the window period. Holds never move between periods, so last month's pending
// work cannot consume this month's budget, while the lifetime window keeps
// counting every unresolved hold.
func periodHold(ctx context.Context, q querier, keyID, win string, at time.Time) (int64, error) {
	start, end := periodBounds(win, at)
	return counterValue(ctx, q, `SELECT COALESCE(SUM(held_micro_usd),0) FROM requests
		WHERE key_id = ? AND state IN (?,?) AND started_at >= ? AND started_at < ?`,
		keyID, stateActive, statePending, unixNano(start), unixNano(end))
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

// reservedTokens sums admission estimates of unresolved requests for one
// window. Period windows only count requests admitted inside that period, so a
// request opened last month does not consume this month's token budget; the
// lifetime window still counts every unresolved request.
func reservedTokens(ctx context.Context, q querier, keyID, model, win string, at time.Time) (int64, error) {
	if win == windowTotal {
		return counterValue(ctx, q, `SELECT COALESCE(SUM(token_estimate),0) FROM requests
			WHERE key_id=? AND model=? AND state IN (?,?)`, keyID, model, stateActive, statePending)
	}
	start, end := periodBounds(win, at)
	return counterValue(ctx, q, `SELECT COALESCE(SUM(token_estimate),0) FROM requests
		WHERE key_id=? AND model=? AND state IN (?,?) AND started_at >= ? AND started_at < ?`,
		keyID, model, stateActive, statePending, unixNano(start), unixNano(end))
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
	// "unknown" is accounting that is not trustworthy, which is exactly what the
	// reliable flag encodes. It is deliberately not "cost IS NULL": a per-image
	// price books a single charge per request, so its reliable usage rows carry
	// no per-execution cost and must not be mistaken for unknown accounting.
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

// applyCharge books a known cost against the key and marks the usage charged.
// The amount is written back to the usage row as well, so a charge that is not
// visible in the per-execution column (the single per-image lifecycle charge)
// still reconciles with the report and the key ledger.
func (s *Store) applyCharge(ctx context.Context, tx *sql.Tx, req *reqRow, usageID string, amount int64) error {
	if err := addMoney(ctx, tx, req.keyID, req.startedAt, amount); err != nil {
		return err
	}
	if err := bumpKeyColumn(ctx, tx, req.keyID, "spent_micro_usd", amount); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usages SET charged = 1, cost_micro_usd = ? WHERE id = ?`,
		amount, usageID); err != nil {
		return fmt.Errorf("mark usage charged: %w", err)
	}
	if err := bumpRequestCharge(ctx, tx, req.id, amount); err != nil {
		return err
	}
	// req.charge mirrors what was just persisted, so the cached row used by the
	// rest of the transaction cannot drift from the ledger.
	next, err := checkedAdd(req.charge, amount)
	if err != nil {
		return err
	}
	req.charge = next
	return nil
}

// settleRequest closes an unresolved request and releases its remaining hold.
// Known usage was already charged when it was recorded, so nothing is written
// off twice and unknown accounting simply stays unknown.
func (s *Store) settleRequest(ctx context.Context, tx *sql.Tx, req *reqRow, at time.Time, resolved string) error {
	if req.state == stateSettled {
		return nil
	}
	if req.held != 0 {
		// A negative result would mean the request released more than the key
		// ever held, so bumpKeyColumn fails instead of corrupting the ledger.
		if err := bumpKeyColumn(ctx, tx, req.keyID, "held_micro_usd", -req.held); err != nil {
			return fmt.Errorf("release hold: %w", err)
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
//
// A rejection is the only outcome that may close without any execution event.
// Anything else needs a trustworthy usage record, and a single unknown
// execution keeps the request (and the remaining hold) open so leaked charges
// cannot be written off silently.
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
	return s.settleRequest(ctx, tx, req, at, resolvedAuto)
}

// reopenRequest returns an automatically closed request to pending, restoring
// the pre-authorization hold it originally took. It is used when a late
// execution with untrustworthy accounting arrives: the outstanding work must be
// visible again instead of being written off silently.
func (s *Store) reopenRequest(ctx context.Context, tx *sql.Tx, req *reqRow) error {
	if req.initialHold != 0 {
		if err := bumpKeyColumn(ctx, tx, req.keyID, "held_micro_usd", req.initialHold); err != nil {
			return fmt.Errorf("restore hold: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET state = ?, resolved = '', held_micro_usd = ?
		WHERE id = ?`, statePending, req.initialHold, req.id); err != nil {
		return fmt.Errorf("reopen request: %w", err)
	}
	req.state = statePending
	req.resolved = ""
	req.held = req.initialHold
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
