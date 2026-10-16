package credit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// keyFeatureDDL adds the key management tables. Every statement is additive and
// uses IF NOT EXISTS, so the migration is idempotent on a fresh store and on a
// store that already ran it.
const keyFeatureDDL = `
CREATE TABLE IF NOT EXISTS callers (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  at INTEGER NOT NULL,
  action TEXT NOT NULL,
  key_id TEXT NOT NULL DEFAULT '',
  caller_id TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_events_key ON audit_events(key_id, seq);
CREATE INDEX IF NOT EXISTS audit_events_caller ON audit_events(caller_id, seq);
CREATE TABLE IF NOT EXISTS key_money_baselines (
  key_id TEXT NOT NULL,
  win TEXT NOT NULL,
  period TEXT NOT NULL,
  micro_usd INTEGER NOT NULL,
  PRIMARY KEY (key_id, win, period)
);
CREATE TABLE IF NOT EXISTS key_token_baselines (
  key_id TEXT NOT NULL,
  model TEXT NOT NULL,
  win TEXT NOT NULL,
  period TEXT NOT NULL,
  tokens INTEGER NOT NULL,
  PRIMARY KEY (key_id, model, win, period)
);
`

// keyFeatureColumns are the columns added to the existing keys table. They are
// additive so every pre-existing row is preserved untouched; recoverable
// material is left NULL for legacy keys, which is what makes RevealKey report a
// conflict until the operator rotates them.
var keyFeatureColumns = []struct {
	table  string
	column string
	ddl    string
}{
	{"keys", "key_material_enc", "BLOB"},
	{"keys", "caller_id", "TEXT NOT NULL DEFAULT ''"},
	{"keys", "deleted_at", "INTEGER"},
}

// migrateKeyFeatures upgrades an existing ledger in place. It creates the key
// management tables and adds the new keys columns when they are missing. It
// runs inside the same transaction as the base schema, so a failure rolls the
// whole migration back and reopening a migrated store is a no-op. Existing v1
// data is never rewritten: legacy keys simply have no encrypted material and a
// zero deleted_at/caller_id.
func migrateKeyFeatures(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, keyFeatureDDL); err != nil {
		return fmt.Errorf("apply key feature schema: %w", err)
	}
	for _, c := range keyFeatureColumns {
		if err := addColumnIfMissing(ctx, tx, c.table, c.column, c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// addColumnIfMissing adds one column only when the table exists and does not
// already carry it. SQLite has no ADD COLUMN IF NOT EXISTS, so the column list
// is inspected first; that keeps a second migration run a no-op instead of
// failing on the duplicate column.
func addColumnIfMissing(ctx context.Context, tx *sql.Tx, table, column, ddl string) error {
	columns, err := tableColumns(ctx, tx, table)
	if err != nil {
		return err
	}
	if _, ok := columns[column]; ok {
		return nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, ddl)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// tableColumns returns the current column names of a table. A missing table
// reports an empty set instead of an error so CREATE TABLE IF NOT EXISTS and
// the column probe can run in either order.
func tableColumns(ctx context.Context, q querier, table string) (map[string]struct{}, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan %s column: %w", table, err)
		}
		out[strings.TrimSpace(name)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s columns: %w", table, err)
	}
	return out, nil
}
