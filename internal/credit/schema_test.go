package credit

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// schemaTables is the complete table set of the current format. It is asserted
// as an exact set so a forgotten table fails the test, not just a missing
// feature column.
var schemaTables = []string{
	"adjustments",
	"audit_events",
	"callers",
	"key_money_baselines",
	"key_money_counters",
	"key_token_baselines",
	"key_token_counters",
	"keys",
	"model_token_limits",
	"prices",
	"pricing_rules",
	"requests",
	"usages",
}

// schemaIndexes are the named indexes the current format creates explicitly.
var schemaIndexes = []string{
	"adjustments_request",
	"audit_events_caller",
	"audit_events_key",
	"key_money_counters_window",
	"key_token_counters_window",
	"pricing_rules_resolution",
	"requests_key_started",
	"requests_key_state",
	"requests_started",
	"requests_state_started",
	"requests_trace",
	"usages_provider",
	"usages_request",
	"usages_request_provider",
	"usages_trace_key",
}

type columnInfo struct {
	kind    string
	notNull bool
}

// pragmaColumns reads the column name, declared type and NOT NULL flag of one
// table, so the format can be asserted instead of assumed.
func pragmaColumns(t *testing.T, db *sql.DB, table string) map[string]columnInfo {
	t.Helper()
	rows, err := db.Query(`SELECT name, type, "notnull" FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("inspect %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]columnInfo{}
	for rows.Next() {
		var (
			name, kind string
			notNull    int
		)
		if err := rows.Scan(&name, &kind, &notNull); err != nil {
			t.Fatalf("scan %s column: %v", table, err)
		}
		out[name] = columnInfo{kind: strings.ToUpper(kind), notNull: notNull != 0}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s columns: %v", table, err)
	}
	return out
}

func userTableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	slices.Sort(names)
	return names
}

func userObjectExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var found int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&found); err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	return found > 0
}

func dbUserVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return version
}

// TestFreshLedgerCarriesCompleteCurrentSchema pins the single current format: a
// first initialization must create every table, index and feature column, with
// no leftover legacy rate column anywhere.
func TestFreshLedgerCarriesCompleteCurrentSchema(t *testing.T) {
	h := newHarness(t)
	db := h.store.db
	if got := dbUserVersion(t, db); got != schemaVersion {
		t.Fatalf("user_version = %d, want %d", got, schemaVersion)
	}
	if got := userTableNames(t, db); !slices.Equal(got, schemaTables) {
		t.Fatalf("tables = %v, want %v", got, schemaTables)
	}
	for _, name := range schemaIndexes {
		if !userObjectExists(t, db, name) {
			t.Errorf("index %s is missing", name)
		}
	}

	// requests freezes its price in one snapshot column only: the duplicated
	// legacy rate columns must not exist at all.
	requests := pragmaColumns(t, db, "requests")
	snapshot, ok := requests["price_snapshot"]
	if !ok || !snapshot.notNull || snapshot.kind != "TEXT" {
		t.Fatalf("requests.price_snapshot = %+v, want a NOT NULL TEXT column", snapshot)
	}
	for _, legacy := range []string{
		"price_input", "price_output", "price_reasoning", "price_cache_read",
		"price_cache_write", "price_version",
		"held_micro_usd", "token_estimate", "initial_hold_micro_usd",
	} {
		if _, ok := requests[legacy]; ok {
			t.Errorf("requests still carries the removed column %s", legacy)
		}
	}

	// Recoverable key material is mandatory, and the key feature columns exist.
	keys := pragmaColumns(t, db, "keys")
	if material, ok := keys["key_material_enc"]; !ok || !material.notNull || material.kind != "BLOB" {
		t.Fatalf("keys.key_material_enc = %+v, want a NOT NULL BLOB column", material)
	}
	for _, column := range []string{"caller_id", "deleted_at"} {
		if _, ok := keys[column]; !ok {
			t.Errorf("keys.%s is missing", column)
		}
	}
	if _, ok := keys["held_micro_usd"]; ok {
		t.Error("keys still carries the removed column held_micro_usd")
	}

	prices := pragmaColumns(t, db, "prices")
	for _, column := range []string{"billing_mode", "per_image_micro_usd"} {
		if _, ok := prices[column]; !ok {
			t.Errorf("prices.%s is missing", column)
		}
	}
	if _, ok := pragmaColumns(t, db, "usages")["service_tier"]; !ok {
		t.Error("usages.service_tier is missing")
	}
}

// TestReopenCurrentFormatLeavesSchemaAndRowsUntouched pins that reopening a
// ledger of the current format neither re-runs nor repairs DDL: whatever the
// ledger contains is opened as it is.
func TestReopenCurrentFormatLeavesSchemaAndRowsUntouched(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	// An object the store never created: a same-format reopen must not drop,
	// recreate or otherwise patch the schema around it.
	if _, err := h.store.db.Exec(`CREATE TABLE sentinel_probe (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(Options{DataDir: h.dir, Now: h.clock.now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got := dbUserVersion(t, reopened.db); got != schemaVersion {
		t.Fatalf("user_version after reopen = %d, want %d", got, schemaVersion)
	}
	if !userObjectExists(t, reopened.db, "sentinel_probe") {
		t.Fatal("reopen rewrote the schema instead of opening it as it is")
	}
	if got, err := reopened.GetKey(context.Background(), key.ID); err != nil || got.ID != key.ID {
		t.Fatalf("key after reopen = %+v, %v", got, err)
	}
	if prices, err := reopened.ListPrices(context.Background()); err != nil || len(prices) != 1 {
		t.Fatalf("prices after reopen = %v, %v", prices, err)
	}
}

// TestOpenRejectsForeignLedgerWithoutChangingIt pins the no-migration contract:
// an unmarked ledger with user schema objects is some other format and must be
// refused before any DDL runs, leaving its schema and rows exactly as they were.
func TestOpenRejectsForeignLedgerWithoutChangingIt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbFileName)
	raw, err := sql.Open(driverName, dsnFor(dbPath))
	if err != nil {
		t.Fatalf("open raw ledger: %v", err)
	}
	// A compact sentinel of another format: no user_version, its own tables.
	for _, stmt := range []string{
		`CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
		`INSERT INTO meta(k, v) VALUES ('schema_version', '1')`,
		`CREATE TABLE foreign_keys (id TEXT PRIMARY KEY, hmac TEXT NOT NULL)`,
		`INSERT INTO foreign_keys(id, hmac) VALUES ('legacy-key', 'legacy-hmac')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed foreign ledger: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw ledger: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, pepperFileName), make([]byte, pepperLen), 0o600); err != nil {
		t.Fatalf("write pepper: %v", err)
	}

	if s, err := Open(Options{DataDir: dir}); !errors.Is(err, ErrSchemaVersion) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("open foreign ledger err = %v, want ErrSchemaVersion", err)
	}

	check, err := sql.Open(driverName, dsnFor(dbPath))
	if err != nil {
		t.Fatalf("reopen raw ledger: %v", err)
	}
	defer check.Close()
	if got := dbUserVersion(t, check); got != 0 {
		t.Fatalf("foreign ledger user_version = %d, want untouched 0", got)
	}
	if userObjectExists(t, check, "requests") {
		t.Fatal("open created schema objects in a foreign ledger")
	}
	var value string
	if err := check.QueryRow(`SELECT v FROM meta WHERE k = 'schema_version'`).Scan(&value); err != nil || value != "1" {
		t.Fatalf("meta row after refused open = %q, %v", value, err)
	}
	var hmac string
	if err := check.QueryRow(`SELECT hmac FROM foreign_keys WHERE id = 'legacy-key'`).Scan(&hmac); err != nil || hmac != "legacy-hmac" {
		t.Fatalf("foreign row after refused open = %q, %v", hmac, err)
	}
}

