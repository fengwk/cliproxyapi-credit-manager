// Package capture stores bounded diagnostic snapshots independently of money.
//
// Capture is disabled by default and intentionally disposable: every failure
// mode (oversized body, full queue, exhausted storage budget, busy database)
// degrades into dropping a snapshot and counting it, never into a failure of
// request inference or accounting.
//
// Snapshots are execution-entry diagnostics, not final upstream HTTP bytes.
// Stored content is private opt-in request data and is not an anonymity
// promise: authorization style headers and obvious JSON secret fields are
// redacted, but arbitrary private payloads are stored as supplied by the
// caller. Bodies are stored gzip compressed; headers are stored separately.
//
// Body handling is deliberately safety first at the boundary: bodies that are
// not JSON shaped (plain text, multipart, binary) are stored as supplied, while
// bodies that look like JSON but are not one complete document are dropped
// rather than stored unredacted, because their secrets cannot be located
// reliably.
package capture

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

// Snapshot statuses reported by Get.
const (
	// StatusDisabled means capture is switched off in the configuration.
	StatusDisabled = "disabled"
	// StatusCaptured means a stored snapshot was returned.
	StatusCaptured = "captured"
	// StatusOversize means the snapshot body exceeded the configured body limit.
	StatusOversize = "oversize"
	// StatusDropped means the snapshot was discarded under storage pressure.
	StatusDropped = "dropped"
	// StatusExpired means the snapshot retention period ended and the stored
	// snapshot is gone or no longer served.
	StatusExpired = "expired"
	// StatusUnavailable means no evidence about the request id is retained.
	StatusUnavailable = "unavailable"
)

const (
	// dbFileName is the disposable capture database inside the data directory.
	dbFileName = "request-captures.db"
	// schemaVersion is stored in PRAGMA user_version.
	schemaVersion = 1

	defaultRetention  = 24 * time.Hour
	defaultMaxBody    = 10 << 20 // 10MiB
	defaultMaxStorage = 1 << 30  // 1GiB

	// headerLimit bounds the serialized, redacted header set of one snapshot.
	headerLimit = 64 << 10
	// rawHeaderLimit rejects oversized header sets before they are copied.
	rawHeaderLimit = 4 * headerLimit
	// minHeaderStructureCost is the bookkeeping the queue charges for every
	// header name and value, so header sets with huge numbers of tiny values
	// are bounded before they are copied.
	minHeaderStructureCost = 8
	// rowOverhead is the per-row bookkeeping (index entries, row header, page
	// slack) charged to the storage budget in addition to stored bytes.
	rowOverhead = 512

	// Bound ID memory and index overhead independently of the body.
	maxRequestIDBytes = 128

	// statusRingLimit bounds the in-memory memory of recent outcomes. Metadata
	// per dropped request is small and TTL bound, never unbounded on disk.
	statusRingLimit = 4096

	// Cleanup and eviction batch sizes keep single statements bounded.
	expireBatchSize   = 256
	evictBatchSize    = 128
	maxEvictBatches   = 64
	maxExpireRows     = 16384
	vacuumStepPages   = 512
	vacuumPagesPerRun = 1 << 16
	// maxVacuumStatements bounds one incremental vacuum pass, so a driver that
	// reports its progress differently cannot make Cleanup spin.
	maxVacuumStatements = 256
)

