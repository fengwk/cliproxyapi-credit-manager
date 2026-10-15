package credit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type keyRow struct {
	id          string
	principal   string
	scope       string
	fingerprint string
	hmac        string
	spec        KeySpec
	limits      []ModelTokenLimit
	createdAt   time.Time
	spent       int64
	held        int64
}

func (k *keyRow) toKey() Key {
	spec := k.spec
	spec.Material = "" // material must never leave the store
	spec.ModelTokenLimits = slices.Clone(k.spec.ModelTokenLimits)
	return Key{
		ID:            k.id,
		Principal:     k.principal,
		Scope:         k.scope,
		Fingerprint:   k.fingerprint,
		Spec:          spec,
		SpentMicroUSD: k.spent,
		HeldMicroUSD:  k.held,
		CreatedAt:     k.createdAt,
	}
}

type keyColumnQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const keyColumns = `id, principal, scope, fingerprint, hmac, label, enabled, quota_micro_usd,
	daily_micro_usd, weekly_micro_usd, monthly_micro_usd, max_concurrent, allowed_models,
	expires_at, created_at, spent_micro_usd, held_micro_usd`

func scanKeyRow(scan func(...any) error) (*keyRow, error) {
	var (
		k          keyRow
		label      string
		enabled    int
		allowedRaw string
		expires    sql.NullInt64
		created    int64
	)
	if err := scan(&k.id, &k.principal, &k.scope, &k.fingerprint, &k.hmac, &label, &enabled,
		&k.spec.QuotaMicroUSD, &k.spec.DailyMicroUSD, &k.spec.WeeklyMicroUSD, &k.spec.MonthlyMicroUSD,
		&k.spec.MaxConcurrent, &allowedRaw, &expires, &created, &k.spent, &k.held); err != nil {
		return nil, err
	}
	k.spec.Label = label
	k.spec.Enabled = enabled != 0
	k.spec.ExpiresAt = scanNano(expires)
	if err := json.Unmarshal([]byte(allowedRaw), &k.spec.AllowedModels); err != nil {
		return nil, fmt.Errorf("%w: corrupt allowed_models for key %s", ErrInvalid, k.id)
	}
	k.createdAt = fromUnixNano(created)
	return &k, nil
}

