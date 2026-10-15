package capture

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClock is a controllable clock. Tests advance it explicitly instead of
// sleeping, so expiry behaviour is deterministic.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// testConfig returns an enabled configuration with the given limits and a one
// hour retention.
func testConfig(maxBody, maxStorage int64) Config {
	return Config{
		Enabled:         true,
		Retention:       time.Hour,
		MaxBodyBytes:    maxBody,
		MaxStorageBytes: maxStorage,
	}
}

// openTestStore creates and starts a store in a temporary directory with a
// controllable clock and an optional writer hook. The store is closed by the
// test cleanup; Close is idempotent so a test may close it explicitly.
func openTestStore(t *testing.T, cfg Config, clock *testClock, hook func()) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := newStore(dir, cfg, storeOptions{now: clock.Now, beforeDequeue: hook})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	store.start()
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store, dir
}

// parkWriter returns a writer hook that parks the writer before its first
// dequeue, a channel that reports the rendezvous and a function that releases
// the writer. The rendezvous notification never blocks the writer, so a released
// writer always drains.
func parkWriter() (hook func(), entered <-chan struct{}, release func()) {
	enteredCh := make(chan struct{}, 1)
	releaseCh := make(chan struct{})
	hook = func() {
		select {
		case enteredCh <- struct{}{}:
		default:
		}
		<-releaseCh
	}
	return hook, enteredCh, func() { close(releaseCh) }
}

// flush waits for the writer to finish every accepted snapshot. It is the only
// synchronization the tests use; they never sleep.
func flush(t *testing.T, store *Store) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		store.waitIdle()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the capture writer")
	}
}

// enqueue is a small helper for snapshot construction.
func enqueue(store *Store, id string, body []byte) {
	store.Enqueue(Snapshot{RequestID: id, Body: body})
}

// statusOf returns the detail status for an id.
func statusOf(t *testing.T, store *Store, id string) string {
	t.Helper()
	detail, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return detail.Status
}

// countRows counts stored capture rows.
func countRows(t *testing.T, store *Store) int {
	t.Helper()
	var rows int
	if err := store.db.QueryRow("SELECT count(*) FROM captures").Scan(&rows); err != nil {
		t.Fatalf("count captures: %v", err)
	}
	return rows
}

// oldestStoredID returns the id with the earliest expiry, which is the snapshot
// the budget evicts first.
func oldestStoredID(t *testing.T, store *Store) string {
	t.Helper()
	var id string
	err := store.db.QueryRow("SELECT id FROM captures ORDER BY expires_at LIMIT 1").Scan(&id)
	if err != nil {
		t.Fatalf("oldest row: %v", err)
	}
	return id
}

// insertRow stores a raw row, used to inject bodies that the writer would never
// produce.
func insertRow(t *testing.T, store *Store, id string, body []byte, expiresAt time.Time) {
	t.Helper()
	at := time.Now().UTC()
	if _, err := store.db.Exec(insertCapture, id, at.UnixNano(), expiresAt.UnixNano(), "", body, len(body), len(body)); err != nil {
		t.Fatalf("insert %q: %v", id, err)
	}
}

// pragmaInt reads a scalar pragma.
func pragmaInt(t *testing.T, store *Store, name string) int64 {
	t.Helper()
	var value int64
	if err := store.db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return value
}

// pragmaText reads a text pragma.
func pragmaText(t *testing.T, store *Store, name string) string {
	t.Helper()
	var value string
	if err := store.db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return value
}

// gzipBytes compresses data for tests that inject stored bodies.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buffer.Bytes()
}

// randomBody returns deterministic, poorly compressible bytes.
func randomBody(t *testing.T, size int, seed int64) []byte {
	t.Helper()
	body := make([]byte, size)
	if _, err := rand.New(rand.NewSource(seed)).Read(body); err != nil {
		t.Fatalf("rand read: %v", err)
	}
	return body
}

// TestDisabledStoreKeepsNoFiles verifies that disabled capture copies nothing,
// creates nothing and reports the documented statuses.
func TestDisabledStoreKeepsNoFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Enqueue(Snapshot{RequestID: "r1", Body: []byte("private")})
	if got := store.Stats(); got != (Stats{}) {
		t.Fatalf("Stats = %+v, want zero", got)
	}
	detail, err := store.Get("r1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Status != StatusDisabled {
		t.Fatalf("status = %q, want %q", detail.Status, StatusDisabled)
	}
	if err := store.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled capture created %d files", len(entries))
	}
}

