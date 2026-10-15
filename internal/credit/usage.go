package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type usageRow struct {
	id            string
	requestID     string
	traceID       string
	keyID         string
	provider      string
	authID        string
	authIndex     string
	executor      string
	model         string
	responseModel string
	quality       string
	failed        bool
	statusCode    int
	tokens        Tokens
	raw           pluginapi.UsageDetail
	cost          *int64
	charged       bool
	reliable      bool
	at            time.Time
	latencyMS     int64
	ttftMS        int64
}

func (u *usageRow) toUsage() Usage {
	out := Usage{
		ID:            u.id,
		RequestID:     u.requestID,
		TraceID:       u.traceID,
		KeyID:         u.keyID,
		Provider:      u.provider,
		AuthID:        u.authID,
		AuthIndex:     u.authIndex,
		Executor:      u.executor,
		Model:         u.model,
		ResponseModel: u.responseModel,
		Raw:           u.raw,
		Tokens:        u.tokens,
		Quality:       u.quality,
		Failed:        u.failed,
		StatusCode:    u.statusCode,
		At:            u.at,
		LatencyMS:     u.latencyMS,
		TTFTMS:        u.ttftMS,
	}
	if u.cost != nil {
		v := *u.cost
		out.CostMicroUSD = &v
	}
	return out
}

// material holds every caller-supplied field that must not silently change for
// a repeated usage identifier.
type usageMaterial struct {
	requestID, traceID, keyID                 string
	provider, authID, authIndex, executorName string
	model, responseModel, quality             string
	failed                                    bool
	statusCode                                int
	tokens                                    Tokens
	raw                                       pluginapi.UsageDetail
	at                                        int64
	latencyMS, ttftMS                         int64
}

func (u Usage) material() usageMaterial {
	return usageMaterial{
		requestID:     u.RequestID,
		traceID:       u.TraceID,
		keyID:         u.KeyID,
		provider:      u.Provider,
		authID:        u.AuthID,
		authIndex:     u.AuthIndex,
		executorName:  u.Executor,
		model:         u.Model,
		responseModel: u.ResponseModel,
		quality:       u.Quality,
		failed:        u.Failed,
		statusCode:    u.StatusCode,
		tokens:        u.Tokens,
		raw:           u.Raw,
		at:            unixNano(u.At),
		latencyMS:     u.LatencyMS,
		ttftMS:        u.TTFTMS,
	}
}

const usageColumns = `id, request_id, trace_id, key_id, provider, auth_id, auth_index, executor,
	model, response_model, quality, failed, status_code, tokens_input, tokens_cache_read,
	tokens_cache_write, tokens_output, tokens_reasoning, tokens_total, raw_input, raw_output,
	raw_reasoning, raw_cached, raw_cache_read, raw_cache_create, raw_total, cost_micro_usd,
	charged, reliable, at, latency_ms, ttft_ms`

func scanUsage(scan func(...any) error) (*usageRow, error) {
	var (
		u        usageRow
		failed   int
		charged  int
		reliable int
		at       int64
		cost     sql.NullInt64
	)
	if err := scan(&u.id, &u.requestID, &u.traceID, &u.keyID, &u.provider, &u.authID, &u.authIndex,
		&u.executor, &u.model, &u.responseModel, &u.quality, &failed, &u.statusCode,
		&u.tokens.Input, &u.tokens.CacheRead, &u.tokens.CacheWrite, &u.tokens.Output, &u.tokens.Reasoning,
		&u.tokens.Total, &u.raw.InputTokens, &u.raw.OutputTokens, &u.raw.ReasoningTokens,
		&u.raw.CachedTokens, &u.raw.CacheReadTokens, &u.raw.CacheCreationTokens, &u.raw.TotalTokens,
		&cost, &charged, &reliable, &at, &u.latencyMS, &u.ttftMS); err != nil {
		return nil, err
	}
	u.failed = failed != 0
	u.charged = charged != 0
	u.reliable = reliable != 0
	u.at = fromUnixNano(at)
	if cost.Valid {
		v := cost.Int64
		u.cost = &v
	}
	return &u, nil
}

func loadUsageByID(ctx context.Context, q querier, id string) (*usageRow, error) {
	row := q.QueryRowContext(ctx, `SELECT `+usageColumns+` FROM usages WHERE id = ?`, id)
	u, err := scanUsage(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load usage: %w", err)
	}
	return u, nil
}

