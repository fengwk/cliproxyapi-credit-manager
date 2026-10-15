package capture

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// busyTimeoutMillis waits for a lock instead of failing immediately. Busy
	// conditions stay observable through errors and Stats.LastError.
	busyTimeoutMillis = 5000

	schemaCaptures = `CREATE TABLE IF NOT EXISTS captures (
	id           TEXT    PRIMARY KEY NOT NULL,
	at           INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	headers      TEXT    NOT NULL DEFAULT '',
	body         BLOB    NOT NULL DEFAULT x'',
	stored_bytes INTEGER NOT NULL,
	raw_bytes    INTEGER NOT NULL
)`
	schemaExpiryIndex = `CREATE INDEX IF NOT EXISTS captures_by_expiry ON captures(expires_at)`

	insertCapture = `INSERT OR REPLACE INTO captures
	(id, at, expires_at, headers, body, stored_bytes, raw_bytes) VALUES (?, ?, ?, ?, ?, ?, ?)`

	selectCapture = `SELECT at, expires_at, headers, body FROM captures WHERE id = ?`
	selectExpired = `SELECT id, at, expires_at FROM captures WHERE expires_at <= ? ORDER BY expires_at LIMIT ?`
	selectOldest  = `SELECT id, at, expires_at, stored_bytes, length(headers) FROM captures ORDER BY expires_at LIMIT ?`
)

var (
	// errNoDataDir reports a missing data directory.
	errNoDataDir = errors.New("capture: data directory is empty")
	// errNonPositive reports a non-positive retention duration.
	errNonPositive = errors.New("duration must be positive")
	// errBodyTooLarge reports a stored body that expands past the body limit.
	errBodyTooLarge = errors.New("capture: stored body exceeds the configured body limit")
)

// configError reports an unusable configuration value.
type configError struct {
	field string
	value string
	err   error
}

func (e *configError) Error() string {
	return "capture: invalid " + e.field + " " + strconv.Quote(e.value) + ": " + e.err.Error()
}

func (e *configError) Unwrap() error { return e.err }

// db is the single-connection SQLite handle of one Store.
type db struct {
	*sql.DB
	path string
}

// openDatabase prepares the capture database file. The file is created
// privately before SQLite opens it so that the -wal and -shm files inherit the
// same 0600 permissions.
func openDatabase(dataDir string) (string, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file := filepath.Join(dir, dbFileName)
	handle, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", err
	}
	if err := handle.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return "", err
	}
	return file, nil
}

// openDB opens the capture database with per-connection pragmas and a single
// pooled connection, so that no statement is retried against a stale handle.
func openDB(path string) (*db, error) {
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMillis) + ")&_pragma=synchronous(NORMAL)",
	}).String()
	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	handle.SetConnMaxLifetime(0)
	handle.SetConnMaxIdleTime(0)
	return &db{DB: handle, path: path}, nil
}

// initSchema selects incremental auto-vacuum before the first schema page is
// written, switches to WAL and creates the capture schema. Incremental
// auto-vacuum is what allows Cleanup to return freed pages to the file system.
func (s *Store) initSchema() error {
	if _, err := s.db.Exec("PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return fmt.Errorf("capture: enable incremental auto_vacuum: %w", err)
	}
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
		return fmt.Errorf("capture: enable write-ahead log: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("capture: journal mode is %q, want wal", mode)
	}
	for _, statement := range []string{schemaCaptures, schemaExpiryIndex} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("capture: create schema: %w", err)
		}
	}
	if _, err := s.db.Exec("PRAGMA user_version = " + strconv.Itoa(schemaVersion)); err != nil {
		return fmt.Errorf("capture: set schema version: %w", err)
	}
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&s.pageSize); err != nil {
		return fmt.Errorf("capture: read page_size: %w", err)
	}
	var autoVacuum int
	if err := s.db.QueryRow("PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil {
		return fmt.Errorf("capture: read auto_vacuum: %w", err)
	}
	if autoVacuum != 2 {
		// A database created by something else cannot be switched to
		// incremental auto-vacuum without a full VACUUM; report it instead of
		// silently never shrinking the file.
		s.setLastError("capture: database has auto_vacuum=" + strconv.Itoa(autoVacuum) +
			"; freed pages are not returned to the file system")
	}
	return nil
}

// storedRow is one raw capture row.
type storedRow struct {
	found     bool
	at        time.Time
	expiresAt time.Time
	headers   string
	body      []byte
}

