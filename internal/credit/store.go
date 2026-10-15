package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver
)

// Lifecycle vocabulary stored in the database. It is part of the internal
// contract shared with the plugin adapter, not with the database file.
const (
	stateActive  = "active"  // execution in flight, holds money and a concurrency slot
	statePending = "pending" // execution finished/restarted but accounting is unresolved
	stateSettled = "settled" // closed: hold released, known costs charged

	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"

	// QualityComplete marks usage that is trustworthy even when all counters
	// are zero. Empty quality is trusted only when it carries positive tokens.
	QualityComplete = "complete"
	QualityPartial  = "partial"
	QualityUnknown  = "unknown"

	ActionRelease = "release"
	ActionCharge  = "charge"

	windowTotal = "total"
	windowDay   = "day"
	windowWeek  = "week"
	windowMonth = "month"

	resolvedAuto = "auto"
)

// ErrPending reports that a key has too many unresolved requests to admit more.
// It is additive to the shared error set in types.go.
var ErrPending = errors.New("too many unresolved requests")

const (
	driverName             = "sqlite"
	dbFileName             = "credit.db"
	pepperFileName         = "pepper"
	defaultRetentionMonths = 3
	defaultMaxPending      = 16
	pepperLen              = 32
	microPerUnit           = 1_000_000 // rates are microUSD per million tokens
	cleanupBatch           = 512
	maxPageSize            = 500
)

const schemaDDL = `
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
  resolved TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS requests_trace ON requests(trace_id);
CREATE INDEX IF NOT EXISTS requests_key_state ON requests(key_id, state);
CREATE INDEX IF NOT EXISTS requests_state_started ON requests(state, started_at);
CREATE INDEX IF NOT EXISTS requests_started ON requests(started_at);
CREATE INDEX IF NOT EXISTS requests_key_started ON requests(key_id, started_at);
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
CREATE INDEX IF NOT EXISTS usages_request ON usages(request_id);
CREATE INDEX IF NOT EXISTS usages_trace_key ON usages(trace_id, key_id);
CREATE INDEX IF NOT EXISTS usages_provider ON usages(provider, auth_id);
CREATE INDEX IF NOT EXISTS usages_request_provider ON usages(request_id, provider, auth_id);
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
CREATE INDEX IF NOT EXISTS adjustments_request ON adjustments(request_id);
`

// Store is the SQLite-backed implementation of Backend.
type Store struct {
	db              *sql.DB
	dbPath          string
	walPath         string
	pepper          []byte
	now             func() time.Time
	retentionMonths int
	maxPending      int
}

var _ Backend = (*Store)(nil)

// Open prepares the accounting database inside Options.DataDir. Reopening an
// existing directory reuses the pepper and marks crashed executions pending.
func Open(opts Options) (*Store, error) {
	dir := strings.TrimSpace(opts.DataDir)
	if dir == "" {
		return nil, fmt.Errorf("%w: data dir is required", ErrInvalid)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	pepper, err := loadPepper(filepath.Join(dir, pepperFileName))
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, dbFileName)
	dsn := dsnFor(dbPath)
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection keeps transactions serialized and pragmas deterministic.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{
		db:              db,
		dbPath:          dbPath,
		walPath:         dbPath + "-wal",
		pepper:          pepper,
		now:             opts.Now,
		retentionMonths: opts.RetentionMonths,
		maxPending:      opts.MaxPendingRequests,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.retentionMonths <= 0 {
		s.retentionMonths = defaultRetentionMonths
	}
	if s.maxPending <= 0 {
		s.maxPending = defaultMaxPending
	}
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.recoverExecutions(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func dsnFor(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "auto_vacuum(INCREMENTAL)")
	q.Add("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

func loadPepper(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != pepperLen {
			return nil, fmt.Errorf("%w: pepper file is corrupt", ErrInvalid)
		}
		// Never keep the pepper group/world readable.
		_ = os.Chmod(path, 0o600)
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read pepper: %w", err)
	}
	b := make([]byte, pepperLen)
	if _, err := readRandom(b); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return loadPepper(path)
		}
		return nil, fmt.Errorf("create pepper: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure pepper: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		return nil, fmt.Errorf("write pepper: %w", err)
	}
	return b, nil
}

func (s *Store) migrate(ctx context.Context) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, schemaDDL); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO meta(k, v) VALUES('schema_version', ?) ON CONFLICT(k) DO NOTHING`,
			fmt.Sprint(1))
		return err
	})
}

// recoverExecutions converts executions interrupted by a restart into pending
// requests: concurrency is released, holds are preserved.
func (s *Store) recoverExecutions(ctx context.Context) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE requests SET state = ? WHERE state = ?`, statePending, stateActive)
		return err
	})
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func (s *Store) nowUTC() time.Time { return s.now().UTC() }

// resolveAt normalizes a caller timestamp, defaulting to the store clock.
func (s *Store) resolveAt(t time.Time) time.Time {
	if t.IsZero() {
		return s.nowUTC()
	}
	return t.UTC()
}

func unixNano(t time.Time) int64 { return t.UTC().UnixNano() }

func fromUnixNano(v int64) time.Time { return time.Unix(0, v).UTC() }

func nullableNano(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return unixNano(*t)
}

func scanNano(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnixNano(v.Int64)
	return &t
}

func dayPeriod(t time.Time) string { return t.UTC().Format("2006-01-02") }

func weekPeriod(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

func monthPeriod(t time.Time) string { return t.UTC().Format("2006-01") }

// periodOf maps a window name to its UTC period label.
func periodOf(window string, t time.Time) string {
	switch window {
	case windowDay:
		return dayPeriod(t)
	case windowWeek:
		return weekPeriod(t)
	case windowMonth:
		return monthPeriod(t)
	default:
		return ""
	}
}

// addMonthsClamped shifts t by whole months in UTC, clamping the day to the
// last day of the target month (Jan 31 - 1 month = Dec 31, Mar 31 - 1 = Feb 28).
func addMonthsClamped(t time.Time, months int) time.Time {
	u := t.UTC()
	first := time.Date(u.Year(), u.Month(), 1, u.Hour(), u.Minute(), u.Second(), u.Nanosecond(), time.UTC).
		AddDate(0, months, 0)
	day := u.Day()
	if last := first.AddDate(0, 1, -1).Day(); day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, first.Hour(), first.Minute(), first.Second(), first.Nanosecond(), time.UTC)
}

// retentionFrom is the inclusive lower bound of retained history.
func (s *Store) retentionFrom(now time.Time) time.Time {
	return addMonthsClamped(now, -s.retentionMonths)
}

func (s *Store) fileSizes() (int64, int64) {
	var dbBytes, walBytes int64
	if info, err := os.Stat(s.dbPath); err == nil {
		dbBytes = info.Size()
	}
	if info, err := os.Stat(s.walPath); err == nil {
		walBytes = info.Size()
	}
	return dbBytes, walBytes
}