// Store owns the disposable capture database and its asynchronous writer.
//
// A Store is safe for concurrent use. Enqueue never blocks on I/O; accepted
// snapshots are persisted by a single writer goroutine.
type Store struct {
	cfg Config
	now func() time.Time

	dbPath   string
	db       *db
	disabled bool
	pageSize int64

	queueLimit   int64
	storageLimit int64

	// beforeDequeue is a test hook, called by the writer before each dequeue.
	// It is set before the writer starts and never mutated afterwards.
	beforeDequeue func()

	opMu sync.Mutex // serializes multi-statement database work

	mu         sync.Mutex
	cond       *sync.Cond
	queue      *list.List
	queueBytes int64
	closed     bool
	dropped    int64
	lastErr    string
	ring       statusRing
	// enqueued and processed count accepted snapshots and snapshots the writer
	// finished, which lets callers and tests wait for the writer deterministically.
	enqueued  int64
	processed int64
	waiters   []drainWaiter
	// rowEstimate remembers the average stored row size for eviction sizing.
	rowEstimate int64

	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// pending is one accepted snapshot waiting for the writer.
type pending struct {
	id          string
	at          time.Time
	expiresAt   time.Time
	headersJSON []byte
	body        []byte
	cost        int64
}

// drainWaiter waits until the writer processed target accepted snapshots.
type drainWaiter struct {
	target int64
	done   chan struct{}
}

// storeOptions carries internal hooks used by tests.
type storeOptions struct {
	// now overrides the clock. Tests use a controllable clock instead of
	// sleeping.
	now func() time.Time
	// beforeDequeue runs at the top of every writer iteration.
	beforeDequeue func()
}

// Open opens (creating it when needed) the disposable capture database of
// dataDir. The returned Store must be closed by the caller.
func Open(dataDir string, cfg Config) (*Store, error) {
	store, err := newStore(dataDir, cfg, storeOptions{now: time.Now})
	if err != nil {
		return nil, err
	}
	store.start()
	return store, nil
}

// newStore prepares a Store without starting the writer goroutine.
func newStore(dataDir string, cfg Config, opts storeOptions) (*Store, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	s := &Store{
		cfg:           normalized,
		now:           opts.now,
		beforeDequeue: opts.beforeDequeue,
		// The pending queue is bounded in raw bytes and holds at least one
		// maximum size snapshot (body plus bounded headers) so that a single
		// large diagnostic is never rejected by queue accounting alone.
		queueLimit:   normalized.MaxBodyBytes + headerLimit + rowOverhead,
		storageLimit: storageLimit(normalized.MaxStorageBytes),
	}
	s.cond = sync.NewCond(&s.mu)
	s.queue = list.New()
	if !normalized.Enabled {
		// Disabled capture owns no files and copies no content.
		s.disabled = true
		return s, nil
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, errNoDataDir
	}
	path, err := openDatabase(dataDir)
	if err != nil {
		return nil, err
	}
	database, err := openDB(path)
	if err != nil {
		return nil, err
	}
	s.dbPath = path
	s.db = database
	if err := s.initSchema(); err != nil {
		_ = database.Close()
		return nil, err
	}
	return s, nil
}

// start launches the writer goroutine. It is called once by Open.
func (s *Store) start() {
	if s.disabled {
		return
	}
	s.wg.Add(1)
	go s.run()
}

// Enqueue accepts a snapshot for asynchronous capture.
//
// It never blocks on I/O and never mutates caller memory. Nothing is copied
// until the snapshot is admitted into the byte bounded queue: rejected
// snapshots (oversized body, oversized headers, full queue, closed store) cost
// the caller no allocation at all, so a queue under pressure cannot make every
// concurrent request copy its body. Bodies are redacted and compressed by the
// writer, headers are redacted here.
func (s *Store) Enqueue(snapshot Snapshot) {
	if s == nil || s.disabled {
		return
	}
	if snapshot.RequestID == "" {
		// Without an id the snapshot can never be retrieved.
		s.countDropped()
		return
	}
	if len(snapshot.RequestID) > maxRequestIDBytes {
		// Do not retain an oversized ID even in the status ring.
		s.countDropped()
		return
	}
	at := snapshot.At
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	expiresAt := at.Add(s.cfg.Retention)
	if int64(len(snapshot.Body)) > s.cfg.MaxBodyBytes {
		s.recordDrop(snapshot.RequestID, StatusOversize, at, expiresAt)
		return
	}
	reserve := headerReserve(snapshot.Headers) + int64(len(snapshot.Body)) + rowOverhead

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.dropped++
		return
	}
	if s.queueBytes+reserve > s.queueLimit {
		s.recordLocked(statusEntry{id: snapshot.RequestID, status: StatusDropped, at: at, expiresAt: expiresAt})
		return
	}
	// The snapshot is admitted: only now is caller memory copied. The copy stays
	// inside this critical section so that the reservation cannot be overtaken,
	// and it is bounded by the body and header limits.
	item := pending{
		id:          snapshot.RequestID,
		at:          at,
		expiresAt:   expiresAt,
		headersJSON: encodeHeaders(snapshot.Headers),
	}
	if len(snapshot.Body) != 0 {
		item.body = append([]byte(nil), snapshot.Body...)
	}
	item.cost = int64(len(item.body)+len(item.headersJSON)) + rowOverhead
	s.queue.PushBack(item)
	s.queueBytes += item.cost
	s.enqueued++
	s.cond.Signal()
}

// waitIdle blocks until the writer has finished every accepted snapshot. It is
// the deterministic synchronization point used by tests and by callers that
// need the queue flushed without closing the store. It returns false when
// called on a store that never accepted a snapshot.
func (s *Store) waitIdle() bool {
	if s == nil || s.disabled {
		return false
	}
	s.mu.Lock()
	target := s.enqueued
	if s.processed >= target {
		s.mu.Unlock()
		return true
	}
	waiter := drainWaiter{target: target, done: make(chan struct{})}
	s.waiters = append(s.waiters, waiter)
	s.mu.Unlock()
	<-waiter.done
	return true
}

