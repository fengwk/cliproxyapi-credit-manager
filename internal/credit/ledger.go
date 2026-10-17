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

// decodePriceSnapshot parses and validates the frozen pricing decision of one
// request. The snapshot column is NOT NULL and it is the only frozen price
// source, so there is no fallback: an empty, corrupt or incomplete snapshot is
// refused instead of being billed as a zero price. The stored price is
// re-validated with the same rules used to write it, which keeps every read
// path bound to a well formed rate card.
func decodePriceSnapshot(raw, model string) (priceSnapshot, error) {
	var s priceSnapshot
	if strings.TrimSpace(raw) == "" {
		return s, fmt.Errorf("%w: request price snapshot is missing", ErrInvalid)
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, fmt.Errorf("%w: decode price snapshot", ErrInvalid)
	}
	// The current writer includes all four rate fields on the base price and
	// every tier, even when zero. Missing or null fields must not become free
	// base rates or inherited tier rates on a damaged snapshot.
	type storedRates struct {
		Rates map[string]*int64 `json:"rates"`
	}
	var stored struct {
		Price storedRates   `json:"price"`
		Tiers []storedRates `json:"tiers"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return s, fmt.Errorf("%w: decode snapshot rate fields", ErrInvalid)
	}
	for i, card := range append([]storedRates{stored.Price}, stored.Tiers...) {
		for _, field := range []string{"input", "output", "cache_read", "cache_write"} {
			if card.Rates[field] == nil {
				return s, fmt.Errorf("%w: price snapshot rate card %d field %s is missing", ErrInvalid, i, field)
			}
		}
	}
	price, err := normalizePriceBody(s.Price)
	if err != nil {
		return s, err
	}
	if price.Model == "" {
		return s, fmt.Errorf("%w: price snapshot carries no model", ErrInvalid)
	}
	if price.Model != model {
		return s, fmt.Errorf("%w: price snapshot model %q does not match request model %q",
			ErrConflict, price.Model, model)
	}
	tiers, err := normalizeTiers(s.Tiers)
	if err != nil {
		return s, err
	}
	// A per-image count is frozen at admission and never defaulted: a corrupted
	// count must fail settlement instead of being billed as a different number.
	if price.IsPerImage() && (s.ImageCount < 1 || s.ImageCount > maxImageCount) {
		return s, fmt.Errorf("%w: per-image snapshot count %d is outside the supported 1..%d range",
			ErrInvalid, s.ImageCount, maxImageCount)
	}
	s.Price = price
	s.Tiers = tiers
	return s, nil
}

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
	charge      int64
	resolved    string
	// snapshot is the frozen pricing decision and the only price source of the
	// request: it is required, so it is a value and not an optional pointer.
	snapshot priceSnapshot
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
		AdjustmentMicroUSD: adjustment,
		Price:              r.snapshot.Price,
		Executions:         executions,
		PricingRuleID:      r.snapshot.RuleID,
		PriceTiers:         r.snapshot.Tiers,
		ServiceTier:        r.snapshot.ServiceTier,
		ImageCount:         r.snapshot.ImageCount,
	}
}

const requestColumns = `id, trace_id, key_id, model, state, outcome, status_code, started_at,
	completed_at, charge_micro_usd, resolved, price_snapshot`

func scanRequest(scan func(...any) error) (*reqRow, error) {
	var (
		r         reqRow
		completed sql.NullInt64
		started   int64
		snapshot  string
	)
	if err := scan(&r.id, &r.traceID, &r.keyID, &r.model, &r.state, &r.outcome, &r.statusCode,
		&started, &completed, &r.charge, &r.resolved, &snapshot); err != nil {
		return nil, err
	}
	r.startedAt = fromUnixNano(started)
	r.completedAt = scanNano(completed)
	decoded, err := decodePriceSnapshot(snapshot, r.model)
	if err != nil {
		return nil, err
	}
	r.snapshot = decoded
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

// settleRequest closes an unresolved request. Known usage was already charged
// when it was recorded, so nothing is written off twice and unknown accounting
// simply stays unknown. No money is released: admission never held any.
func (s *Store) settleRequest(ctx context.Context, tx *sql.Tx, req *reqRow, at time.Time, resolved string) error {
	if req.state == stateSettled {
		return nil
	}
	completed := req.completedAt
	if completed == nil {
		completed = &at
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET state = ?, resolved = ?, completed_at = ?
		WHERE id = ?`,
		stateSettled, resolved, unixNano(*completed), req.id); err != nil {
		return fmt.Errorf("close request: %w", err)
	}
	req.state = stateSettled
	req.resolved = resolved
	req.completedAt = completed
	return nil
}

// evaluateSettle closes a completed request only when its accounting is
// trustworthy; otherwise the request stays pending.
//
// A rejection is the only outcome that may close without any execution event.
// Anything else needs a trustworthy usage record, and a single unknown
// execution keeps the request open so leaked charges cannot be written off
// silently.
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

// reopenRequest returns an automatically closed request to pending. It is used
// when a late execution with untrustworthy accounting arrives: the outstanding
// work must be visible again instead of being written off silently. It moves no
// money, because admission never held any.
func (s *Store) reopenRequest(ctx context.Context, tx *sql.Tx, req *reqRow) error {
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET state = ?, resolved = '' WHERE id = ?`,
		statePending, req.id); err != nil {
		return fmt.Errorf("reopen request: %w", err)
	}
	req.state = statePending
	req.resolved = ""
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

// baselineMoney reads the reset baseline of one money window/period. An absent
// baseline means no reset ever happened for that window, so the durable counter
// is the effective usage.
func baselineMoney(ctx context.Context, q querier, keyID, win, period string) (int64, error) {
	return counterValue(ctx, q, `SELECT micro_usd FROM key_money_baselines WHERE key_id=? AND win=? AND period=?`,
		keyID, win, period)
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

// effectiveMoney resolves the effective money usage of one window/period from
// the durable counter and its reset baseline.
func effectiveMoney(ctx context.Context, q querier, keyID, win, period string, durable int64) (int64, error) {
	base, err := baselineMoney(ctx, q, keyID, win, period)
	if err != nil {
		return 0, err
	}
	return effectiveUsage(durable, base), nil
}

// effectiveToken resolves the effective token usage of one model window/period.
func effectiveToken(ctx context.Context, q querier, keyID, model, win, period string, durable int64) (int64, error) {
	base, err := baselineToken(ctx, q, keyID, model, win, period)
	if err != nil {
		return 0, err
	}
	return effectiveUsage(durable, base), nil
}

// hydrateEffectiveUsage fills the effective (post reset) usage view of a key
// row: the lifetime total from the durable spent column plus the current day,
// week and month money counters. It is used by the management read paths only;
// admission reads the same values with the store clock at admission time.
func (s *Store) hydrateEffectiveUsage(ctx context.Context, q querier, k *keyRow) error {
	now := s.nowUTC()
	base, err := baselineMoney(ctx, q, k.id, windowTotal, periodOf(windowTotal, now))
	if err != nil {
		return err
	}
	k.quotaUsed = effectiveUsage(k.spent, base)
	windows := []struct {
		win string
		dst *int64
	}{
		{windowDay, &k.dayUsed},
		{windowWeek, &k.weekUsed},
		{windowMonth, &k.monthUsed},
	}
	for _, w := range windows {
		period := periodOf(w.win, now)
		durable, err := moneyCounter(ctx, q, k.id, w.win, period)
		if err != nil {
			return err
		}
		effective, err := effectiveMoney(ctx, q, k.id, w.win, period, durable)
		if err != nil {
			return err
		}
		*w.dst = effective
	}
	return nil
}