// TestRoundTripRedactsSecretsAndKeepsCallerMemory verifies capture round trips
// text bodies, redacts credentials, keeps private file permissions and never
// mutates the caller's snapshot.
func TestRoundTripRedactsSecretsAndKeepsCallerMemory(t *testing.T) {
	clock := newTestClock()
	store, dir := openTestStore(t, testConfig(1<<20, 1<<30), clock, nil)

	body := []byte(`{"model":"gpt","api_key":"sk-secret","messages":[{"content":"你好\nworld"}]}`)
	original := append([]byte(nil), body...)
	headers := http.Header{
		"Authorization": {"Bearer sk-secret"},
		"X-Request-Id":  {"req-1"},
	}
	store.Enqueue(Snapshot{RequestID: "req-1", Headers: headers, Body: body})
	flush(t, store)

	detail, err := store.Get("req-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Status != StatusCaptured {
		t.Fatalf("status = %q, want %q", detail.Status, StatusCaptured)
	}
	if got := detail.Headers.Get("Authorization"); got != redactedValue {
		t.Fatalf("Authorization = %q, want %q", got, redactedValue)
	}
	if got := detail.Headers.Get("X-Request-Id"); got != "req-1" {
		t.Fatalf("X-Request-Id = %q, want preserved", got)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(detail.Body), &stored); err != nil {
		t.Fatalf("stored body is not JSON: %v (%q)", err, detail.Body)
	}
	if stored["api_key"] != redactedValue {
		t.Fatalf("stored api_key = %v, want redacted", stored["api_key"])
	}
	messages := stored["messages"].([]any)
	if got := messages[0].(map[string]any)["content"]; got != "你好\nworld" {
		t.Fatalf("message content = %q, want preserved text", got)
	}
	// The caller's memory is forwarded elsewhere and must never be modified.
	if !bytes.Equal(body, original) {
		t.Fatalf("caller body mutated: %q", body)
	}
	if got := headers.Get("Authorization"); got != "Bearer sk-secret" {
		t.Fatalf("caller headers mutated: %q", got)
	}
	wantAt := clock.Now()
	if !detail.At.Equal(wantAt) {
		t.Fatalf("At = %v, want %v", detail.At, wantAt)
	}
	if !detail.ExpiresAt.Equal(wantAt.Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want %v", detail.ExpiresAt, wantAt.Add(time.Hour))
	}

	for _, name := range []string{dbFileName, dbFileName + "-wal"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s permissions = %04o, want 0600", name, perm)
		}
	}
}

// TestNonJSONBodyPreservedExactly verifies that bodies which are not JSON
// documents are stored and returned byte for byte.
func TestNonJSONBodyPreservedExactly(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(1<<20, 1<<30), clock, nil)
	body := []byte("plain text \x00\xff token=secret 你好")
	enqueue(store, "plain", body)
	flush(t, store)
	detail, err := store.Get("plain")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Status != StatusCaptured {
		t.Fatalf("status = %q, want %q", detail.Status, StatusCaptured)
	}
	if detail.Body != string(body) {
		t.Fatalf("body = %q, want %q", detail.Body, body)
	}
}

// TestOversizeBodyIsDroppedBeforeCopying verifies the raw body limit, including
// that the boundary itself is still stored.
func TestOversizeBodyIsDroppedBeforeCopying(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(64, 1<<30), clock, nil)

	enqueue(store, "big", bytes.Repeat([]byte("x"), 65))
	if got := statusOf(t, store, "big"); got != StatusOversize {
		t.Fatalf("oversize status = %q, want %q", got, StatusOversize)
	}
	if stats := store.Stats(); stats.Dropped != 1 {
		t.Fatalf("Dropped = %d, want 1", stats.Dropped)
	}
	if rows := countRows(t, store); rows != 0 {
		t.Fatalf("oversize snapshot was stored (%d rows)", rows)
	}

	enqueue(store, "exact", bytes.Repeat([]byte("y"), 64))
	flush(t, store)
	if got := statusOf(t, store, "exact"); got != StatusCaptured {
		t.Fatalf("boundary status = %q, want %q", got, StatusCaptured)
	}
}