// markProcessed records that the writer finished one accepted snapshot and
// releases waiters that are done waiting.
func (s *Store) markProcessed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processed++
	remaining := s.waiters[:0]
	for _, waiter := range s.waiters {
		if waiter.target <= s.processed {
			close(waiter.done)
			continue
		}
		remaining = append(remaining, waiter)
	}
	s.waiters = remaining
}

// Get returns the captured detail of one request id.
//
// The lookup is lazy and never claims that a request expired without evidence:
// ids that were never seen (or whose evidence aged out) report StatusUnavailable.
func (s *Store) Get(id string) (Detail, error) {
	if s == nil {
		return Detail{RequestID: id, Status: StatusUnavailable}, nil
	}
	if s.disabled {
		return Detail{RequestID: id, Status: StatusDisabled}, nil
	}
	if id == "" {
		return Detail{RequestID: id, Status: StatusUnavailable}, nil
	}
	row, err := s.db.fetch(id)
	if err != nil {
		s.setLastError("capture: read " + id + ": " + err.Error())
		return Detail{RequestID: id, Status: StatusUnavailable}, err
	}
	if !row.found {
		return s.remembered(id), nil
	}
	if !s.now().Before(row.expiresAt) {
		// The stored row is still present and proves the retention period ended.
		return Detail{RequestID: id, Status: StatusExpired, At: row.at, ExpiresAt: row.expiresAt}, nil
	}
	headers, err := unmarshalHeaders(row.headers)
	if err != nil {
		s.setLastError("capture: read " + id + ": " + err.Error())
		return Detail{RequestID: id, Status: StatusUnavailable, At: row.at, ExpiresAt: row.expiresAt}, err
	}
	body, err := decodeBody(row.body, s.cfg.MaxBodyBytes)
	if err != nil {
		s.setLastError("capture: read " + id + ": " + err.Error())
		return Detail{RequestID: id, Status: StatusUnavailable, At: row.at, ExpiresAt: row.expiresAt}, err
	}
	return Detail{
		RequestID: id,
		Status:    StatusCaptured,
		Headers:   headers,
		Body:      string(body),
		At:        row.at,
		ExpiresAt: row.expiresAt,
	}, nil
}

// Cleanup expires aged snapshots, evicts the oldest snapshots when the physical
// storage budget is exhausted and returns freed pages to the file system.
//
// Cleanup never runs a full VACUUM and never claims that retention alone keeps
// the database bounded: eviction is what bounds it. Busy or failing statements
// are reported to the caller and recorded in Stats.LastError.
func (s *Store) Cleanup() error {
	if s == nil || s.disabled {
		return nil
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.expireSnapshots(s.now().UnixNano(), maxExpireRows); err != nil {
		s.setLastError("capture: cleanup expiry: " + err.Error())
		return err
	}
	if _, err := s.makeRoom(0); err != nil {
		s.setLastError("capture: cleanup eviction: " + err.Error())
		return err
	}
	if err := s.reclaim(vacuumPagesPerRun); err != nil {
		s.setLastError("capture: cleanup reclaim: " + err.Error())
		return err
	}
	return nil
}

// Stats reports current storage usage and drop counters.
func (s *Store) Stats() Stats {
	if s == nil || s.disabled {
		return Stats{}
	}
	var stats Stats
	stats.DatabaseBytes, stats.WALBytes = s.physicalBytes()
	s.mu.Lock()
	stats.QueueBytes = s.queueBytes
	stats.Dropped = s.dropped
	stats.LastError = s.lastErr
	s.mu.Unlock()
	return stats
}

// Close stops accepting snapshots, drains the accepted queue and closes the
// database. It is idempotent.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.disabled {
			return
		}
		s.mu.Lock()
		s.closed = true
		s.cond.Broadcast()
		s.mu.Unlock()
		s.wg.Wait() // the writer drains every accepted snapshot first
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// run is the single writer goroutine.
func (s *Store) run() {
	defer s.wg.Done()
	for {
		if s.beforeDequeue != nil {
			s.beforeDequeue()
		}
		item, ok := s.next()
		if !ok {
			return
		}
		s.persist(item)
		s.markProcessed()
	}
}

// next blocks until an accepted snapshot exists or the store is closed and
// drained. The state mutex is never held across database work.
func (s *Store) next() (pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.queue.Len() == 0 && !s.closed {
		s.cond.Wait()
	}
	if s.queue.Len() == 0 {
		return pending{}, false
	}
	element := s.queue.Front()
	s.queue.Remove(element)
	item := element.Value.(pending)
	s.queueBytes -= item.cost
	return item, true
}