// normalizeTokens validates canonical buckets, rejects overflow and derives a
// missing total. Canonical input is already uncached.
func normalizeTokens(t Tokens) (Tokens, error) {
	buckets := []int64{t.Input, t.CacheRead, t.CacheWrite, t.Output, t.Reasoning, t.Total}
	for _, v := range buckets {
		if v < 0 {
			return Tokens{}, fmt.Errorf("%w: token counters must not be negative", ErrInvalid)
		}
	}
	sum := int64(0)
	var err error
	for _, v := range []int64{t.Input, t.CacheRead, t.CacheWrite, t.Output, t.Reasoning} {
		if sum, err = checkedAdd(sum, v); err != nil {
			return Tokens{}, err
		}
	}
	if t.Total == 0 {
		t.Total = sum
	} else if t.Total != sum {
		return Tokens{}, fmt.Errorf("%w: token total %d does not match bucket sum %d", ErrInvalid, t.Total, sum)
	}
	return t, nil
}

func normalizeRaw(raw pluginapi.UsageDetail) (pluginapi.UsageDetail, error) {
	values := []int64{raw.InputTokens, raw.OutputTokens, raw.ReasoningTokens, raw.CachedTokens,
		raw.CacheReadTokens, raw.CacheCreationTokens, raw.TotalTokens}
	for _, v := range values {
		if v < 0 {
			return pluginapi.UsageDetail{}, fmt.Errorf("%w: raw token counters must not be negative", ErrInvalid)
		}
	}
	sum := int64(0)
	var err error
	for _, v := range values {
		if sum, err = checkedAdd(sum, v); err != nil {
			return pluginapi.UsageDetail{}, err
		}
	}
	return raw, nil
}

func normalizeQuality(q string) (string, error) {
	switch q {
	case "", QualityComplete, QualityPartial, QualityUnknown:
		return q, nil
	default:
		return "", fmt.Errorf("%w: unknown usage quality %q", ErrInvalid, q)
	}
}

// accountingReliable reports whether the usage carries trustworthy token
// accounting. Explicit quality wins; otherwise positive tokens are trusted and
// unknown/zero events are not.
func accountingReliable(u Usage) bool {
	switch u.Quality {
	case QualityUnknown, QualityPartial:
		return false
	case QualityComplete:
		return true
	default:
		return u.Tokens.Total > 0
	}
}

// RecordUsage attaches one execution event to a persisted request. Unknown or
// zero accounting is retained as unknown and never billed as a fake zero.
//
// The cost is always derived by the core from the request price snapshot; a
// caller supplied cost is rejected so quality and money can never disagree.
func (s *Store) RecordUsage(ctx context.Context, usage Usage) error {
	if strings.TrimSpace(usage.ID) == "" || strings.TrimSpace(usage.TraceID) == "" || strings.TrimSpace(usage.KeyID) == "" {
		return fmt.Errorf("%w: usage id, trace id and key id are required", ErrInvalid)
	}
	if usage.CostMicroUSD != nil {
		return fmt.Errorf("%w: usage cost is derived from the request price snapshot", ErrInvalid)
	}
	tokens, err := normalizeTokens(usage.Tokens)
	if err != nil {
		return err
	}
	usage.Tokens = tokens
	if usage.Raw, err = normalizeRaw(usage.Raw); err != nil {
		return err
	}
	if usage.Quality, err = normalizeQuality(usage.Quality); err != nil {
		return err
	}
	explicitAt := !usage.At.IsZero()
	at := s.resolveAt(usage.At)
	usage.At = at
	reliable := accountingReliable(usage)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		req, err := loadRequest(ctx, tx, "trace_id = ? AND key_id = ?", usage.TraceID, usage.KeyID)
		if err != nil {
			return err
		}
		if supplied := strings.TrimSpace(usage.RequestID); supplied != "" && supplied != req.id {
			return fmt.Errorf("%w: usage belongs to request %s", ErrConflict, req.id)
		}
		// Canonicalize the request id before comparison: the native bridge only
		// supplies a trace, so an empty id must still replay idempotently.
		usage.RequestID = req.id
		existing, err := loadUsageByID(ctx, tx, usage.ID)
		switch {
		case err == nil:
			candidate := usage
			if !explicitAt {
				// The caller omitted the timestamp, so a replay must compare
				// against the stored instant instead of the wall clock that
				// moved on since the original event.
				candidate.At = existing.at
			}
			if existing.materialEqual(candidate) {
				return nil // idempotent replay
			}
			return fmt.Errorf("%w: usage id reused with different content", ErrConflict)
		case errors.Is(err, ErrNotFound):
		default:
			return err
		}
		var cost *int64
		if reliable {
			v, err := computeCost(req.price, tokens)
			if err != nil {
				return err
			}
			cost = &v
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usages (`+usageColumns+`) VALUES
			(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			usage.ID, req.id, usage.TraceID, usage.KeyID, usage.Provider, usage.AuthID, usage.AuthIndex,
			usage.Executor, usage.Model, usage.ResponseModel, usage.Quality, boolInt(usage.Failed),
			usage.StatusCode, tokens.Input, tokens.CacheRead, tokens.CacheWrite, tokens.Output,
			tokens.Reasoning, tokens.Total, usage.Raw.InputTokens, usage.Raw.OutputTokens,
			usage.Raw.ReasoningTokens, usage.Raw.CachedTokens, usage.Raw.CacheReadTokens,
			usage.Raw.CacheCreationTokens, usage.Raw.TotalTokens, cost, 0, boolInt(reliable),
			unixNano(at), usage.LatencyMS, usage.TTFTMS); err != nil {
			return fmt.Errorf("insert usage: %w", err)
		}
		if reliable {
			// Consumed tokens count against admission budgets immediately.
			if err := addTokens(ctx, tx, req.keyID, req.model, req.startedAt, tokens.Total); err != nil {
				return err
			}
		}
		// A trustworthy cost is booked in the same transaction that records it,
		// even while the request stays open, so a later write-off can never
		// discard a real charge.
		if cost != nil {
			if err := s.applyCharge(ctx, tx, req, usage.ID, *cost); err != nil {
				return err
			}
		}
		if cost == nil && req.state == stateSettled && req.resolved == resolvedAuto {
			// Untrustworthy accounting arrived after the request was closed
			// automatically: reopen it so the work stays visible until an
			// administrator decides. A manually resolved request is left
			// closed because an administrator already ruled on it.
			if err := s.reopenRequest(ctx, tx, req); err != nil {
				return err
			}
		}
		if req.state == statePending {
			return s.evaluateSettle(ctx, tx, req, at)
		}
		return nil
	})
}