func loadKey(ctx context.Context, q keyColumnQuerier, where string, args ...any) (*keyRow, error) {
	row := q.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM keys WHERE `+where, args...)
	k, err := scanKeyRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load key: %w", err)
	}
	return k, nil
}

// limitsQuerier is the minimal surface used to hydrate model token limits.
type limitsQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadLimits(ctx context.Context, q limitsQuerier, keyID string) ([]ModelTokenLimit, error) {
	rows, err := q.QueryContext(ctx, `SELECT model, total_tokens, daily_tokens, weekly_tokens, monthly_tokens
		FROM model_token_limits WHERE key_id = ? ORDER BY model`, keyID)
	if err != nil {
		return nil, fmt.Errorf("load token limits: %w", err)
	}
	defer rows.Close()
	var out []ModelTokenLimit
	for rows.Next() {
		var l ModelTokenLimit
		if err := rows.Scan(&l.Model, &l.Total, &l.Daily, &l.Weekly, &l.Monthly); err != nil {
			return nil, fmt.Errorf("scan token limit: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// hydrateLimits fills ModelTokenLimits using the provided querier.
func hydrateLimits(ctx context.Context, q limitsQuerier, k *keyRow) error {
	limits, err := loadLimits(ctx, q, k.id)
	if err != nil {
		return err
	}
	k.limits = limits
	k.spec.ModelTokenLimits = slices.Clone(limits)
	return nil
}

// normalizeSpec validates a caller supplied key spec and returns a normalized copy.
func normalizeSpec(spec KeySpec) (KeySpec, error) {
	out := spec
	out.Material = ""
	if spec.QuotaMicroUSD < 0 || spec.DailyMicroUSD < 0 || spec.WeeklyMicroUSD < 0 || spec.MonthlyMicroUSD < 0 {
		return out, fmt.Errorf("%w: monetary limits must not be negative", ErrInvalid)
	}
	if spec.MaxConcurrent < 0 {
		return out, fmt.Errorf("%w: max concurrency must not be negative", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(spec.AllowedModels))
	models := make([]string, 0, len(spec.AllowedModels))
	for _, m := range spec.AllowedModels {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if strings.Contains(m, "*") {
			return out, fmt.Errorf("%w: allowed models are exact identifiers, wildcards are unsupported", ErrInvalid)
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		models = append(models, m)
	}
	if len(models) == 0 {
		out.AllowedModels = nil
	} else {
		out.AllowedModels = models
	}
	limitSeen := make(map[string]struct{}, len(spec.ModelTokenLimits))
	limits := make([]ModelTokenLimit, 0, len(spec.ModelTokenLimits))
	for _, l := range spec.ModelTokenLimits {
		l.Model = strings.TrimSpace(l.Model)
		if l.Model == "" {
			return out, fmt.Errorf("%w: token limit requires a model", ErrInvalid)
		}
		if l.Total < 0 || l.Daily < 0 || l.Weekly < 0 || l.Monthly < 0 {
			return out, fmt.Errorf("%w: token limits must not be negative", ErrInvalid)
		}
		if _, dup := limitSeen[l.Model]; dup {
			return out, fmt.Errorf("%w: duplicate token limit for model %s", ErrInvalid, l.Model)
		}
		limitSeen[l.Model] = struct{}{}
		limits = append(limits, l)
	}
	out.ModelTokenLimits = limits
	if out.ExpiresAt != nil {
		e := out.ExpiresAt.UTC()
		out.ExpiresAt = &e
	}
	return out, nil
}

func marshalAllowedModels(models []string) (string, error) {
	if len(models) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(models)
	if err != nil {
		return "", fmt.Errorf("encode allowed models: %w", err)
	}
	return string(b), nil
}

func modelAllowed(models []string, model string) bool {
	if len(models) == 0 {
		return true
	}
	return slices.Contains(models, model)
}

func (s *Store) insertLimits(ctx context.Context, q querier, keyID string, limits []ModelTokenLimit) error {
	for _, l := range limits {
		if _, err := q.ExecContext(ctx, `INSERT INTO model_token_limits
			(key_id, model, total_tokens, daily_tokens, weekly_tokens, monthly_tokens)
			VALUES(?,?,?,?,?,?)`, keyID, l.Model, l.Total, l.Daily, l.Weekly, l.Monthly); err != nil {
			return fmt.Errorf("insert token limit: %w", err)
		}
	}
	return nil
}

// checkMaterialFree fails when another key already owns this material.
func checkMaterialFree(ctx context.Context, q querier, hmac string, exceptID string) error {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM keys WHERE hmac = ?`, hmac).Scan(&id)
	if err == nil && id != exceptID {
		return fmt.Errorf("%w: key material already registered", ErrConflict)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check key material: %w", err)
	}
	return nil
}

