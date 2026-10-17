package credit

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Audit actions. They are the stable vocabulary of the management audit trail.
const (
	ActionCreate      = "create"
	ActionUpdate      = "update"
	ActionRotate      = "rotate"
	ActionReveal      = "reveal"
	ActionDelete      = "delete"
	ActionReset       = "reset"
	ActionCallerNew   = "caller_create"
	ActionCallerEdit  = "caller_update"
	ActionPriceUpdate = "price_update"
	ActionRuleUpdate  = "rule_update"
	ActionRuleDelete  = "rule_delete"
)

// materialKeyDomain separates the key-material encryption key from the key
// verifier. The same pepper seeds two independent HMACs, so neither derivation
// can be used to forge the other.
const materialKeyDomain = "credit-manager:key-material:v1\x00"

// Caller is an attribution record. It carries operator metadata only: enabling
// or disabling a caller never changes a key's identity, scope, authentication
// or scheduling, and it has no quota of its own.
type Caller struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CallerSpec is the caller create/update payload. An empty ID is generated.
type CallerSpec struct {
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
}

// ResetSpec describes a token usage reset. Exactly one scope form (KeyID, IDs,
// All) and at least one window must be selected, and a non-empty reason is
// required.
//
// A reset only reclaims token headroom: it never deletes history, never zeroes a
// durable token counter and never touches price, logs or derived cost. It stores
// a baseline snapshot per model/window so the effective token usage an operator
// sees and admission enforces becomes max(0, durable - baseline). Unresolved
// requests are untouched, so late usage still counts.
type ResetSpec struct {
	KeyID   string   `json:"id,omitempty"`
	IDs     []string `json:"ids,omitempty"`
	All     bool     `json:"all,omitempty"`
	Total   bool     `json:"total,omitempty"`
	Daily   bool     `json:"daily,omitempty"`
	Weekly  bool     `json:"weekly,omitempty"`
	Monthly bool     `json:"monthly,omitempty"`
	Reason  string   `json:"reason"`
}

// ResetResult reports what a reset touched.
type ResetResult struct {
	Keys    int      `json:"keys"`
	Windows []string `json:"windows"`
}

// AuditEvent is one immutable management action.
type AuditEvent struct {
	Seq      int64     `json:"seq"`
	At       time.Time `json:"at"`
	Action   string    `json:"action"`
	KeyID    string    `json:"key_id,omitempty"`
	CallerID string    `json:"caller_id,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

// AuditFilter selects and pages the audit trail. Limit is a shorthand for a
// first page of that size; otherwise Page/PageSize are used, both bounded.
type AuditFilter struct {
	KeyID    string
	CallerID string
	Page     int
	PageSize int
	Limit    int
}

// AuditPage is one page of the audit trail, newest first.
type AuditPage struct {
	Items    []AuditEvent `json:"items"`
	Total    int64        `json:"total"`
	Page     int          `json:"page"`
	PageSize int          `json:"page_size"`
}

// materialEncryptionKey derives a 32 byte AES-256 key from the store pepper in
// a domain separated way, so the encryption key and the verifier HMAC are
// cryptographically independent even though both come from the pepper.
func materialEncryptionKey(pepper []byte) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(materialKeyDomain))
	return mac.Sum(nil)
}

// encryptMaterial seals plaintext with AES-256-GCM. The returned blob is
// nonce||ciphertext||tag and contains no plaintext.
func encryptMaterial(pepper []byte, material string) ([]byte, error) {
	block, err := aes.NewCipher(materialEncryptionKey(pepper))
	if err != nil {
		return nil, fmt.Errorf("material cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("material cipher: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := readRandom(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(material), nil), nil
}

// decryptMaterial opens a blob produced by encryptMaterial. A different pepper,
// a truncated blob or any tampering fails the GCM tag check.
func decryptMaterial(pepper []byte, blob []byte) (string, error) {
	block, err := aes.NewCipher(materialEncryptionKey(pepper))
	if err != nil {
		return "", fmt.Errorf("material cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("material cipher: %w", err)
	}
	if len(blob) < gcm.NonceSize() {
		return "", fmt.Errorf("%w: stored key material is truncated", ErrConflict)
	}
	nonce, ciphertext := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("stored key material failed verification")
	}
	return string(plain), nil
}

// insertAudit appends one audit row inside the caller transaction. Audit rows
// are written in the same transaction as the action they describe, so a rolled
// back action never leaves a phantom event and a committed action always has
// one. Detail never carries key material.
func insertAudit(ctx context.Context, q querier, at time.Time, action, keyID, callerID, reason, detail string) error {
	if _, err := q.ExecContext(ctx, `INSERT INTO audit_events(at, action, key_id, caller_id, reason, detail)
		VALUES(?,?,?,?,?,?)`, unixNano(at), action, keyID, callerID, reason, detail); err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return nil
}

// requireCaller validates an optional caller attribution. An empty id is
// always allowed; a non-empty id must name an existing caller.
func requireCaller(ctx context.Context, q querier, callerID string) error {
	if callerID == "" {
		return nil
	}
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM callers WHERE id = ?`, callerID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: unknown caller %s", ErrInvalid, callerID)
	}
	if err != nil {
		return fmt.Errorf("load caller: %w", err)
	}
	return nil
}

