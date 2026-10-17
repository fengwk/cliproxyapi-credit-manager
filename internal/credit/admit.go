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
// trace/key binding and the request-time service tier fact before any execution
// starts.
//
// Admission is a post-usage gate: it checks only the key's permissions and the
// effective usage already consumed. It reads no price and computes no amount, so
// a missing price, a disabled pricing rule, an exhausted budget figure or an
// unrepresentable rate can never block a request. Concurrent in-flight requests
// may legitimately exceed any limit.
func (s *Store) Admit(ctx context.Context, in AdmitInput) (Request, error) {
	if strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.TraceID) == "" ||
		strings.TrimSpace(in.KeyID) == "" || strings.TrimSpace(in.RequestedModel) == "" {
		return Request{}, fmt.Errorf("%w: id, trace id, key id and requested model are required", ErrInvalid)
	}
	at := s.resolveAt(in.At)
	serviceTier := normalizeServiceTier(in.ServiceTier)
	var out Request
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		existing, err := loadRequestByID(ctx, tx, in.ID)
		switch {
		case err == nil:
			if existing.traceID != in.TraceID || existing.keyID != in.KeyID || existing.model != in.RequestedModel {
				return fmt.Errorf("%w: lifecycle id already bound to another request", ErrConflict)
			}
			out = existing.toRequest(nil)
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
		// A soft deleted key must never admit new work.
		if key.deletedAt != nil {
			return ErrUnauthorized
		}
		if !modelAllowed(key.spec.AllowedModels, in.RequestedModel) {
			return fmt.Errorf("%w: model %s is not allowed for this key", ErrDenied, in.RequestedModel)
		}
		if err := s.checkConcurrency(ctx, tx, key); err != nil {
			return err
		}
		if err := s.checkTokenLimits(ctx, tx, key, in.RequestedModel, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO requests
			(id, trace_id, key_id, model, state, outcome, status_code, started_at, completed_at, service_tier)
			VALUES(?,?,?,?,?,'',0,?,NULL,?)`,
			in.ID, in.TraceID, in.KeyID, in.RequestedModel, stateActive, unixNano(at),
			serviceTier); err != nil {
			return fmt.Errorf("insert request: %w", err)
		}
		out = (&reqRow{
			id:          in.ID,
			traceID:     in.TraceID,
			keyID:       in.KeyID,
			model:       in.RequestedModel,
			state:       stateActive,
			startedAt:   at,
			serviceTier: serviceTier,
		}).toRequest(nil)
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	return out, nil
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