// TestHeaderLimitDropsHeadersKeepsBody verifies that headers are bounded
// independently of the body.
func TestHeaderLimitDropsHeadersKeepsBody(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(1<<20, 1<<30), clock, nil)
	headers := http.Header{}
	headers.Set("X-Huge", strings.Repeat("a", headerLimit+1024))
	store.Enqueue(Snapshot{RequestID: "hdr", Headers: headers, Body: []byte("payload")})
	flush(t, store)

	detail, err := store.Get("hdr")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Status != StatusCaptured {
		t.Fatalf("status = %q, want %q", detail.Status, StatusCaptured)
	}
	if detail.Headers != nil {
		t.Fatalf("oversized headers were kept: %d entries", len(detail.Headers))
	}
	if detail.Body != "payload" {
		t.Fatalf("body = %q, want preserved payload", detail.Body)
	}
}

// TestQueueIsByteBoundedAndDropsSafely verifies that the pending queue is bounded
// in bytes, that overflow is dropped instead of blocking the caller, and that
// the drop is visible through Get and Stats.
func TestQueueIsByteBoundedAndDropsSafely(t *testing.T) {
	clock := newTestClock()
	hook, entered, release := parkWriter()
	store, _ := openTestStore(t, testConfig(3000, 1<<30), clock, hook)
	<-entered // the writer is parked before its first dequeue

	// Fill the queue until the byte bound rejects a snapshot. The writer cannot
	// consume anything while it is parked, so the outcome is deterministic.
	accepted := []string{}
	dropped := ""
	for i := 0; i < 500 && dropped == ""; i++ {
		id := fmt.Sprintf("q-%03d", i)
		before := store.Stats().Dropped
		enqueue(store, id, bytes.Repeat([]byte("a"), 2000))
		if store.Stats().Dropped > before {
			dropped = id
			break
		}
		accepted = append(accepted, id)
	}
	if dropped == "" {
		t.Fatal("the queue never rejected a snapshot")
	}
	if len(accepted) == 0 {
		t.Fatal("the queue rejected every snapshot")
	}

	stats := store.Stats()
	if stats.QueueBytes <= 0 || stats.QueueBytes > store.queueLimit {
		t.Fatalf("QueueBytes = %d, want within (0, %d]", stats.QueueBytes, store.queueLimit)
	}
	if stats.Dropped != 1 {
		t.Fatalf("Dropped = %d, want 1", stats.Dropped)
	}
	if got := statusOf(t, store, dropped); got != StatusDropped {
		t.Fatalf("dropped status = %q, want %q", got, StatusDropped)
	}

	release()
	flush(t, store)
	if got := statusOf(t, store, accepted[0]); got != StatusCaptured {
		t.Fatalf("accepted status = %q, want %q", got, StatusCaptured)
	}
	if got := statusOf(t, store, dropped); got != StatusDropped {
		t.Fatalf("dropped status = %q, want %q", got, StatusDropped)
	}
	if stats := store.Stats(); stats.QueueBytes != 0 {
		t.Fatalf("QueueBytes = %d after drain, want 0", stats.QueueBytes)
	}
}

// TestCloseDrainsQueueAndSurvivesRestart verifies that Close persists every
// accepted snapshot and that stored snapshots survive a restart.
func TestCloseDrainsQueueAndSurvivesRestart(t *testing.T) {
	clock := newTestClock()
	hook, entered, release := parkWriter()
	cfg := testConfig(1<<20, 1<<30)
	store, dir := openTestStore(t, cfg, clock, hook)
	<-entered
	enqueue(store, "p1", []byte("first"))
	enqueue(store, "p2", []byte(`{"api_key":"secret","keep":"me"}`))

	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	release() // let the writer drain the accepted queue
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not drain the accepted queue")
	}

	reopened, err := newStore(dir, cfg, storeOptions{now: clock.Now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopened.start()
	defer reopened.Close()

	first, err := reopened.Get("p1")
	if err != nil {
		t.Fatalf("Get(p1): %v", err)
	}
	if first.Status != StatusCaptured || first.Body != "first" {
		t.Fatalf("p1 = %+v, want captured body first", first)
	}
	second, err := reopened.Get("p2")
	if err != nil {
		t.Fatalf("Get(p2): %v", err)
	}
	if second.Status != StatusCaptured {
		t.Fatalf("p2 status = %q, want %q", second.Status, StatusCaptured)
	}
	if second.Body != `{"api_key":"[redacted]","keep":"me"}` {
		t.Fatalf("p2 body = %q, want redacted and preserved", second.Body)
	}
}

// TestUnknownAndEmptyIDsReportUnavailable verifies that a lookup never invents a
// status for an id that was never captured.
func TestUnknownAndEmptyIDsReportUnavailable(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(1<<20, 1<<30), clock, nil)
	for _, id := range []string{"missing", "req-0", ""} {
		detail, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		if detail.Status != StatusUnavailable {
			t.Fatalf("Get(%q) status = %q, want %q", id, detail.Status, StatusUnavailable)
		}
	}
}