// CreateKey registers a key. The returned string is the only copy of the
// plaintext material; it is never persisted or recoverable.
func (s *Store) CreateKey(ctx context.Context, spec KeySpec) (Key, string, error) {
	norm, err := normalizeSpec(spec)
	if err != nil {
		return Key{}, "", err
	}
	material := strings.TrimSpace(spec.Material)
	if material == "" {
		if material, err = newMaterial(); err != nil {
			return Key{}, "", err
		}
	} else if len(material) < 8 {
		return Key{}, "", fmt.Errorf("%w: key material is too short", ErrInvalid)
	}
	allowed, err := marshalAllowedModels(norm.AllowedModels)
	if err != nil {
		return Key{}, "", err
	}
	id, err := newUUID()
	if err != nil {
		return Key{}, "", err
	}
	principal, err := newUUID()
	if err != nil {
		return Key{}, "", err
	}
	principalValue := principalPrefix + principal
	// The scope must equal the host's trusted session.CallerScope(principal),
	// otherwise the native layer can never resolve a request back to this key.
	scope := callerScope(principalValue)
	hmac := materialHMAC(s.pepper, material)
	now := s.nowUTC()
	var out Key
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if err := checkMaterialFree(ctx, tx, hmac, ""); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO keys
			(id, principal, scope, fingerprint, hmac, label, enabled, quota_micro_usd,
			 daily_micro_usd, weekly_micro_usd, monthly_micro_usd, max_concurrent,
			 allowed_models, expires_at, created_at, spent_micro_usd, held_micro_usd)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,0)`,
			id, principalValue, scope, fingerprintOf(hmac), hmac,
			norm.Label, boolInt(norm.Enabled), norm.QuotaMicroUSD, norm.DailyMicroUSD, norm.WeeklyMicroUSD,
			norm.MonthlyMicroUSD, norm.MaxConcurrent, allowed, nullableNano(norm.ExpiresAt), unixNano(now)); err != nil {
			return fmt.Errorf("insert key: %w", err)
		}
		if err := s.insertLimits(ctx, tx, id, norm.ModelTokenLimits); err != nil {
			return err
		}
		out = Key{
			ID:          id,
			Principal:   principalValue,
			Scope:       scope,
			Fingerprint: fingerprintOf(hmac),
			Spec:        norm,
			CreatedAt:   now,
		}
		return nil
	})
	if err != nil {
		return Key{}, "", err
	}
	return out, material, nil
}

// UpdateKey replaces the mutable part of a key spec. Key material is not part
// of an update: rotate it through RotateKey so ownership changes stay explicit.
func (s *Store) UpdateKey(ctx context.Context, id string, spec KeySpec) (Key, error) {
	if strings.TrimSpace(id) == "" {
		return Key{}, fmt.Errorf("%w: key id is required", ErrInvalid)
	}
	if strings.TrimSpace(spec.Material) != "" {
		return Key{}, fmt.Errorf("%w: use RotateKey to change key material", ErrInvalid)
	}
	norm, err := normalizeSpec(spec)
	if err != nil {
		return Key{}, err
	}
	allowed, err := marshalAllowedModels(norm.AllowedModels)
	if err != nil {
		return Key{}, err
	}
	var out Key
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		key, err := loadKey(ctx, tx, "id = ?", id)
		if err != nil {
			return err
		}
		if err := hydrateLimits(ctx, tx, key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE keys SET label=?, enabled=?, quota_micro_usd=?,
			daily_micro_usd=?, weekly_micro_usd=?, monthly_micro_usd=?, max_concurrent=?,
			allowed_models=?, expires_at=? WHERE id=?`,
			norm.Label, boolInt(norm.Enabled), norm.QuotaMicroUSD, norm.DailyMicroUSD, norm.WeeklyMicroUSD,
			norm.MonthlyMicroUSD, norm.MaxConcurrent, allowed, nullableNano(norm.ExpiresAt), key.id); err != nil {
			return fmt.Errorf("update key: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM model_token_limits WHERE key_id = ?`, key.id); err != nil {
			return fmt.Errorf("reset token limits: %w", err)
		}
		if err := s.insertLimits(ctx, tx, key.id, norm.ModelTokenLimits); err != nil {
			return err
		}
		key.spec = norm
		out = key.toKey()
		return nil
	})
	if err != nil {
		return Key{}, err
	}
	return out, nil
}

