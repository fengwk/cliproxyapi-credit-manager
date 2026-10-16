package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Reserve admits one execution. It is idempotent by lifecycle ID and persists
// the trace/key binding before any execution starts.
func (s *Store) Reserve(ctx context.Context, in ReserveInput) (Request, error) {
	if strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.TraceID) == "" ||
		strings.TrimSpace(in.KeyID) == "" || strings.TrimSpace(in.RequestedModel) == "" {
		return Request{}, fmt.Errorf("%w: id, trace id, key id and requested model are required", ErrInvalid)
	}
	if in.InputEstimate < 0 || in.OutputEstimate < 0 {
		return Request{}, fmt.Errorf("%w: token estimates must not be negative", ErrInvalid)
	}
	estimate, err := checkedAdd(in.InputEstimate, in.OutputEstimate)
	if err != nil {
		return Request{}, err
	}
	at := s.resolveAt(in.At)
	var out Request
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		existing, err := loadRequestByID(ctx, tx, in.ID)
		switch {
		case err == nil:
			if existing.traceID != in.TraceID || existing.keyID != in.KeyID || existing.model != in.RequestedModel {
				return fmt.Errorf("%w: lifecycle id already bound to another request", ErrConflict)
			}
			adjustments, err := adjustmentTotals(ctx, tx, "r.id = ?", []any{existing.id})
			if err != nil {
				return err
			}
			out = existing.toRequest(nil, adjustments)
			return nil
		case errors.Is(err, ErrNotFound):
		default:
			return err
		}
		if _, err := loadRequestByTrace(ctx, tx, in.TraceID); err == nil {
			return fmt.Errorf("%w: trace id already bound to another request", ErrConflict)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		key, err := loadKey(ctx, tx, "id = ?", in.KeyID)
		if err != nil {
			return err
		}
		if err := hydrateLimits(ctx, tx, key); err != nil {
			return err
		}
		if !key.spec.Enabled {
			return ErrUnauthorized
		}
		if key.spec.ExpiresAt != nil && !at.Before(*key.spec.ExpiresAt) {
			return ErrUnauthorized
		}
		if !modelAllowed(key.spec.AllowedModels, in.RequestedModel) {
			return fmt.Errorf("%w: model %s is not allowed for this key", ErrDenied, in.RequestedModel)
		}
		price, err := lookupPrice(ctx, tx, in.RequestedModel)
		if err != nil {
			return err
		}
		hold, err := computeCost(price, Tokens{
			Input:  in.InputEstimate,
			Output: in.OutputEstimate,
			Total:  estimate,
		})
		if err != nil {
			return err
		}
		if err := s.checkMoneyLimits(ctx, tx, key, at, hold); err != nil {
			return err
		}
		if err := s.checkConcurrency(ctx, tx, key); err != nil {
			return err
		}
		if err := s.checkBacklog(ctx, tx, key); err != nil {
			return err
		}
		if err := s.checkTokenLimits(ctx, tx, key, in.RequestedModel, estimate, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO requests
			(id, trace_id, key_id, model, state, outcome, status_code, started_at, completed_at,
			 held_micro_usd, price_input, price_output, price_reasoning, price_cache_read,
			 price_cache_write, price_version, token_estimate, charge_micro_usd, resolved,
			 initial_hold_micro_usd)
			VALUES(?,?,?,?,?,'',0,?,NULL,?,?,?,?,?,?,?,?,0,'',?)`,
			in.ID, in.TraceID, in.KeyID, in.RequestedModel, stateActive, unixNano(at), hold,
			price.Rates.Input, price.Rates.Output, price.Rates.Reasoning, price.Rates.CacheRead,
			price.Rates.CacheWrite, price.Version, estimate, hold); err != nil {
			return fmt.Errorf("insert request: %w", err)
		}
		if hold != 0 {
			if err := bumpKeyColumn(ctx, tx, key.id, "held_micro_usd", hold); err != nil {
				return fmt.Errorf("update key hold: %w", err)
			}
		}
		out = Request{
			ID:           in.ID,
			TraceID:      in.TraceID,
			KeyID:        in.KeyID,
			Model:        in.RequestedModel,
			State:        stateActive,
			StartedAt:    at,
			HeldMicroUSD: hold,
			Price:        price,
			Executions:   []Usage{},
		}
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	return out, nil
}

func (s *Store) checkMoneyLimits(ctx context.Context, tx *sql.Tx, key *keyRow, at time.Time, hold int64) error {
	// A soft deleted key must never admit new work. This is the single
	// admission gate for deleted keys: money limits are always evaluated, even
	// when no monetary limit is configured, before any execution is admitted.
	if key.deletedAt != nil {
		return ErrUnauthorized
	}
	if quota := key.spec.QuotaMicroUSD; quota > 0 {
		// Effective usage is the durable lifetime spend minus the operator
		// reset baseline. A reset therefore clears quota headroom without ever
		// erasing the true ledger total.
		effective, err := effectiveMoney(ctx, tx, key.id, windowTotal, periodOf(windowTotal, at), key.spent)
		if err != nil {
			return err
		}
		committed, err := checkedAdd(effective, key.held)
		if err != nil {
			return err
		}
		total, err := checkedAdd(committed, hold)
		if err != nil {
			return err
		}
		if total > quota {
			return fmt.Errorf("%w: lifetime monetary limit reached", ErrQuota)
		}
	}
	limits := []struct {
		win   string
		limit int64
	}{
		{windowDay, key.spec.DailyMicroUSD},
		{windowWeek, key.spec.WeeklyMicroUSD},
		{windowMonth, key.spec.MonthlyMicroUSD},
	}
	for _, l := range limits {
		if l.limit <= 0 {
			continue
		}
		raw, err := moneyCounter(ctx, tx, key.id, l.win, periodOf(l.win, at))
		if err != nil {
			return err
		}
		spent, err := effectiveMoney(ctx, tx, key.id, l.win, periodOf(l.win, at), raw)
		if err != nil {
			return err
		}
		// Unresolved requests admitted in this same period still hold budget
		// that has not been charged yet; without them two concurrent requests
		// could each pass the check and jointly exceed the limit. A reset never
		// touches these holds, so late or unknown obligations keep counting.
		held, err := periodHold(ctx, tx, key.id, l.win, at)
		if err != nil {
			return err
		}
		total, err := checkedAdd(spent, held)
		if err != nil {
			return err
		}
		total, err = checkedAdd(total, hold)
		if err != nil {
			return err
		}
		if total > l.limit {
			return fmt.Errorf("%w: %s monetary limit reached", ErrQuota, l.win)
		}
	}
	return nil
}

func (s *Store) checkConcurrency(ctx context.Context, tx *sql.Tx, key *keyRow) error {
	if key.spec.MaxConcurrent <= 0 {
		return nil
	}
	active, err := counterValue(ctx, tx,
		`SELECT COUNT(*) FROM requests WHERE key_id = ? AND state = ?`, key.id, stateActive)
	if err != nil {
		return err
	}
	if active >= key.spec.MaxConcurrent {
		return fmt.Errorf("%w: %d active executions", ErrConcurrency, active)
	}
	return nil
}

// checkBacklog bounds unresolved requests per key (active + pending), which is
// what must be reconciled before more work is admitted.
func (s *Store) checkBacklog(ctx context.Context, tx *sql.Tx, key *keyRow) error {
	unresolved, err := counterValue(ctx, tx,
		`SELECT COUNT(*) FROM requests WHERE key_id = ? AND state IN (?,?)`,
		key.id, stateActive, statePending)
	if err != nil {
		return err
	}
	if unresolved >= int64(s.maxPending) {
		return fmt.Errorf("%w: %d unresolved requests for key", ErrPending, unresolved)
	}
	return nil
}

// checkTokenLimits enforces durable model token budgets using consumed tokens
// plus the admission estimates of unresolved requests.
func (s *Store) checkTokenLimits(ctx context.Context, tx *sql.Tx, key *keyRow, model string, estimate int64, at time.Time) error {
	var limit *ModelTokenLimit
	for i := range key.spec.ModelTokenLimits {
		if key.spec.ModelTokenLimits[i].Model == model {
			limit = &key.spec.ModelTokenLimits[i]
			break
		}
	}
	if limit == nil {
		return nil
	}
	windows := []struct {
		win   string
		limit int64
	}{
		{windowTotal, limit.Total},
		{windowDay, limit.Daily},
		{windowWeek, limit.Weekly},
		{windowMonth, limit.Monthly},
	}
	for _, w := range windows {
		if w.limit <= 0 {
			continue
		}
		// Estimates are period scoped: an unresolved request only occupies the
		// period it was admitted in, so stale work cannot starve the next one.
		reserved, err := reservedTokens(ctx, tx, key.id, model, w.win, at)
		if err != nil {
			return err
		}
		admit, err := checkedAdd(reserved, estimate)
		if err != nil {
			return err
		}
		used, err := tokenCounter(ctx, tx, key.id, model, w.win, periodOf(w.win, at))
		if err != nil {
			return err
		}
		// Effective token usage subtracts the operator reset baseline for this
		// window/period. Reserved estimates of unresolved requests are never
		// baselined, so a reset cannot forgive an unknown obligation.
		effective, err := effectiveToken(ctx, tx, key.id, model, w.win, periodOf(w.win, at), used)
		if err != nil {
			return err
		}
		total, err := checkedAdd(effective, admit)
		if err != nil {
			return err
		}
		if total > w.limit {
			return fmt.Errorf("%w: %s token limit reached for model %s", ErrQuota, w.win, model)
		}
	}
	return nil
}