// TestSchemaUsesIncrementalAutoVacuumAndWAL verifies the storage setup that makes
// physical reclamation possible.
func TestSchemaUsesIncrementalAutoVacuumAndWAL(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(1<<20, 1<<30), clock, nil)

	if got := pragmaInt(t, store, "auto_vacuum"); got != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (incremental)", got)
	}
	if got := pragmaText(t, store, "journal_mode"); got != "wal" {
		t.Fatalf("journal_mode = %q, want wal", got)
	}
	if got := pragmaInt(t, store, "user_version"); got != schemaVersion {
		t.Fatalf("user_version = %d, want %d", got, schemaVersion)
	}
	if got := pragmaInt(t, store, "busy_timeout"); got <= 0 {
		t.Fatalf("busy_timeout = %d, want a positive timeout", got)
	}
	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", got)
	}
	if stats := store.Stats(); stats.LastError != "" {
		t.Fatalf("LastError = %q, want empty", stats.LastError)
	}
}

// TestConfigValidationAndDefaults verifies retention validation and defaults.
func TestConfigValidationAndDefaults(t *testing.T) {
	for _, text := range []string{"abc", "-1h", "0s"} {
		if _, err := newStore(t.TempDir(), Config{Enabled: true, RetentionText: text}, storeOptions{}); err == nil {
			t.Fatalf("RetentionText %q was accepted", text)
		}
	}
	if _, err := newStore("", Config{Enabled: true}, storeOptions{}); err == nil {
		t.Fatal("empty data directory was accepted")
	}

	store, err := newStore(t.TempDir(), Config{Enabled: true, RetentionText: "48h"}, storeOptions{})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	defer store.Close()
	if store.cfg.Retention != 48*time.Hour {
		t.Fatalf("Retention = %v, want 48h", store.cfg.Retention)
	}
	if store.cfg.MaxBodyBytes != 10<<20 || store.cfg.MaxStorageBytes != 1<<30 {
		t.Fatalf("limits = %d/%d, want 10MiB/1GiB", store.cfg.MaxBodyBytes, store.cfg.MaxStorageBytes)
	}
	if store.queueLimit != store.cfg.MaxBodyBytes+headerLimit+rowOverhead {
		t.Fatalf("queueLimit = %d, want the body limit plus bounded headers", store.queueLimit)
	}
	if want := int64(1<<30) - (1<<30)/20; store.storageLimit != want {
		t.Fatalf("storageLimit = %d, want %d", store.storageLimit, want)
	}
}

// TestConcurrentEnqueueGetStatsAndClose exercises the lifecycle under the race
// detector: producers, readers and Close must not deadlock, panic or corrupt
// state.
func TestConcurrentEnqueueGetStatsAndClose(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(4<<10, 1<<20), clock, nil)

	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for i := 0; i < 25; i++ {
				id := fmt.Sprintf("c-%d-%d", worker, i)
				store.Enqueue(Snapshot{
					RequestID: id,
					Headers:   http.Header{"Authorization": {"Bearer secret"}, "X-Request-Id": {id}},
					Body:      []byte(fmt.Sprintf(`{"api_key":"secret","i":%d}`, i)),
				})
				_, _ = store.Get(id)
				store.Stats()
			}
		}(worker)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	group.Wait()

	// Post-close use stays safe and observable.
	store.Enqueue(Snapshot{RequestID: "after-close", Body: []byte("x")})
	if stats := store.Stats(); stats.QueueBytes != 0 {
		t.Fatalf("QueueBytes = %d after Close, want 0", stats.QueueBytes)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCorruptAndBombBodiesAreRejected verifies that a damaged stored body is an
// observable error and that inflation is bounded.
func TestCorruptAndBombBodiesAreRejected(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(1024, 1<<30), clock, nil)
	expiry := clock.Now().Add(time.Hour)

	insertRow(t, store, "corrupt", []byte("not a gzip stream"), expiry)
	if _, err := store.Get("corrupt"); err == nil {
		t.Fatal("corrupt body was reported as a capture")
	}

	insertRow(t, store, "bomb", gzipBytes(t, bytes.Repeat([]byte("0"), 1<<20)), expiry)
	if _, err := store.Get("bomb"); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("bomb error = %v, want %v", err, errBodyTooLarge)
	}
	if stats := store.Stats(); stats.LastError == "" {
		t.Fatal("decode failure is not observable in Stats")
	}
}
