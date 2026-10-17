package credit

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// legacyV1DDL is the original estimated-admission schema: keys carry a money
// hold and requests carry a hold, an initial hold and a token estimate. It has
// none of the key management (key_material_enc/caller_id/deleted_at) or pricing
// feature columns, so seeding it exercises both the additive and the removal
// migrations on a real populated store.
const legacyV1DDL = `
CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS keys (
  id TEXT PRIMARY KEY,
  principal TEXT NOT NULL UNIQUE,
  scope TEXT NOT NULL UNIQUE,
  fingerprint TEXT NOT NULL,
  hmac TEXT NOT NULL UNIQUE,
  label TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  quota_micro_usd INTEGER NOT NULL DEFAULT 0,
  daily_micro_usd INTEGER NOT NULL DEFAULT 0,
  weekly_micro_usd INTEGER NOT NULL DEFAULT 0,
  monthly_micro_usd INTEGER NOT NULL DEFAULT 0,
  max_concurrent INTEGER NOT NULL DEFAULT 0,
  allowed_models TEXT NOT NULL DEFAULT '[]',
  expires_at INTEGER,
  created_at INTEGER NOT NULL,
  spent_micro_usd INTEGER NOT NULL DEFAULT 0,
  held_micro_usd INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS model_token_limits (
  key_id TEXT NOT NULL,
  model TEXT NOT NULL,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  daily_tokens INTEGER NOT NULL DEFAULT 0,
  weekly_tokens INTEGER NOT NULL DEFAULT 0,
  monthly_tokens INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (key_id, model)
);
CREATE TABLE IF NOT EXISTS prices (
  model TEXT PRIMARY KEY,
  input_rate INTEGER NOT NULL,
  output_rate INTEGER NOT NULL,
  reasoning_rate INTEGER,
  cache_read_rate INTEGER NOT NULL,
  cache_write_rate INTEGER NOT NULL,
  version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS requests (
  id TEXT PRIMARY KEY,
  trace_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  model TEXT NOT NULL,
  state TEXT NOT NULL,
  outcome TEXT NOT NULL DEFAULT '',
  status_code INTEGER NOT NULL DEFAULT 0,
  started_at INTEGER NOT NULL,
  completed_at INTEGER,
  held_micro_usd INTEGER NOT NULL,
  price_input INTEGER NOT NULL,
  price_output INTEGER NOT NULL,
  price_reasoning INTEGER,
  price_cache_read INTEGER NOT NULL,
  price_cache_write INTEGER NOT NULL,
  price_version INTEGER NOT NULL,
  token_estimate INTEGER NOT NULL DEFAULT 0,
  charge_micro_usd INTEGER NOT NULL DEFAULT 0,
  resolved TEXT NOT NULL DEFAULT '',
  initial_hold_micro_usd INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS requests_trace ON requests(trace_id);
CREATE INDEX IF NOT EXISTS requests_key_state ON requests(key_id, state);
CREATE TABLE IF NOT EXISTS usages (
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL,
  trace_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  auth_id TEXT NOT NULL DEFAULT '',
  auth_index TEXT NOT NULL DEFAULT '',
  executor TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  response_model TEXT NOT NULL DEFAULT '',
  quality TEXT NOT NULL DEFAULT '',
  failed INTEGER NOT NULL DEFAULT 0,
  status_code INTEGER NOT NULL DEFAULT 0,
  tokens_input INTEGER NOT NULL,
  tokens_cache_read INTEGER NOT NULL,
  tokens_cache_write INTEGER NOT NULL,
  tokens_output INTEGER NOT NULL,
  tokens_reasoning INTEGER NOT NULL,
  tokens_total INTEGER NOT NULL,
  raw_input INTEGER NOT NULL,
  raw_output INTEGER NOT NULL,
  raw_reasoning INTEGER NOT NULL,
  raw_cached INTEGER NOT NULL,
  raw_cache_read INTEGER NOT NULL,
  raw_cache_create INTEGER NOT NULL,
  raw_total INTEGER NOT NULL,
  cost_micro_usd INTEGER,
  charged INTEGER NOT NULL DEFAULT 0,
  reliable INTEGER NOT NULL DEFAULT 0,
  at INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  ttft_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS key_money_counters (
  key_id TEXT NOT NULL,
  win TEXT NOT NULL,
  period TEXT NOT NULL,
  micro_usd INTEGER NOT NULL,
  PRIMARY KEY (key_id, win, period)
);
CREATE TABLE IF NOT EXISTS key_token_counters (
  key_id TEXT NOT NULL,
  model TEXT NOT NULL,
  win TEXT NOT NULL,
  period TEXT NOT NULL,
  tokens INTEGER NOT NULL,
  PRIMARY KEY (key_id, model, win, period)
);
CREATE TABLE IF NOT EXISTS adjustments (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  action TEXT NOT NULL,
  amount_micro_usd INTEGER NOT NULL,
  reason TEXT NOT NULL,
  at INTEGER NOT NULL
);
`

