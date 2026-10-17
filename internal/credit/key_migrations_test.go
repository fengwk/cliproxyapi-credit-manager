package credit

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// openLegacyRaw opens the ledger file of dir directly, bypassing Store so a test
// can inspect or mutate the on-disk state without triggering the migration.
func openLegacyRaw(t *testing.T, dir string) *sql.DB {
	t.Helper()
	raw, err := sql.Open(driverName, dsnFor(filepath.Join(dir, dbFileName)))
	if err != nil {
		t.Fatalf("open raw ledger: %v", err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

// readMetaVersion reads the recorded schema marker through any querier.
func readMetaVersion(t *testing.T, q querier) string {
	t.Helper()
	var v string
	if err := q.QueryRowContext(t.Context(), `SELECT v FROM meta WHERE k = 'schema_version'`).Scan(&v); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return v
}

// A ledger written by a newer or corrupt schema version must be refused before
// any destructive step, so its marker is never silently downgraded and its
// estimate/hold columns and real spend stay untouched.
func TestMigrationRejectsUnsupportedSchemaVersion(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"future", "3"},
		{"far future", "99"},
		{"zero", "0"},
		{"corrupt", "not-a-version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seedV1Ledger(t, dir)
			raw := openLegacyRaw(t, dir)
			if _, err := raw.Exec(`INSERT INTO meta(k, v) VALUES('schema_version', ?)`, tc.value); err != nil {
				t.Fatalf("stamp version: %v", err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			s, err := Open(Options{DataDir: dir, Now: newClock(testEpoch).now})
			if s != nil {
				_ = s.Close()
			}
			if !errors.Is(err, ErrSchemaVersion) {
				t.Fatalf("open err = %v, want ErrSchemaVersion", err)
			}

			// The refused open must not have altered the ledger at all.
			raw = openLegacyRaw(t, dir)
			if v := readMetaVersion(t, raw); v != tc.value {
				t.Fatalf("schema_version = %q, want it left at %q", v, tc.value)
			}
			columns, err := tableColumns(t.Context(), raw, "keys")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := columns["held_micro_usd"]; !ok {
				t.Fatal("keys.held_micro_usd was dropped despite the version refusal")
			}
			var spent int64
			if err := raw.QueryRow(`SELECT spent_micro_usd FROM keys WHERE id = 'legacy'`).Scan(&spent); err != nil || spent != 500 {
				t.Fatalf("spent = %d (%v), want 500 preserved", spent, err)
			}
		})
	}
}

// Missing (original v1) and the supported 1/2 markers all migrate in place and
// end up stamped as the current version.
func TestMigrationAcceptsSupportedSchemaVersions(t *testing.T) {
	for _, version := range []string{"", "1", "2"} {
		name := version
		if name == "" {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			seedV1Ledger(t, dir)
			if version != "" {
				raw := openLegacyRaw(t, dir)
				if _, err := raw.Exec(`INSERT INTO meta(k, v) VALUES('schema_version', ?)`, version); err != nil {
					t.Fatalf("stamp version: %v", err)
				}
				if err := raw.Close(); err != nil {
					t.Fatal(err)
				}
			}
			s, err := Open(Options{DataDir: dir, Now: newClock(testEpoch).now})
			if err != nil {
				t.Fatalf("open supported version %q: %v", version, err)
			}
			defer s.Close()
			assertHoldColumnsDropped(t, s)
			if v := readMetaVersion(t, s.db); v != "2" {
				t.Fatalf("schema_version = %q, want 2 after migration", v)
			}
			if k, err := s.GetKey(t.Context(), "legacy"); err != nil || k.SpentMicroUSD != 500 {
				t.Fatalf("spent after migration = %+v %v", k, err)
			}
		})
	}
}

// A store created by the current build already carries real usage charges, a
// frozen price snapshot, encrypted key material, reset baselines and audit
// history. Forcing it back to version 1 with the legacy estimate/hold columns
// proves the migration preserves those capabilities, which the original v1
// fixture (holding none of them) cannot cover.
func TestMigrationPreservesPopulatedAlignedV1Store(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx()
	h.mustPrice("m1", Rates{Input: million, Output: 2 * million})
	const material = "sk-aligned-material-0123456789"
	key, _ := h.mustKey(KeySpec{Label: "aligned", Enabled: true, Material: material})
	h.mustRule(PricingRule{ID: "rule-aligned", MatchKind: MatchExact, Pattern: "m1", Priority: 5,
		Enabled: true, Price: Price{Rates: Rates{Input: million, Output: 2 * million}}})
	req := h.mustAdmit(AdmitInput{ID: "req-aligned", TraceID: "trace-aligned",
		KeyID: key.ID, RequestedModel: "m1"})
	h.mustUsage(usageFor("u-aligned", req, "m1", tokens(100, 0, 0, 100, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if _, err := h.store.ResetSpend(ctx, ResetSpec{KeyID: key.ID, Total: true, Daily: true,
		Reason: "aligned baseline"}); err != nil {
		t.Fatal(err)
	}

	// Capture the real accounting before the migration.
	keyBefore := mustLoadKeyRow(t, h.store.db, key.ID)
	reqBefore := mustLoadRequestRow(t, h.store.db, req.ID)
	blobBefore := keyMaterialBlob(t, h.store.db, key.ID)
	baselinesBefore := countRows(t, h.store.db, `SELECT COUNT(*) FROM key_money_baselines WHERE key_id = ?`, key.ID)
	auditBefore := countRows(t, h.store.db, `SELECT COUNT(*) FROM audit_events`)
	usageBefore := countRows(t, h.store.db, `SELECT COUNT(*) FROM usages WHERE request_id = ? AND charged = 1`, req.ID)
	if keyBefore.spent == 0 || reqBefore.snapshot == nil || len(blobBefore) == 0 || baselinesBefore == 0 || auditBefore == 0 {
		t.Fatalf("fixture is not populated: spent=%d snapshot=%v blob=%d baselines=%d audit=%d",
			keyBefore.spent, reqBefore.snapshot, len(blobBefore), baselinesBefore, auditBefore)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	// Add the legacy columns back and stamp the store as version 1 so Open has
	// to run the destructive migration over populated data.
	raw := openLegacyRaw(t, h.dir)
	for _, stmt := range []string{
		`ALTER TABLE keys ADD COLUMN held_micro_usd INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN held_micro_usd INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN token_estimate INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN initial_hold_micro_usd INTEGER NOT NULL DEFAULT 0`,
		`INSERT INTO meta(k, v) VALUES('schema_version', '1')
		 ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("recreate legacy column: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{DataDir: h.dir, Now: h.clock.now})
	if err != nil {
		t.Fatalf("reopen aligned store: %v", err)
	}
	defer reopened.Close()
	assertHoldColumnsDropped(t, reopened)
	if v := readMetaVersion(t, reopened.db); v != "2" {
		t.Fatalf("schema_version = %q, want 2", v)
	}

	// Real money, the execution and its frozen snapshot survive untouched.
	if got := mustLoadKeyRow(t, reopened.db, key.ID); got.spent != keyBefore.spent {
		t.Fatalf("spent = %d, want %d", got.spent, keyBefore.spent)
	}
	if got := mustLoadRequestRow(t, reopened.db, req.ID); got.charge != reqBefore.charge ||
		got.state != stateSettled || got.snapshot == nil || got.snapshot.RuleID != "rule-aligned" {
		t.Fatalf("request after migration = %+v", got)
	}
	if got := countRows(t, reopened.db, `SELECT COUNT(*) FROM usages WHERE request_id = ? AND charged = 1`, req.ID); got != usageBefore {
		t.Fatalf("charged executions = %d, want %d", got, usageBefore)
	}
	// Reset baselines and audit history survive, and the reset still hides the
	// charge from the effective quota view.
	if got := countRows(t, reopened.db, `SELECT COUNT(*) FROM key_money_baselines WHERE key_id = ?`, key.ID); got != baselinesBefore {
		t.Fatalf("money baselines = %d, want %d", got, baselinesBefore)
	}
	if got := countRows(t, reopened.db, `SELECT COUNT(*) FROM audit_events`); got != auditBefore {
		t.Fatalf("audit events = %d, want %d", got, auditBefore)
	}
	if k, err := reopened.GetKey(ctx, key.ID); err != nil || k.QuotaUsedMicroUSD != 0 {
		t.Fatalf("effective usage after migration = %+v %v, want the baseline honoured", k, err)
	}
	// The sealed key material is byte-identical and still decrypts across the
	// migration, so the pepper/ciphertext pair was not disturbed.
	if got := keyMaterialBlob(t, reopened.db, key.ID); !bytes.Equal(got, blobBefore) {
		t.Fatalf("key ciphertext changed: %d bytes -> %d bytes", len(blobBefore), len(got))
	}
	revealed, err := reopened.RevealKey(ctx, key.ID)
	if err != nil || revealed != material {
		t.Fatalf("reveal after migration = %q %v, want the original material", revealed, err)
	}
	if _, err := reopened.Authenticate(ctx, material); err != nil {
		t.Fatalf("authenticate after migration: %v", err)
	}
}

// A DROP COLUMN cannot run while an index depends on the column. That failure
// must roll the whole migration transaction back: the drops that already ran
// are undone, the additive columns are not left behind, and the real spend,
// the schema marker and the legacy columns all survive.
func TestMigrationDropFailureRollsBackWholeMigration(t *testing.T) {
	dir := t.TempDir()
	seedV1Ledger(t, dir)
	raw := openLegacyRaw(t, dir)
	for _, stmt := range []string{
		`CREATE INDEX legacy_token_estimate ON requests(token_estimate)`,
		`INSERT INTO meta(k, v) VALUES('schema_version', '1')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("prepare failing migration: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(Options{DataDir: dir, Now: newClock(testEpoch).now})
	if s != nil {
		_ = s.Close()
	}
	if err == nil {
		t.Fatal("open succeeded although a dependent index blocks a DROP COLUMN")
	}
	if !strings.Contains(err.Error(), "drop requests.token_estimate") {
		t.Fatalf("open err = %v, want the blocked drop failure", err)
	}

	// Everything must be exactly as before the failed migration.
	raw = openLegacyRaw(t, dir)
	keyColumns, err := tableColumns(t.Context(), raw, "keys")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keyColumns["held_micro_usd"]; !ok {
		t.Fatal("keys.held_micro_usd drop was not rolled back")
	}
	if _, ok := keyColumns["key_material_enc"]; ok {
		t.Fatal("additive key feature column survived the failed migration")
	}
	reqColumns, err := tableColumns(t.Context(), raw, "requests")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"held_micro_usd", "token_estimate", "initial_hold_micro_usd"} {
		if _, ok := reqColumns[col]; !ok {
			t.Fatalf("requests.%s missing after rollback", col)
		}
	}
	if _, ok := reqColumns["price_snapshot"]; ok {
		t.Fatal("additive pricing column survived the failed migration")
	}
	if v := readMetaVersion(t, raw); v != "1" {
		t.Fatalf("schema_version = %q, want the original 1", v)
	}
	var spent int64
	if err := raw.QueryRow(`SELECT spent_micro_usd FROM keys WHERE id = 'legacy'`).Scan(&spent); err != nil || spent != 500 {
		t.Fatalf("spent = %d (%v), want 500 preserved", spent, err)
	}
	var indexes int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'legacy_token_estimate'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("blocking index = %d (%v), want it kept", indexes, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Removing the blocker lets the very same store migrate cleanly.
	raw = openLegacyRaw(t, dir)
	if _, err := raw.Exec(`DROP INDEX legacy_token_estimate`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{DataDir: dir, Now: newClock(testEpoch).now})
	if err != nil {
		t.Fatalf("open after removing the blocker: %v", err)
	}
	defer reopened.Close()
	assertHoldColumnsDropped(t, reopened)
	if k, err := reopened.GetKey(t.Context(), "legacy"); err != nil || k.SpentMicroUSD != 500 {
		t.Fatalf("spent after successful migration = %+v %v", k, err)
	}
}

// mustLoadKeyRow loads a key row through the store's own reader, kept as a helper
// so the aligned migration test can compare rows across a reopen.
func mustLoadKeyRow(t *testing.T, q querier, id string) *keyRow {
	t.Helper()
	k, err := loadKey(t.Context(), q, "id = ?", id)
	if err != nil {
		t.Fatalf("load key %s: %v", id, err)
	}
	return k
}

// mustLoadRequestRow loads a request row through the store's own reader.
func mustLoadRequestRow(t *testing.T, q querier, id string) *reqRow {
	t.Helper()
	r, err := loadRequestByID(t.Context(), q, id)
	if err != nil {
		t.Fatalf("load request %s: %v", id, err)
	}
	return r
}

func keyMaterialBlob(t *testing.T, q querier, id string) []byte {
	t.Helper()
	var blob []byte
	if err := q.QueryRowContext(t.Context(), `SELECT key_material_enc FROM keys WHERE id = ?`, id).Scan(&blob); err != nil {
		t.Fatalf("read key material blob: %v", err)
	}
	return blob
}

func countRows(t *testing.T, q querier, query string, args ...any) int64 {
	t.Helper()
	n, err := counterValue(t.Context(), q, query, args...)
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}