// fetch reads one capture row without decoding the body.
func (d *db) fetch(id string) (storedRow, error) {
	var at, expiresAt int64
	var row storedRow
	err := d.QueryRow(selectCapture, id).Scan(&at, &expiresAt, &row.headers, &row.body)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRow{}, nil
	}
	if err != nil {
		return storedRow{}, err
	}
	row.found = true
	row.at = time.Unix(0, at).UTC()
	row.expiresAt = time.Unix(0, expiresAt).UTC()
	return row, nil
}

// persist writes one accepted snapshot, evicting older snapshots when the
// physical storage budget is exhausted.
func (s *Store) persist(item pending) {
	body, plainBytes, err := encodeBody(item.body)
	if err != nil {
		s.dropAccepted(item, "capture: encode "+item.id+": "+err.Error())
		return
	}
	// Redaction replaces secrets with a marker that can be longer than the
	// secret itself, so a body within the raw limit can still grow past it. Such
	// a snapshot is dropped with an explicit status instead of being stored as a
	// row that Get could never decode within the body limit.
	if plainBytes > s.cfg.MaxBodyBytes {
		s.dropAcceptedAs(item, StatusOversize,
			"capture: redacted body grew to "+strconv.FormatInt(plainBytes, 10)+
				" bytes, above the "+strconv.FormatInt(s.cfg.MaxBodyBytes, 10)+" byte body limit; snapshot dropped")
		return
	}
	estimated := int64(len(body)+len(item.headersJSON)) + rowOverhead

	s.opMu.Lock()
	defer s.opMu.Unlock()
	fit, err := s.makeRoom(estimated)
	if err != nil {
		s.dropAccepted(item, err.Error())
		return
	}
	if !fit {
		s.dropAccepted(item, "capture: storage budget exhausted; snapshot dropped")
		return
	}
	if _, err := s.db.Exec(insertCapture, item.id, item.at.UnixNano(), item.expiresAt.UnixNano(),
		string(item.headersJSON), body, len(body), plainBytes); err != nil {
		s.dropAccepted(item, "capture: store "+item.id+": "+err.Error())
		return
	}
	s.observeRow(estimated)
}

// dropAccepted counts an accepted snapshot that could not be stored.
func (s *Store) dropAccepted(item pending, message string) {
	s.dropAcceptedAs(item, StatusDropped, message)
}

// dropAcceptedAs counts an accepted snapshot that could not be stored and
// remembers why, so that Get explains the outcome instead of reporting a raw
// failure.
func (s *Store) dropAcceptedAs(item pending, status, message string) {
	s.setLastError(message)
	s.recordDrop(item.id, status, item.at, item.expiresAt)
}

// encodeBody redacts obvious JSON secrets and gzip compresses a body, reporting
// how many plaintext bytes the stored form represents. Bodies are never stored
// uncompressed, and identical input always yields identical bytes.
func encodeBody(raw []byte) (compressed []byte, plainBytes int64, err error) {
	if len(raw) == 0 {
		return nil, 0, nil
	}
	body := raw
	redacted, changed, err := redactJSONBody(raw)
	if err != nil {
		return nil, 0, err
	}
	if changed {
		body = redacted
	}
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.DefaultCompression)
	if err != nil {
		return nil, 0, err
	}
	if _, err := writer.Write(body); err != nil {
		return nil, 0, err
	}
	if err := writer.Close(); err != nil {
		return nil, 0, err
	}
	return buffer.Bytes(), int64(len(body)), nil
}