func (u *usageRow) materialEqual(in Usage) bool {
	return usageMaterial{
		requestID:     u.requestID,
		traceID:       u.traceID,
		keyID:         u.keyID,
		provider:      u.provider,
		authID:        u.authID,
		authIndex:     u.authIndex,
		executorName:  u.executor,
		model:         u.model,
		responseModel: u.responseModel,
		quality:       u.quality,
		failed:        u.failed,
		statusCode:    u.statusCode,
		tokens:        u.tokens,
		raw:           u.raw,
		at:            unixNano(u.at),
		latencyMS:     u.latencyMS,
		ttftMS:        u.ttftMS,
	} == in.material()
}

// Complete records a terminal lifecycle event. Concurrency is released once and
// the monetary hold is closed only for trustworthy accounting.
func (s *Store) Complete(ctx context.Context, c Completion) error {
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("%w: completion id is required", ErrInvalid)
	}
	switch c.Outcome {
	case OutcomeSucceeded, OutcomeFailed, OutcomeRejected, OutcomeCanceled:
	default:
		return fmt.Errorf("%w: unsupported outcome %q", ErrInvalid, c.Outcome)
	}
	at := s.resolveAt(c.At)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		req, err := loadRequestByID(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if strings.TrimSpace(c.TraceID) != "" && c.TraceID != req.traceID {
			return fmt.Errorf("%w: completion trace does not match request", ErrConflict)
		}
		if req.state == stateSettled && req.resolved != resolvedAuto {
			// An administrator already closed this request. A late completion
			// may record its outcome metadata once, but it must never reopen
			// the request or touch the released hold.
			if req.completionSeen {
				return fmt.Errorf("%w: request was already closed by %s and its completion was recorded",
					ErrConflict, req.resolved)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE requests SET outcome = ?, status_code = ?,
				completion_seen = 1 WHERE id = ?`, c.Outcome, c.StatusCode, req.id); err != nil {
				return fmt.Errorf("record late completion: %w", err)
			}
			return nil
		}
		if req.outcome != "" {
			// A repeated completion must agree, otherwise the ledger would
			// silently change what was already recorded.
			if req.outcome != c.Outcome || req.statusCode != c.StatusCode {
				return fmt.Errorf("%w: request already completed as %s/%d",
					ErrConflict, req.outcome, req.statusCode)
			}
			if req.state == stateSettled {
				return nil
			}
			// Still unresolved: a late usage may have made it closable.
			return s.evaluateSettle(ctx, tx, req, at)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET state = ?, outcome = ?, status_code = ?, completed_at = ?
			WHERE id = ?`, statePending, c.Outcome, c.StatusCode, unixNano(at), req.id); err != nil {
			return fmt.Errorf("complete request: %w", err)
		}
		req.state = statePending
		req.outcome = c.Outcome
		req.statusCode = c.StatusCode
		req.completedAt = &at
		return s.evaluateSettle(ctx, tx, req, at)
	})
}