// RotateKey issues fresh material for an existing key. Rotating onto the
// current material is rejected as a conflict.
func (s *Store) RotateKey(ctx context.Context, id string, custom string) (Key, string, error) {
	if strings.TrimSpace(id) == "" {
		return Key{}, "", fmt.Errorf("%w: key id is required", ErrInvalid)
	}
	material := strings.TrimSpace(custom)
	var err error
	if material == "" {
		if material, err = newMaterial(); err != nil {
			return Key{}, "", err
		}
	}
	hmac := materialHMAC(s.pepper, material)
	var out Key
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		key, err := loadKey(ctx, tx, "id = ?", id)
		if err != nil {
			return err
		}
		if err := hydrateLimits(ctx, tx, key); err != nil {
			return err
		}
		if key.hmac == hmac {
			return fmt.Errorf("%w: new material matches the current material", ErrConflict)
		}
		if err := checkMaterialFree(ctx, tx, hmac, key.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE keys SET hmac=?, fingerprint=? WHERE id=?`,
			hmac, fingerprintOf(hmac), key.id); err != nil {
			return fmt.Errorf("rotate key: %w", err)
		}
		key.hmac = hmac
		key.fingerprint = fingerprintOf(hmac)
		out = key.toKey()
		return nil
	})
	if err != nil {
		return Key{}, "", err
	}
	return out, material, nil
}

func (s *Store) ListKeys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM keys ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()
	all, err := scanAllKeys(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Key, 0, len(all))
	for _, k := range all {
		if err := hydrateLimits(ctx, s.db, k); err != nil {
			return nil, err
		}
		out = append(out, k.toKey())
	}
	return out, nil
}

func scanAllKeys(rows *sql.Rows) ([]*keyRow, error) {
	var out []*keyRow
	for rows.Next() {
		k, err := scanKeyRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate keys: %w", err)
	}
	return out, nil
}

func (s *Store) GetKey(ctx context.Context, id string) (Key, error) {
	if strings.TrimSpace(id) == "" {
		return Key{}, fmt.Errorf("%w: key id is required", ErrInvalid)
	}
	key, err := loadKey(ctx, s.db, "id = ?", id)
	if err != nil {
		return Key{}, err
	}
	if err := hydrateLimits(ctx, s.db, key); err != nil {
		return Key{}, err
	}
	return key.toKey(), nil
}

// Authenticate verifies raw key material and rejects disabled or expired keys.
func (s *Store) Authenticate(ctx context.Context, material string) (Key, error) {
	if strings.TrimSpace(material) == "" {
		return Key{}, ErrUnauthorized
	}
	hmac := materialHMAC(s.pepper, material)
	key, err := loadKey(ctx, s.db, "hmac = ?", hmac)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Key{}, ErrUnauthorized
		}
		return Key{}, err
	}
	if err := hydrateLimits(ctx, s.db, key); err != nil {
		return Key{}, err
	}
	if !key.spec.Enabled {
		return Key{}, ErrUnauthorized
	}
	if key.spec.ExpiresAt != nil && !s.nowUTC().Before(*key.spec.ExpiresAt) {
		return Key{}, ErrUnauthorized
	}
	return key.toKey(), nil
}

func (s *Store) KeyByScope(ctx context.Context, scope string) (Key, error) {
	if strings.TrimSpace(scope) == "" {
		return Key{}, fmt.Errorf("%w: scope is required", ErrInvalid)
	}
	key, err := loadKey(ctx, s.db, "scope = ?", scope)
	if err != nil {
		return Key{}, err
	}
	if err := hydrateLimits(ctx, s.db, key); err != nil {
		return Key{}, err
	}
	return key.toKey(), nil
}

func (s *Store) KeyByPrincipal(ctx context.Context, principal string) (Key, error) {
	if strings.TrimSpace(principal) == "" {
		return Key{}, fmt.Errorf("%w: principal is required", ErrInvalid)
	}
	key, err := loadKey(ctx, s.db, "principal = ?", principal)
	if err != nil {
		return Key{}, err
	}
	if err := hydrateLimits(ctx, s.db, key); err != nil {
		return Key{}, err
	}
	return key.toKey(), nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
