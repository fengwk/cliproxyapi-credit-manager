package credit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Cleanup purges closed history older than the retention window. Durable
// counters and unresolved requests are never removed, and free pages are
// physically reclaimed from the database and the WAL.
func (s *Store) Cleanup(ctx context.Context) (Maintenance, error) {
	cutoff := s.retentionFrom(s.nowUTC())
	var deleted int64
	for {
		ids, err := s.cleanupBatchIDs(ctx, cutoff)
		if err != nil {
			return Maintenance{}, err
		}
		if len(ids) == 0 {
			break
		}
		if err := s.inTx(ctx, func(tx *sql.Tx) error { return deleteHistory(ctx, tx, ids) }); err != nil {
			return Maintenance{}, err
		}
		deleted += int64(len(ids))
	}
	// Reclaim physical space: the WAL is first folded into the database, then
	// incremental vacuum returns free pages to the filesystem, then the WAL is
	// truncated to zero.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return Maintenance{}, fmt.Errorf("checkpoint wal: %w", err)
	}
	if err := s.reclaimSpace(ctx); err != nil {
		return Maintenance{}, err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return Maintenance{}, fmt.Errorf("truncate wal: %w", err)
	}
	dbBytes, walBytes := s.fileSizes()
	return Maintenance{Deleted: deleted, DatabaseBytes: dbBytes, WALBytes: walBytes}, nil
}

// reclaimSpace drives the incremental auto-vacuum to completion. SQLite reports
// one row per reclaimed page, so all rows must be consumed for the file to
// actually shrink.
func (s *Store) reclaimSpace(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA incremental_vacuum(2147483647)`)
	if err != nil {
		return fmt.Errorf("incremental vacuum: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("incremental vacuum: %w", err)
	}
	scratch := make([]any, len(cols))
	holders := make([]sql.NullString, len(cols))
	for i := range scratch {
		scratch[i] = &holders[i]
	}
	for rows.Next() {
		if err := rows.Scan(scratch...); err != nil {
			return fmt.Errorf("incremental vacuum: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("incremental vacuum: %w", err)
	}
	return nil
}

func (s *Store) cleanupBatchIDs(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM requests
		WHERE state = ? AND started_at < ? ORDER BY started_at LIMIT ?`,
		stateSettled, unixNano(cutoff), cleanupBatch)
	if err != nil {
		return nil, fmt.Errorf("select expired requests: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired request: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired requests: %w", err)
	}
	return ids, nil
}

func deleteHistory(ctx context.Context, tx *sql.Tx, ids []string) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usages WHERE request_id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("delete usages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM adjustments WHERE request_id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("delete adjustments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("delete requests: %w", err)
	}
	return nil
}