// RevealKey returns the recoverable plaintext material of one key after proving
// it still matches the stored verifier. The stored blob is authenticated, so a
// corrupted one is refused instead of being decrypted into a wrong value, and
// no plaintext is ever fabricated. Every successful disclosure is audited
// without the material itself.
func (s *Store) RevealKey(ctx context.Context, id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("%w: key id is required", ErrInvalid)
	}
	var material string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		key, err := loadKey(ctx, tx, "id = ?", id)
		if err != nil {
			return err
		}
		if key.deletedAt != nil {
			return fmt.Errorf("%w: key is deleted", ErrConflict)
		}
		plain, err := decryptMaterial(s.pepper, key.materialEnc)
		if err != nil {
			return err
		}
		if materialHMAC(s.pepper, plain) != key.hmac {
			return fmt.Errorf("reveal key: stored material failed verification")
		}
		material = plain
		// The audit row is written only after the ciphertext proved both
		// openable and consistent, so a corrupt blob leaves no audit trail.
		return insertAudit(ctx, tx, s.nowUTC(), ActionReveal, key.id, key.callerID, "", "")
	})
	if err != nil {
		return "", err
	}
	return material, nil
}

// DeleteKey soft deletes a key: it is disabled and permanently ineligible for
// re-enable, rotation or authentication, while its records, ledger counters and
// unresolved requests are preserved so in flight work still settles. Deleting
// an already deleted key is an idempotent no-op.
func (s *Store) DeleteKey(ctx context.Context, id, reason string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: key id is required", ErrInvalid)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: deletion reason is required", ErrInvalid)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		key, err := loadKey(ctx, tx, "id = ?", id)
		if err != nil {
			return err
		}
		if key.deletedAt != nil {
			return nil
		}
		now := s.nowUTC()
		if _, err := tx.ExecContext(ctx, `UPDATE keys SET deleted_at = ?, enabled = 0 WHERE id = ?`,
			unixNano(now), key.id); err != nil {
			return fmt.Errorf("delete key: %w", err)
		}
		return insertAudit(ctx, tx, now, ActionDelete, key.id, key.callerID, reason, "")
	})
}

// ResetTokens snapshots token reset baselines for the selected keys and
// windows. It touches neither history nor unresolved requests: the durable token
// counters keep their value and only the effective usage is reduced, so later
// usage counts again and repeated resets can never go negative or refund. No
// amount, price or log row is read or written.
func (s *Store) ResetTokens(ctx context.Context, spec ResetSpec) (ResetResult, error) {
	reason := strings.TrimSpace(spec.Reason)
	if reason == "" {
		return ResetResult{}, fmt.Errorf("%w: reset reason is required", ErrInvalid)
	}
	windows := selectedWindows(spec)
	if len(windows) == 0 {
		return ResetResult{}, fmt.Errorf("%w: select at least one window to reset", ErrInvalid)
	}
	ids, all, err := normalizeResetScope(spec)
	if err != nil {
		return ResetResult{}, err
	}
	var out ResetResult
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowUTC()
		if all {
			if ids, err = s.allKeyIDs(ctx, tx); err != nil {
				return err
			}
		}
		for _, id := range ids {
			key, err := loadKey(ctx, tx, "id = ?", id)
			if err != nil {
				return err
			}
			for _, win := range windows {
				if err := snapshotTokenBaseline(ctx, tx, key, win, now); err != nil {
					return err
				}
			}
			detail := "tokens windows=" + strings.Join(windows, ",")
			if err := insertAudit(ctx, tx, now, ActionReset, key.id, key.callerID, reason, detail); err != nil {
				return err
			}
			out.Keys++
		}
		out.Windows = windows
		return nil
	})
	if err != nil {
		return ResetResult{}, err
	}
	return out, nil
}

// selectedWindows returns the requested windows in a stable order.
func selectedWindows(spec ResetSpec) []string {
	var out []string
	if spec.Total {
		out = append(out, windowTotal)
	}
	if spec.Daily {
		out = append(out, windowDay)
	}
	if spec.Weekly {
		out = append(out, windowWeek)
	}
	if spec.Monthly {
		out = append(out, windowMonth)
	}
	return out
}

