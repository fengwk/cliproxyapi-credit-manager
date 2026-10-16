package credit

import (
	"context"
	"database/sql"
	"fmt"
)

// migratePricingFeatures applies the additive schema of advanced pricing on top
// of the base schema, on a fresh and on an existing populated store alike.
//
// It is transactional (it runs inside the journal-mode transaction started by
// migrate) and idempotent: every step is either CREATE TABLE IF NOT EXISTS or a
// column that is added only when it is still absent. Existing rows keep their
// stored columns, so a request that predates the price snapshot still settles
// from its legacy rate columns.
func migratePricingFeatures(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS pricing_rules (
		id TEXT PRIMARY KEY,
		match_kind TEXT NOT NULL,
		pattern TEXT NOT NULL,
		priority INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		billing_mode TEXT NOT NULL DEFAULT '',
		per_image_micro_usd INTEGER NOT NULL DEFAULT 0,
		input_rate INTEGER NOT NULL DEFAULT 0,
		output_rate INTEGER NOT NULL DEFAULT 0,
		reasoning_rate INTEGER,
		cache_read_rate INTEGER NOT NULL DEFAULT 0,
		cache_write_rate INTEGER NOT NULL DEFAULT 0,
		tiers_json TEXT NOT NULL DEFAULT '[]'
	)`); err != nil {
		return fmt.Errorf("create pricing rules: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS pricing_rules_resolution ON pricing_rules(priority DESC, id)`); err != nil {
		return fmt.Errorf("index pricing rules: %w", err)
	}
	additions := []struct {
		table  string
		column string
		ddl    string
	}{
		{"prices", "billing_mode", `ALTER TABLE prices ADD COLUMN billing_mode TEXT NOT NULL DEFAULT ''`},
		{"prices", "per_image_micro_usd", `ALTER TABLE prices ADD COLUMN per_image_micro_usd INTEGER NOT NULL DEFAULT 0`},
		{"requests", "price_snapshot", `ALTER TABLE requests ADD COLUMN price_snapshot TEXT`},
		{"usages", "service_tier", `ALTER TABLE usages ADD COLUMN service_tier TEXT NOT NULL DEFAULT ''`},
	}
	for _, addition := range additions {
		exists, err := columnExists(ctx, tx, addition.table, addition.column)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.ExecContext(ctx, addition.ddl); err != nil {
			return fmt.Errorf("add %s.%s: %w", addition.table, addition.column, err)
		}
	}
	return nil
}

// columnExists reports whether a table already carries a column. SQLite has no
// ADD COLUMN IF NOT EXISTS, so the additive migration must inspect the table
// first.
func columnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, fmt.Errorf("inspect %s columns: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	return false, nil
}