// TestOpenRejectsUnknownUserVersionWithoutChangingIt pins that a version this
// build does not own is refused as-is, without dropping or rebuilding anything.
func TestOpenRejectsUnknownUserVersionWithoutChangingIt(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	if err := h.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	dbPath := filepath.Join(h.dir, dbFileName)
	raw, err := sql.Open(driverName, dsnFor(dbPath))
	if err != nil {
		t.Fatalf("open raw ledger: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatalf("stamp unknown version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw ledger: %v", err)
	}

	if s, err := Open(Options{DataDir: h.dir, Now: h.clock.now}); !errors.Is(err, ErrSchemaVersion) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("open unknown version err = %v, want ErrSchemaVersion", err)
	}

	check, err := sql.Open(driverName, dsnFor(dbPath))
	if err != nil {
		t.Fatalf("reopen raw ledger: %v", err)
	}
	defer check.Close()
	if got := dbUserVersion(t, check); got != 7 {
		t.Fatalf("user_version = %d, want untouched 7", got)
	}
	if !userObjectExists(t, check, "requests") {
		t.Fatal("refused open dropped the existing schema")
	}
	var id string
	if err := check.QueryRow(`SELECT id FROM keys WHERE id = ?`, key.ID).Scan(&id); err != nil || id != key.ID {
		t.Fatalf("key row after refused open = %q, %v", id, err)
	}
}

// TestSchemaInitializationRollsBackOnFailure proves the initialization
// transaction is atomic: the whole schema and the format marker are written
// together, so a genuine storage failure part way through the DDL leaves a
// completely empty ledger instead of a partial schema (or a marker without a
// schema).
func TestSchemaInitializationRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbFileName)
	db, err := sql.Open(driverName, dsnFor(dbPath))
	if err != nil {
		t.Fatalf("open raw ledger: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	// Squeeze the ledger so the schema cannot fit: the DDL fails for real part
	// way through, after some statements already ran, which is exactly the
	// interrupted initialization the rollback must survive.
	if _, err := db.ExecContext(ctx, `PRAGMA page_size = 1024`); err != nil {
		t.Fatalf("shrink page size: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA max_page_count = 2`); err != nil {
		t.Fatalf("cap page count: %v", err)
	}
	s := &Store{db: db}
	err = s.initSchema(ctx)
	if err == nil {
		t.Fatal("initialization succeeded on a ledger too small for the schema")
	}
	// The failure must come from the DDL itself, after the version and
	// emptiness checks passed, otherwise the rollback claim would be untested.
	if !strings.Contains(err.Error(), "apply schema") {
		t.Fatalf("initialization failed outside the DDL (%v)", err)
	}
	if got := dbUserVersion(t, db); got != 0 {
		t.Fatalf("user_version = %d after rollback, want 0", got)
	}
	if got := userTableNames(t, db); len(got) != 0 {
		t.Fatalf("tables after rollback = %v, want none", got)
	}
}