// normalizeResetScope resolves the scope form to a unique, stable key id list.
// all reports that every key should be resolved inside the transaction.
func normalizeResetScope(spec ResetSpec) ([]string, bool, error) {
	forms := 0
	if strings.TrimSpace(spec.KeyID) != "" {
		forms++
	}
	if len(spec.IDs) > 0 {
		forms++
	}
	if spec.All {
		forms++
	}
	if forms == 0 {
		return nil, false, fmt.Errorf("%w: reset needs a key id, explicit ids or all", ErrInvalid)
	}
	if forms > 1 {
		return nil, false, fmt.Errorf("%w: reset scope must be exactly one of key id, ids or all", ErrInvalid)
	}
	if spec.All {
		return nil, true, nil
	}
	if len(spec.IDs) > 0 {
		seen := map[string]struct{}{}
		ids := make([]string, 0, len(spec.IDs))
		for _, raw := range spec.IDs {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return nil, false, fmt.Errorf("%w: reset ids are empty", ErrInvalid)
		}
		return ids, false, nil
	}
	return []string{strings.TrimSpace(spec.KeyID)}, false, nil
}

// allKeyIDs resolves the "all keys" scope inside the transaction.
func (s *Store) allKeyIDs(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM keys ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan key id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// snapshotTokenBaseline records the durable token counters of one key and
// window as the new reset baseline, per model, so each model token budget resets
// independently. A snapshot never lowers an existing baseline, so a repeated
// reset can only raise the floor and never refunds usage.
func snapshotTokenBaseline(ctx context.Context, q querier, key *keyRow, win string, at time.Time) error {
	period := periodOf(win, at)
	models, err := tokenModels(ctx, q, key.id)
	if err != nil {
		return err
	}
	for _, model := range models {
		used, err := tokenCounter(ctx, q, key.id, model, win, period)
		if err != nil {
			return err
		}
		if err := raiseTokenBaseline(ctx, q, key.id, model, win, period, used); err != nil {
			return err
		}
	}
	return nil
}

// tokenModels lists every model that has durable token counters for a key, plus
// every model the key has a configured limit for, so a configured budget with
// no usage yet also gets a coherent baseline row.
func tokenModels(ctx context.Context, q querier, keyID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT model FROM key_token_counters WHERE key_id = ?
		UNION SELECT model FROM model_token_limits WHERE key_id = ? ORDER BY model`, keyID, keyID)
	if err != nil {
		return nil, fmt.Errorf("list token models: %w", err)
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, fmt.Errorf("scan token model: %w", err)
		}
		models = append(models, model)
	}
	return models, rows.Err()
}

// raiseTokenBaseline stores max(existing, value) as the token baseline.
func raiseTokenBaseline(ctx context.Context, q querier, keyID, model, win, period string, value int64) error {
	cur, err := baselineToken(ctx, q, keyID, model, win, period)
	if err != nil {
		return err
	}
	next := max(cur, value)
	if _, err := q.ExecContext(ctx, `INSERT INTO key_token_baselines(key_id, model, win, period, tokens)
		VALUES(?,?,?,?,?)
		ON CONFLICT(key_id, model, win, period) DO UPDATE SET tokens = excluded.tokens`,
		keyID, model, win, period, next); err != nil {
		return fmt.Errorf("update token baseline: %w", err)
	}
	return nil
}

// ListCallers returns every caller record in a stable order.
func (s *Store) ListCallers(ctx context.Context) ([]Caller, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, display_name, enabled, created_at, updated_at
		FROM callers ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list callers: %w", err)
	}
	defer rows.Close()
	out := []Caller{}
	for rows.Next() {
		c, err := scanCaller(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate callers: %w", err)
	}
	return out, nil
}

// CreateCaller registers a caller record. An empty id is generated. A caller is
// attribution metadata only.
func (s *Store) CreateCaller(ctx context.Context, spec CallerSpec) (Caller, error) {
	name := strings.TrimSpace(spec.DisplayName)
	if name == "" {
		return Caller{}, fmt.Errorf("%w: caller display name is required", ErrInvalid)
	}
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		generated, err := newUUID()
		if err != nil {
			return Caller{}, err
		}
		id = generated
	}
	now := s.nowUTC()
	var out Caller
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT id FROM callers WHERE id = ?`, id).Scan(&existing)
		if err == nil {
			return fmt.Errorf("%w: caller %s already exists", ErrConflict, id)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load caller: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO callers(id, display_name, enabled, created_at, updated_at)
			VALUES(?,?,?,?,?)`, id, name, boolInt(spec.Enabled), unixNano(now), unixNano(now)); err != nil {
			return fmt.Errorf("insert caller: %w", err)
		}
		if err := insertAudit(ctx, tx, now, ActionCallerNew, "", id, "", "display_name="+name); err != nil {
			return err
		}
		out = Caller{ID: id, DisplayName: name, Enabled: spec.Enabled, CreatedAt: now, UpdatedAt: now}
		return nil
	})
	if err != nil {
		return Caller{}, err
	}
	return out, nil
}

// UpdateCaller updates a caller's mutable metadata. Disabling a caller is
// metadata only and never affects any key's identity or authentication.
func (s *Store) UpdateCaller(ctx context.Context, spec CallerSpec) (Caller, error) {
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		return Caller{}, fmt.Errorf("%w: caller id is required", ErrInvalid)
	}
	name := strings.TrimSpace(spec.DisplayName)
	if name == "" {
		return Caller{}, fmt.Errorf("%w: caller display name is required", ErrInvalid)
	}
	now := s.nowUTC()
	var out Caller
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var createdAt int64
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM callers WHERE id = ?`, id).Scan(&createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: caller %s", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("load caller: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE callers SET display_name = ?, enabled = ?, updated_at = ?
			WHERE id = ?`, name, boolInt(spec.Enabled), unixNano(now), id); err != nil {
			return fmt.Errorf("update caller: %w", err)
		}
		if err := insertAudit(ctx, tx, now, ActionCallerEdit, "", id, "", "display_name="+name); err != nil {
			return err
		}
		out = Caller{ID: id, DisplayName: name, Enabled: spec.Enabled, CreatedAt: fromUnixNano(createdAt), UpdatedAt: now}
		return nil
	})
	if err != nil {
		return Caller{}, err
	}
	return out, nil
}

func scanCaller(scan func(...any) error) (Caller, error) {
	var (
		c                Caller
		enabled          int
		created, updated int64
	)
	if err := scan(&c.ID, &c.DisplayName, &enabled, &created, &updated); err != nil {
		return Caller{}, fmt.Errorf("scan caller: %w", err)
	}
	c.Enabled = enabled != 0
	c.CreatedAt = fromUnixNano(created)
	c.UpdatedAt = fromUnixNano(updated)
	return c, nil
}

// Audit returns one page of the audit trail, newest first, filtered by key
// and/or caller. Paging is bounded so a caller cannot request an unbounded or
// overflowing page.
func (s *Store) Audit(ctx context.Context, f AuditFilter) (AuditPage, error) {
	page, size := normalizeAuditPaging(f)
	offset, err := pageOffset(page, size)
	if err != nil {
		return AuditPage{}, err
	}
	where := "1 = 1"
	var args []any
	if id := strings.TrimSpace(f.KeyID); id != "" {
		where += " AND key_id = ?"
		args = append(args, id)
	}
	if id := strings.TrimSpace(f.CallerID); id != "" {
		where += " AND caller_id = ?"
		args = append(args, id)
	}
	var out AuditPage
	err = s.withSnapshot(ctx, func(q querier) error {
		total, err := counterValue(ctx, q, `SELECT COUNT(*) FROM audit_events WHERE `+where, args...)
		if err != nil {
			return err
		}
		rows, err := q.QueryContext(ctx, `SELECT seq, at, action, key_id, caller_id, reason, detail
			FROM audit_events WHERE `+where+` ORDER BY seq DESC LIMIT ? OFFSET ?`,
			append(append([]any{}, args...), size, offset)...)
		if err != nil {
			return fmt.Errorf("list audit: %w", err)
		}
		defer rows.Close()
		items := []AuditEvent{}
		for rows.Next() {
			var (
				e  AuditEvent
				at int64
			)
			if err := rows.Scan(&e.Seq, &at, &e.Action, &e.KeyID, &e.CallerID, &e.Reason, &e.Detail); err != nil {
				return fmt.Errorf("scan audit: %w", err)
			}
			e.At = fromUnixNano(at)
			items = append(items, e)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate audit: %w", err)
		}
		out = AuditPage{Items: items, Total: total, Page: page, PageSize: size}
		return nil
	})
	if err != nil {
		return AuditPage{}, err
	}
	return out, nil
}

// normalizeAuditPaging maps Limit or Page/PageSize to a bounded page.
func normalizeAuditPaging(f AuditFilter) (int, int) {
	if f.Limit > 0 {
		size := min(f.Limit, maxPageSize)
		return 1, size
	}
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
