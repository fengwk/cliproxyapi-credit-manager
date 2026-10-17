package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Admit admits one execution. It is idempotent by lifecycle ID and persists the
// trace/key binding and frozen price snapshot before any execution starts.
//
// Admission is a post-usage gate: it only checks key permissions, the
// requested-model price and the effective usage already consumed. It never
// estimates what the request will cost, so a positive remaining quota admits a
// very large declared output limit or input payload, and concurrent in-flight
// requests may legitimately exceed a quota.
func (s *Store) Admit(ctx context.Context, in AdmitInput) (Request, error) {
	if strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.TraceID) == "" ||
		strings.TrimSpace(in.KeyID) == "" || strings.TrimSpace(in.RequestedModel) == "" {
		return Request{}, fmt.Errorf("%w: id, trace id, key id and requested model are required", ErrInvalid)
	}
	at := s.resolveAt(in.At)
	var out Request
	err := s.inTx(ctx, func(tx *sql.Tx) error {
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
		price, tiers, ruleID, err := resolvePricing(ctx, tx, in.RequestedModel)
		if err != nil {
			return err
		}
		// Only a per-image price cares about the requested image count, and only
		// then is an unsupported explicit count refused: a token request may
		// legitimately carry an image hint that is irrelevant to its billing.
		images := int64(0)
		if price.IsPerImage() {
			if images, err = imageCountFor(in); err != nil {
				return err
			}
		}
		if err := s.checkMoneyLimits(ctx, tx, key, at); err != nil {
			return err
		}
		if err := s.checkConcurrency(ctx, tx, key); err != nil {
			return err
		}
		if err := s.checkTokenLimits(ctx, tx, key, in.RequestedModel, at); err != nil {
			return err
		}
		snapshot, err := encodePriceSnapshot(priceSnapshot{
			Price:       price,
			Tiers:       tiers,
			RuleID:      ruleID,
			ServiceTier: normalizeServiceTier(in.ServiceTier),
			ImageCount:  images,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO requests
			(id, trace_id, key_id, model, state, outcome, status_code, started_at, completed_at,
			 price_input, price_output, price_reasoning, price_cache_read,
			 price_cache_write, price_version, charge_micro_usd, resolved, price_snapshot)
			VALUES(?,?,?,?,?,'',0,?,NULL,?,?,?,?,?,?,0,'',?)`,
			in.ID, in.TraceID, in.KeyID, in.RequestedModel, stateActive, unixNano(at),
			price.Rates.Input, price.Rates.Output, price.Rates.Reasoning, price.Rates.CacheRead,
			price.Rates.CacheWrite, price.Version, snapshot); err != nil {
			return fmt.Errorf("insert request: %w", err)
		}
		out = Request{
			ID:         in.ID,
			TraceID:    in.TraceID,
			KeyID:      in.KeyID,
			Model:      in.RequestedModel,
			State:      stateActive,
			StartedAt:  at,
			Price:      price,
			Executions: []Usage{},
		}
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	return out, nil
}

// checkMoneyLimits enforces the soft monetary admission limits of a key.
//
// Every configured limit > 0 rejects the next request once the actual effective
// usage has reached it. Effective usage is the durable counter minus the
// operator reset baseline; no in-flight prediction is added. A key with a
// positive remaining quota therefore always admits, however large the payload.
func (s *Store) checkMoneyLimits(ctx context.Context, tx *sql.Tx, key *keyRow, at time.Time) error {
	// A soft deleted key must never admit new work. This is the single
	// admission gate for deleted keys: money limits are always evaluated, even
	// when no monetary limit is configured, before any execution is admitted.
	if key.deletedAt != nil {
		return ErrUnauthorized
	}
	if quota := key.spec.QuotaMicroUSD; quota > 0 {
		effective, err := effectiveMoney(ctx, tx, key.id, windowTotal, periodOf(windowTotal, at), key.spent)
		if err != nil {
			return err
		}
		if effective >= quota {
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
		effective, err := effectiveMoney(ctx, tx, key.id, l.win, periodOf(l.win, at), raw)
		if err != nil {
			return err
		}
		if effective >= l.limit {
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

// checkTokenLimits enforces the soft model token admission limits of a key
// against already-consumed effective tokens only.
func (s *Store) checkTokenLimits(ctx context.Context, tx *sql.Tx, key *keyRow, model string, at time.Time) error {
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
		used, err := tokenCounter(ctx, tx, key.id, model, w.win, periodOf(w.win, at))
		if err != nil {
			return err
		}
		// Effective token usage subtracts the operator reset baseline for this
		// window/period, so a reset restores headroom without erasing history.
		effective, err := effectiveToken(ctx, tx, key.id, model, w.win, periodOf(w.win, at), used)
		if err != nil {
			return err
		}
		if effective >= w.limit {
			return fmt.Errorf("%w: %s token limit reached for model %s", ErrQuota, w.win, model)
		}
	}
	return nil
}
