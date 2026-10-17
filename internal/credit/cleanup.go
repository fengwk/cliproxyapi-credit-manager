package credit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Cleanup purges closed history older than the retention window. Durable token
// counters and unresolved requests are never removed, and free pages are
// physically reclaimed from the database and the WAL. Space reclamation is
// bounded per run, so a single call may leave the file larger than its content;
// the counters above are exact, the sizes are not.
//
// Because a request's cost is derived from its executions at read time, deleting
// expired history also removes it from every reported amount: the selection
// range is the retained window, and no permanent amount is kept for a purged
// request.
//
// Idempotency note: keyed requests, usages and completions stay replayable only
// while their history is retained. History older than the retention window is
// physically deleted, so a very late replay of a long closed request is not
// recognised again.
func (s *Store) Cleanup(ctx context.Context) (Maintenance, error) {
	cutoff := s.retentionFrom(s.nowUTC())
	var deleted int64
	for {
		n, err := s.cleanupBatch(ctx, cutoff)
		if err != nil {
			return Maintenance{}, err
		}
		if n == 0 {
			break
		}
		deleted += n
	}
	// Reclaim physical space: the WAL is first folded into the database, then
	// incremental vacuum returns free pages to the filesystem, then the WAL is
	// truncated to zero.
	if err := s.pruneCounters(ctx, cutoff); err != nil {
		return Maintenance{}, err
	}
	if err := s.checkpointWAL(ctx); err != nil {
		return Maintenance{}, err
	}
	if err := s.reclaimSpace(ctx); err != nil {
		return Maintenance{}, err
	}
	if err := s.checkpointWAL(ctx); err != nil {
		return Maintenance{}, err
	}
	dbBytes, walBytes := s.fileSizes()
	return Maintenance{Deleted: deleted, DatabaseBytes: dbBytes, WALBytes: walBytes}, nil
}

// checkpointWAL folds the WAL back into the database and truncates it. SQLite
// reports the outcome instead of failing, so a busy result has to be surfaced:
// claiming a truncated WAL while a reader still blocks it would hide a real
// maintenance failure behind a green health check.
func (s *Store) checkpointWAL(ctx context.Context) error {
	var busy, logPages, checkpointed int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &logPages, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint wal: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint wal: database is busy after %d of %d pages", checkpointed, logPages)
	}
	return nil
}

// reclaimSpace frees a bounded number of pages per run. SQLite reports one row
// per page it actually reclaims, so all rows must be consumed, and asking for a
// page budget keeps routine maintenance from scanning a whole large database.
func (s *Store) reclaimSpace(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, vacuumPages))
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

// pruneCounters bounds the per-period token counter tables. Lifetime totals are
// the durable record and are never pruned; day/week/month rows disappear only
// once they are older than the retention window and no unresolved request can
// still add usage into them. Without a cleanup run nothing is removed.
func (s *Store) pruneCounters(ctx context.Context, cutoff time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		keep, err := unresolvedPeriods(ctx, tx)
		if err != nil {
			return err
		}
		for _, win := range []string{windowDay, windowWeek, windowMonth} {
			age := periodOf(win, cutoff)
			periods := keep[win]
			if err := pruneCounterTable(ctx, tx, "key_token_counters", win, age, periods); err != nil {
				return err
			}
			if err := pruneCounterTable(ctx, tx, "key_token_baselines", win, age, periods); err != nil {
				return err
			}
		}
		return nil
	})
}

// unresolvedPeriods collects the periods still reachable by a late charge: only
// unresolved requests can be charged after history was purged.
func unresolvedPeriods(ctx context.Context, q querier) (map[string][]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT started_at FROM requests WHERE state IN (?,?)`,
		stateActive, statePending)
	if err != nil {
		return nil, fmt.Errorf("load unresolved periods: %w", err)
	}
	defer rows.Close()
	seen := map[string]map[string]struct{}{}
	for rows.Next() {
		var started int64
		if err := rows.Scan(&started); err != nil {
			return nil, fmt.Errorf("scan unresolved period: %w", err)
		}
		at := fromUnixNano(started)
		for _, win := range []string{windowDay, windowWeek, windowMonth} {
			if seen[win] == nil {
				seen[win] = map[string]struct{}{}
			}
			seen[win][periodOf(win, at)] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unresolved periods: %w", err)
	}
	out := map[string][]string{}
	for win, set := range seen {
		periods := make([]string, 0, len(set))
		for p := range set {
			periods = append(periods, p)
		}
		out[win] = periods
	}
	return out, nil
}

// pruneCounterTable drops aged rows of one window, except the periods an
// unresolved request may still charge.
func pruneCounterTable(ctx context.Context, tx *sql.Tx, table, win, age string, keep []string) error {
	query := `DELETE FROM ` + table + ` WHERE win = ? AND period < ?`
	args := []any{win, age}
	if len(keep) > 0 {
		query += ` AND period NOT IN (` + placeholders(len(keep)) + `)`
		for _, p := range keep {
			args = append(args, p)
		}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("prune %s: %w", table, err)
	}
	return nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// cleanupBatch removes one bounded batch of expired settled history and returns
// how many requests actually disappeared. Selection and deletion share a single
// write transaction: with a separate read a late unaccounted execution could
// reopen a settled request in between and the deletion would race with it.
func (s *Store) cleanupBatch(ctx context.Context, cutoff time.Time) (int64, error) {
	var deleted int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		ids, err := expiredRequestIDs(ctx, tx, cutoff)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		deleted, err = deleteHistory(ctx, tx, ids, cutoff)
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// expiredRequestIDs lists the oldest closed requests that the retention window
// has passed, bounded so maintenance stays a fixed cost per run.
func expiredRequestIDs(ctx context.Context, q querier, cutoff time.Time) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM requests
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

// deleteHistory removes the history of the given candidates, but only for the
// requests that are still settled and still past the retention window. The
// check runs inside the same write transaction as the delete, so a request that
// was reopened by a late execution keeps its row and its executions.
func deleteHistory(ctx context.Context, tx *sql.Tx, ids []string, cutoff time.Time) (int64, error) {
	args := make([]any, 0, len(ids)+2)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, stateSettled, unixNano(cutoff))
	rows, err := tx.QueryContext(ctx, `SELECT id FROM requests WHERE id IN (`+placeholders(len(ids))+`)
		AND state = ? AND started_at < ?`, args...)
	if err != nil {
		return 0, fmt.Errorf("recheck expired requests: %w", err)
	}
	var actual []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired request: %w", err)
		}
		actual = append(actual, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate expired requests: %w", err)
	}
	rows.Close()
	if len(actual) == 0 {
		return 0, nil
	}
	// Every row of the surviving set is removed together: no request can keep
	// half of its history.
	actualArgs := make([]any, 0, len(actual))
	for _, id := range actual {
		actualArgs = append(actualArgs, id)
	}
	list := placeholders(len(actual))
	if _, err := tx.ExecContext(ctx, `DELETE FROM usages WHERE request_id IN (`+list+`)`, actualArgs...); err != nil {
		return 0, fmt.Errorf("delete usages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE id IN (`+list+`)`, actualArgs...); err != nil {
		return 0, fmt.Errorf("delete requests: %w", err)
	}
	return int64(len(actual)), nil
}