// seedV1Ledger writes a populated ledger using only the legacy schema. It proves
// the migration both adds the new columns and removes the estimate/hold columns
// on a real existing store.
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
	if _, err := raw.Exec(legacyV1DDL); err != nil {
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
			VALUES('req-legacy','trace-legacy','legacy','m1','pending','',0,?,NULL,25,1000000,0,NULL,0,0,1,50,0,'',25)`} {
		if _, err := raw.Exec(stmt, unixNano(testEpoch)); err != nil {
			t.Fatalf("seed v1 row: %v", err)
		}
	}
}

// legacyHoldColumns are the estimate/hold columns the post-usage migration must
// remove from a legacy store.
var legacyHoldColumns = []struct {
	table  string
	column string
}{
	{"keys", "held_micro_usd"},
	{"requests", "held_micro_usd"},
	{"requests", "token_estimate"},
	{"requests", "initial_hold_micro_usd"},
}

func assertHoldColumnsDropped(t *testing.T, s *Store) {
	t.Helper()
	for _, probe := range legacyHoldColumns {
		columns, err := tableColumns(t.Context(), s.db, probe.table)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := columns[probe.column]; ok {
			t.Fatalf("legacy column %s.%s survived the migration", probe.table, probe.column)
		}
	}
}

// A brand new store must never create an estimate or hold column at all: the
// post-usage schema has no such state, so a fresh ledger cannot report or
// restore one.
func TestFreshSchemaHasNoHoldColumns(t *testing.T) {
	h := newHarness(t)
	assertHoldColumnsDropped(t, h.store)
}

// Opening a populated v1 ledger migrates it in place, preserving every actual
// counter and dropping the estimate/hold columns, and reopening the migrated
// store is idempotent.
func TestMigrationPreservesPopulatedV1Ledger(t *testing.T) {
	dir := t.TempDir()
	seedV1Ledger(t, dir)
	clock := newClock(testEpoch)
	ctx := t.Context()

	s, err := Open(Options{DataDir: dir, Now: clock.now})
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	// The new columns are present; the old estimate/hold columns are gone.
	columns, err := tableColumns(ctx, s.db, "keys")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"key_material_enc", "caller_id", "deleted_at"} {
		if _, ok := columns[name]; !ok {
			t.Fatalf("column %s missing after migration", name)
		}
	}
	assertHoldColumnsDropped(t, s)
	var version string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'schema_version'`).Scan(&version); err != nil || version != "2" {
		t.Fatalf("schema version = %q %v, want 2", version, err)
	}
	key, err := s.GetKey(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	// The durable lifetime charge is preserved exactly; the legacy hold is not
	// money and must not leak into any ledger total.
	if key.SpentMicroUSD != 500 || key.Spec.Label != "legacy label" ||
		!key.Spec.Enabled || key.CallerID != "" || key.DeletedAt != nil {
		t.Fatalf("v1 key not preserved: %+v", key)
	}
	if key.QuotaUsedMicroUSD != 500 {
		t.Fatalf("effective usage without a baseline = %d, want 500", key.QuotaUsedMicroUSD)
	}
	if key.Scope != callerScope(key.Principal) {
		t.Fatalf("v1 scope changed: %+v", key)
	}
	// The legacy request survives with its frozen price and charge intact.
	legacy, err := loadRequestByID(ctx, s.db, "req-legacy")
	if err != nil {
		t.Fatalf("legacy request lost: %v", err)
	}
	if legacy.model != "m1" || legacy.state != statePending || legacy.charge != 0 || legacy.price.Rates.Input != 1_000_000 {
		t.Fatalf("legacy request not preserved: %+v", legacy)
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
		assertHoldColumnsDropped(t, reopened)
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

// A late execution attributed to a legacy pending request must settle from the
// stored rate columns and add its real charge to the preserved lifetime total,
// exactly as it would on a store that never carried estimates.
func TestMigrationSettlesLateUsageOnLegacyRequest(t *testing.T) {
	dir := t.TempDir()
	seedV1Ledger(t, dir)
	clock := newClock(testEpoch)
	ctx := t.Context()

	s, err := Open(Options{DataDir: dir, Now: clock.now})
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer s.Close()
	if err := s.RecordUsage(ctx, Usage{ID: "u-late", TraceID: "trace-legacy", KeyID: "legacy",
		Model: "m1", Tokens: tokens(10, 0, 0, 0, 0), Quality: QualityComplete}); err != nil {
		t.Fatalf("late usage: %v", err)
	}
	if err := s.Complete(ctx, Completion{ID: "req-legacy", TraceID: "trace-legacy",
		Outcome: OutcomeSucceeded}); err != nil {
		t.Fatalf("late completion: %v", err)
	}
	key, err := s.GetKey(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	// 500 of preserved history plus the 10 microUSD of the late execution.
	if key.SpentMicroUSD != 510 {
		t.Fatalf("late settlement spent = %d, want 510", key.SpentMicroUSD)
	}
	req, err := loadRequestByID(ctx, s.db, "req-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if req.state != stateSettled || req.charge != 10 {
		t.Fatalf("legacy request after late settlement = %+v", req)
	}
}
