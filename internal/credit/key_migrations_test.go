package credit

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedV1Ledger writes a populated ledger using only the original v1 schema: no
// key_material_enc/caller_id/deleted_at columns and none of the key management
// tables. It proves the migration is additive on a real existing store.
func seedV1Ledger(t *testing.T, dir string) {
	t.Helper()
	pepper := make([]byte, pepperLen)
	if _, err := readRandom(pepper); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pepperFileName), pepper, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(driverName, dsnFor(filepath.Join(dir, dbFileName)))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(schemaDDL); err != nil {
		t.Fatalf("seed v1 schema: %v", err)
	}
	material := "sk-legacy-material-1234567890"
	hmac := materialHMAC(pepper, material)
	principal := principalPrefix + "legacy-principal"
	if _, err := raw.Exec(`INSERT INTO keys (id, principal, scope, fingerprint, hmac, label, enabled,
		quota_micro_usd, daily_micro_usd, weekly_micro_usd, monthly_micro_usd, max_concurrent,
		allowed_models, expires_at, created_at, spent_micro_usd, held_micro_usd)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"legacy", principal, callerScope(principal), fingerprintOf(hmac), hmac,
		"legacy label", 1, 0, 0, 0, 0, 0, "[]", nil, unixNano(testEpoch), 500, 25); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO key_money_counters(key_id, win, period, micro_usd) VALUES('legacy','total','',500)`,
		`INSERT INTO key_money_counters(key_id, win, period, micro_usd) VALUES('legacy','month','2026-06',500)`,
		`INSERT INTO key_token_counters(key_id, model, win, period, tokens) VALUES('legacy','m1','total','',42)`,
		`INSERT INTO requests(id, trace_id, key_id, model, state, outcome, status_code, started_at,
			completed_at, held_micro_usd, price_input, price_output, price_reasoning, price_cache_read,
			price_cache_write, price_version, token_estimate, charge_micro_usd, resolved, initial_hold_micro_usd)
			VALUES('req-legacy','trace-legacy','legacy','m1','pending','',0,?,NULL,25,1000000,0,NULL,0,0,1,50,0,'',25)`,
	} {
		if _, err := raw.Exec(stmt, unixNano(testEpoch)); err != nil {
			t.Fatalf("seed v1 row: %v", err)
		}
	}
}

// Opening a populated v1 ledger migrates it in place without rewriting any
// existing row, and reopening the migrated store is idempotent.
func TestMigrationPreservesPopulatedV1Ledger(t *testing.T) {
	dir := t.TempDir()
	seedV1Ledger(t, dir)
	clock := newClock(testEpoch)
	ctx := t.Context()

	s, err := Open(Options{DataDir: dir, Now: clock.now})
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	// The new columns are present; the old data is untouched.
	columns, err := tableColumns(ctx, s.db, "keys")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"key_material_enc", "caller_id", "deleted_at"} {
		if _, ok := columns[name]; !ok {
			t.Fatalf("column %s missing after migration", name)
		}
	}
	key, err := s.GetKey(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if key.SpentMicroUSD != 500 || key.HeldMicroUSD != 25 || key.Spec.Label != "legacy label" ||
		!key.Spec.Enabled || key.CallerID != "" || key.DeletedAt != nil {
		t.Fatalf("v1 key not preserved: %+v", key)
	}
	if key.QuotaUsedMicroUSD != 500 {
		t.Fatalf("effective usage without a baseline = %d, want 500", key.QuotaUsedMicroUSD)
	}
	if key.Scope != callerScope(key.Principal) {
		t.Fatalf("v1 scope changed: %+v", key)
	}
	// Existing material still verifies, but has no recoverable copy: reveal must
	// report a conflict and never fabricate a plaintext.
	if _, err := s.Authenticate(ctx, "sk-legacy-material-1234567890"); err != nil {
		t.Fatalf("v1 material rejected: %v", err)
	}
	if _, err := s.RevealKey(ctx, "legacy"); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy reveal err = %v, want conflict", err)
	}
	// Migration fabricates no audit history.
	page, err := s.Audit(ctx, AuditFilter{})
	if err != nil || page.Total != 0 {
		t.Fatalf("migration wrote audit: %+v %v", page, err)
	}
	var total int64
	if err := s.db.QueryRow(`SELECT micro_usd FROM key_money_counters WHERE key_id='legacy' AND win='total'`).Scan(&total); err != nil || total != 500 {
		t.Fatalf("lifetime counter not preserved: %d %v", total, err)
	}
	// The new tables are usable on the migrated store.
	caller, err := s.CreateCaller(ctx, CallerSpec{DisplayName: "Acme", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetSpend(ctx, ResetSpec{KeyID: "legacy", Total: true, Reason: "migrated"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateKey(ctx, "legacy", KeySpec{Enabled: true, CallerID: caller.ID, Label: "updated"}); err != nil {
		t.Fatal(err)
	}
	updated, err := s.GetKey(ctx, "legacy")
	if err != nil || updated.CallerID != caller.ID || updated.Spec.Label != "updated" || updated.QuotaUsedMicroUSD != 0 {
		t.Fatalf("post migration update = %+v %v", updated, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening a freshly migrated store must be a no-op and preserve the new
	// state.
	for range 2 {
		reopened, err := Open(Options{DataDir: dir, Now: clock.now})
		if err != nil {
			t.Fatalf("reopen migrated store: %v", err)
		}
		got, err := reopened.GetKey(ctx, "legacy")
		if err != nil || got.CallerID != caller.ID || got.SpentMicroUSD != 500 || got.QuotaUsedMicroUSD != 0 {
			t.Fatalf("reopened state = %+v %v", got, err)
		}
		callers, err := reopened.ListCallers(ctx)
		if err != nil || len(callers) != 1 {
			t.Fatalf("callers after reopen = %+v %v", callers, err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