// decodeBody inflates a stored body, bounding the result so a corrupted or
// crafted body cannot exhaust memory.
func decodeBody(compressed []byte, maxBody int64) ([]byte, error) {
	if len(compressed) == 0 {
		return nil, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer reader.Close()
	out, err := io.ReadAll(io.LimitReader(reader, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if int64(len(out)) > maxBody {
		return nil, errBodyTooLarge
	}
	return out, nil
}

// physicalBytes reports the on-disk size of the database and its WAL file.
// Index and header storage live inside the database file.
func (s *Store) physicalBytes() (databaseBytes, walBytes int64) {
	return fileSize(s.dbPath), fileSize(s.dbPath + "-wal")
}

// usedBytes reports the physical bytes the store currently occupies.
func (s *Store) usedBytes() int64 {
	databaseBytes, walBytes := s.physicalBytes()
	return databaseBytes + walBytes
}

// observeRow remembers the size of a stored row so that eviction can discard as
// little diagnostic data as possible.
func (s *Store) observeRow(size int64) {
	if size <= 0 {
		return
	}
	s.mu.Lock()
	if s.rowEstimate <= 0 {
		s.rowEstimate = size
	} else {
		s.rowEstimate = (s.rowEstimate*7 + size) / 8
	}
	s.mu.Unlock()
}

// rowSize returns the remembered average stored row size.
func (s *Store) rowSize() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rowEstimate <= 0 {
		return 4096
	}
	return s.rowEstimate
}

// evictBatch chooses how many snapshots to evict for need bytes of pressure.
func (s *Store) evictBatch(need int64) int {
	if need <= 0 {
		return 1
	}
	batch := int((need + s.rowSize() - 1) / s.rowSize())
	if batch < 1 {
		batch = 1
	}
	if batch > evictBatchSize {
		batch = evictBatchSize
	}
	return batch
}

// fileSize returns the size of path, or zero when it does not exist yet.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// makeRoom reports whether a row of size bytes fits into the physical budget,
// evicting the oldest snapshots and reclaiming space when it does not.
//
// The eviction batch is sized from the current pressure and the average stored
// row size, so a store that is only slightly over its budget discards only
// slightly more than the snapshot it is making room for.
func (s *Store) makeRoom(size int64) (bool, error) {
	if size > s.storageLimit {
		// The row can never fit; evicting everything else would not help.
		return false, nil
	}
	for attempt := 0; attempt < maxEvictBatches; attempt++ {
		used := s.usedBytes()
		if used <= 0 || used+size <= s.storageLimit {
			return true, nil
		}
		need := used + size - s.storageLimit
		evicted, freed, err := s.evictOldest(s.evictBatch(need))
		if err != nil {
			return false, err
		}
		if evicted == 0 {
			if err := s.reclaim(vacuumStepPages * 4); err != nil {
				return false, err
			}
			return s.fits(size), nil
		}
		if err := s.reclaim(reclaimPages(freed, s.pageSize)); err != nil {
			return false, err
		}
	}
	return s.fits(size), nil
}

// fits reports whether a row of size bytes fits below the storage limit.
func (s *Store) fits(size int64) bool {
	used := s.usedBytes()
	if used <= 0 {
		// The database file does not exist yet, so nothing can be over budget.
		return true
	}
	return used+size <= s.storageLimit
}

// reclaimPages converts evicted bytes into the number of pages the following
// incremental vacuum has to return, with slack for the pages it moves.
func reclaimPages(freed, pageSize int64) int64 {
	if pageSize <= 0 {
		pageSize = 4096
	}
	pages := freed/pageSize + 8
	if pages > vacuumPagesPerRun {
		pages = vacuumPagesPerRun
	}
	if pages < vacuumStepPages {
		pages = vacuumStepPages
	}
	return pages
}

// expireSnapshots deletes expired rows in bounded batches and remembers the
// outcome so that Get can report StatusExpired with evidence.
func (s *Store) expireSnapshots(now int64, maxRows int) error {
	deleted := 0
	for deleted < maxRows {
		batch := expireBatchSize
		if rest := maxRows - deleted; rest < batch {
			batch = rest
		}
		entries, err := s.rowEntries(selectExpired, now, batch)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		if err := s.deleteRows(entries); err != nil {
			return err
		}
		// Evidence of expiry outlives the snapshot itself by one retention
		// period, so a later lookup reports StatusExpired instead of pretending
		// the request was never seen.
		evidence := s.now().Add(s.cfg.Retention)
		s.mu.Lock()
		for _, entry := range entries {
			entry.status = StatusExpired
			entry.expiresAt = evidence
			s.ring.push(entry, s.now())
		}
		s.mu.Unlock()
		deleted += len(entries)
	}
	return nil
}

// evictOldest deletes up to limit of the oldest rows, counts them as dropped and
// reports how many stored bytes they occupied, keeping the physical budget
// bounded under pressure.
func (s *Store) evictOldest(limit int) (int, int64, error) {
	rows, err := s.db.Query(selectOldest, limit)
	if err != nil {
		return 0, 0, err
	}
	var entries []statusEntry
	var freed int64
	for rows.Next() {
		var entry statusEntry
		var at, expiresAt, stored, headers int64
		if err := rows.Scan(&entry.id, &at, &expiresAt, &stored, &headers); err != nil {
			rows.Close()
			return 0, 0, err
		}
		entry.at = time.Unix(0, at).UTC()
		entry.expiresAt = time.Unix(0, expiresAt).UTC()
		entries = append(entries, entry)
		freed += stored + headers + rowOverhead
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()
	if len(entries) == 0 {
		return 0, 0, nil
	}
	if err := s.deleteRows(entries); err != nil {
		return 0, 0, err
	}
	// Keep the explanation of the eviction for one retention period, so a lookup
	// reports StatusDropped instead of pretending the snapshot was never seen.
	evidence := s.now().Add(s.cfg.Retention)
	s.mu.Lock()
	s.dropped += int64(len(entries))
	for _, entry := range entries {
		entry.status = StatusDropped
		entry.expiresAt = evidence
		s.ring.push(entry, s.now())
	}
	s.mu.Unlock()
	return len(entries), freed, nil
}

// rowEntries reads id and lifetime of rows selected by query.
func (s *Store) rowEntries(query string, args ...any) ([]statusEntry, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []statusEntry
	for rows.Next() {
		var id string
		var at, expiresAt int64
		if err := rows.Scan(&id, &at, &expiresAt); err != nil {
			return nil, err
		}
		entries = append(entries, statusEntry{
			id:        id,
			at:        time.Unix(0, at).UTC(),
			expiresAt: time.Unix(0, expiresAt).UTC(),
		})
	}
	return entries, rows.Err()
}

// deleteRows removes the given snapshot rows in one bounded statement.
func (s *Store) deleteRows(entries []statusEntry) error {
	if len(entries) == 0 {
		return nil
	}
	args := make([]any, len(entries))
	var builder strings.Builder
	builder.WriteString("DELETE FROM captures WHERE id IN (")
	for i, entry := range entries {
		if i != 0 {
			builder.WriteByte(',')
		}
		builder.WriteByte('?')
		args[i] = entry.id
	}
	builder.WriteByte(')')
	_, err := s.db.Exec(builder.String(), args...)
	return err
}

// reclaim returns freed pages to the file system step by step and truncates the
// WAL. It is never a full VACUUM, so a database that is still alive is never
// rewritten wholesale.
//
// The log is folded into the database first, so that the pages freed by the
// deletes actually belong to the database file that the vacuum shrinks.
func (s *Store) reclaim(maxPages int64) error {
	if err := s.checkpoint(); err != nil {
		return err
	}
	var freed int64
	// Both the returned pages and the number of statements are bounded, so a
	// driver that reports progress differently cannot spin here.
	for statements := 0; freed < maxPages && statements < maxVacuumStatements; statements++ {
		step := int64(vacuumStepPages)
		if rest := maxPages - freed; rest < step {
			step = rest
		}
		// The driver only performs the work of the rows it steps through, so the
		// result must be drained for every vacated page.
		rows, err := s.db.Query("PRAGMA incremental_vacuum(" + strconv.FormatInt(step, 10) + ")")
		if err != nil {
			return fmt.Errorf("capture: incremental vacuum: %w", err)
		}
		steps := 0
		for rows.Next() {
			steps++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("capture: incremental vacuum: %w", err)
		}
		if steps == 0 {
			break
		}
		freed += int64(steps)
	}
	return s.checkpoint()
}

// checkpoint truncates the write-ahead log after its contents were committed
// into the database. A busy result is reported, not hidden.
func (s *Store) checkpoint() error {
	var busy, logFrames, checkpointed int64
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("capture: wal checkpoint: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("capture: wal checkpoint busy (log=%d checkpointed=%d)", logFrames, checkpointed)
	}
	return nil
}

// unmarshalHeaders decodes stored, already redacted headers.
func unmarshalHeaders(encoded string) (http.Header, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	var headers http.Header
	if err := json.Unmarshal([]byte(encoded), &headers); err != nil {
		return nil, fmt.Errorf("capture: decode headers: %w", err)
	}
	return headers, nil
}

// encodeHeaders redacts and serializes headers. Headers are bounded
// independently of the body; an oversized header set is dropped while the body
// is still captured.
func encodeHeaders(headers http.Header) []byte {
	if len(headers) == 0 || rawHeaderSize(headers, rawHeaderLimit) > rawHeaderLimit {
		return nil
	}
	encoded, err := json.Marshal(redactHeaders(headers))
	if err != nil || len(encoded) > headerLimit {
		return nil
	}
	return encoded
}

// headerReserve returns the queue bytes to reserve for a header set without
// serializing or copying it.
//
// The reservation is an upper bound of the encoded form: JSON escaping can
// inflate content up to sixfold, and an encoded set is never kept above
// headerLimit. A set that is too large to keep reserves nothing because it will
// be dropped.
func headerReserve(headers http.Header) int64 {
	size := int64(rawHeaderSize(headers, rawHeaderLimit))
	if size > rawHeaderLimit {
		return 0
	}
	if bound := size * 6; bound < headerLimit {
		return bound
	}
	return headerLimit
}