// remembered reports the retained outcome of an id that has no stored row.
func (s *Store) remembered(id string) Detail {
	s.mu.Lock()
	entry, ok := s.ring.lookup(id, s.now())
	s.mu.Unlock()
	if !ok {
		return Detail{RequestID: id, Status: StatusUnavailable}
	}
	return Detail{RequestID: id, Status: entry.status, At: entry.at, ExpiresAt: entry.expiresAt}
}

// recordDrop counts a dropped snapshot and remembers its outcome.
func (s *Store) recordDrop(id, status string, at, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(statusEntry{id: id, status: status, at: at, expiresAt: expiresAt})
}

// recordLocked remembers a dropped outcome. Callers hold s.mu.
func (s *Store) recordLocked(entry statusEntry) {
	s.dropped++
	s.ring.push(entry, s.now())
}

// countDropped counts a discarded snapshot that has no usable id.
func (s *Store) countDropped() {
	s.mu.Lock()
	s.dropped++
	s.mu.Unlock()
}

// setLastError records the most recent observable failure.
func (s *Store) setLastError(message string) {
	s.mu.Lock()
	s.lastErr = message
	s.mu.Unlock()
}

// normalizeConfig applies capture defaults and validates the retention text.
func normalizeConfig(cfg Config) (Config, error) {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBody
	}
	if cfg.MaxStorageBytes <= 0 {
		cfg.MaxStorageBytes = defaultMaxStorage
	}
	if cfg.Retention <= 0 {
		text := strings.TrimSpace(cfg.RetentionText)
		if text == "" {
			cfg.Retention = defaultRetention
			return cfg, nil
		}
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return cfg, &configError{field: "retention", value: text, err: err}
		}
		if parsed <= 0 {
			return cfg, &configError{field: "retention", value: text, err: errNonPositive}
		}
		cfg.Retention = parsed
	}
	return cfg, nil
}

// storageLimit reserves headroom below the configured physical budget so that
// deletion, index maintenance and the WAL still fit while evicting.
func storageLimit(max int64) int64 {
	headroom := max / 20
	if headroom < 4<<20 {
		headroom = 4 << 20
	}
	if half := max / 2; headroom > half {
		headroom = half
	}
	return max - headroom
}

// statusEntry is one remembered capture outcome.
type statusEntry struct {
	id        string
	status    string
	at        time.Time
	expiresAt time.Time
	// evidenceUntil, when set, is how long the outcome is remembered after the
	// snapshot itself is gone. The entry's own expiresAt is always the original
	// snapshot lifetime, so Get never rewrites it when remembering an expiry or
	// eviction.
	evidenceUntil time.Time
}

// retainedUntil reports when the remembered outcome stops being served: the
// explicit evidence lifetime when set, otherwise the snapshot's own expiry.
func (e statusEntry) retainedUntil() time.Time {
	if !e.evidenceUntil.IsZero() {
		return e.evidenceUntil
	}
	return e.expiresAt
}

// statusRing is a small fixed capacity FIFO of recent outcomes. Entries are
// dropped when they age out or when the ring wraps, so metadata about dropped
// requests stays bounded and never lives on disk.
type statusRing struct {
	buf    []statusEntry
	head   int
	size   int
	pushes int
}

// push appends an entry, evicting the oldest one when the ring is full.
func (r *statusRing) push(entry statusEntry, now time.Time) {
	if r.pushes%128 == 0 {
		r.prune(now)
	}
	r.pushes++
	if len(r.buf) < statusRingLimit {
		r.buf = append(r.buf, entry)
		r.size = len(r.buf)
		return
	}
	r.buf[r.head] = entry
	r.head = (r.head + 1) % statusRingLimit
}

// lookup returns the newest unexpired entry for id.
func (r *statusRing) lookup(id string, now time.Time) (statusEntry, bool) {
	for i := r.size - 1; i >= 0; i-- {
		entry := r.buf[(r.head+i)%len(r.buf)]
		if entry.id != id {
			continue
		}
		if !now.Before(entry.retainedUntil()) {
			return statusEntry{}, false
		}
		return entry, true
	}
	return statusEntry{}, false
}

// prune removes aged out entries.
func (r *statusRing) prune(now time.Time) {
	if r.size == 0 {
		return
	}
	kept := make([]statusEntry, 0, r.size)
	for i := 0; i < r.size; i++ {
		entry := r.buf[(r.head+i)%len(r.buf)]
		if now.Before(entry.retainedUntil()) {
			kept = append(kept, entry)
		}
	}
	r.buf = kept
	r.head = 0
	r.size = len(kept)
}
