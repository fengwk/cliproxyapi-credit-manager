package capture

import (
	"fmt"
	"testing"
	"time"
)

// TestRetentionExpiryKeepsEvidence verifies lazy expiry: an expired snapshot is
// reported as expired while its row still exists, remains expired right after
// Cleanup removed it, and degrades to unavailable once the retained evidence
// itself ages out instead of claiming expiry forever.
func TestRetentionExpiryKeepsEvidence(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(1<<20, 1<<30), clock, nil)
	enqueue(store, "ttl", []byte("expiring"))
	flush(t, store)

	clock.advance(time.Hour + time.Minute)
	detail, err := store.Get("ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Status != StatusExpired {
		t.Fatalf("status = %q, want %q", detail.Status, StatusExpired)
	}
	if detail.At.IsZero() || detail.ExpiresAt.IsZero() {
		t.Fatalf("expired detail lost its lifetime: %+v", detail)
	}

	if err := store.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if rows := countRows(t, store); rows != 0 {
		t.Fatalf("Cleanup left %d expired rows", rows)
	}
	if got := statusOf(t, store, "ttl"); got != StatusExpired {
		t.Fatalf("status after cleanup = %q, want %q", got, StatusExpired)
	}

	clock.advance(2 * time.Hour)
	if got := statusOf(t, store, "ttl"); got != StatusUnavailable {
		t.Fatalf("status after evidence aged out = %q, want %q", got, StatusUnavailable)
	}
}

// TestCleanupReclaimsDatabaseAndWAL verifies that Cleanup returns freed pages to
// the file system: both the database file and the write-ahead log actually
// shrink, and Stats reports the new sizes.
func TestCleanupReclaimsDatabaseAndWAL(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(64<<10, 1<<30), clock, nil)

	for i := 0; i < 160; i++ {
		enqueue(store, fmt.Sprintf("r-%03d", i), randomBody(t, 16<<10, int64(i)))
		if i%4 == 0 {
			flush(t, store)
		}
	}
	flush(t, store)

	before := store.Stats()
	beforeTotal := before.DatabaseBytes + before.WALBytes
	if beforeTotal < 2<<20 {
		t.Fatalf("store only holds %d bytes; the reclamation test needs real pages", beforeTotal)
	}
	if before.WALBytes <= 0 {
		t.Fatalf("WAL is %d bytes before cleanup, want pending log frames", before.WALBytes)
	}

	clock.advance(2 * time.Hour) // everything expired
	if err := store.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	after := store.Stats()
	afterTotal := after.DatabaseBytes + after.WALBytes
	if after.WALBytes != 0 {
		t.Fatalf("WAL was not truncated: %d -> %d", before.WALBytes, after.WALBytes)
	}
	if afterTotal >= beforeTotal/2 {
		t.Fatalf("database did not shrink: %d -> %d bytes", beforeTotal, afterTotal)
	}
	if rows := countRows(t, store); rows != 0 {
		t.Fatalf("%d rows survived cleanup", rows)
	}
	if got := statusOf(t, store, "r-000"); got != StatusExpired {
		t.Fatalf("status after cleanup = %q, want %q", got, StatusExpired)
	}

	// A second cleanup on an empty database must stay idempotent and cheap.
	if err := store.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if again := store.Stats(); again.DatabaseBytes != after.DatabaseBytes || again.WALBytes != 0 {
		t.Fatalf("second cleanup changed sizes: %+v -> %+v", after, again)
	}
}

// TestStorageBudgetEvictsOldestAndStaysBounded verifies that the physical budget
// (database plus WAL, including indexes) bounds on-disk storage: the oldest
// snapshots are evicted early, the newest survive and the eviction is counted.
func TestStorageBudgetEvictsOldestAndStaysBounded(t *testing.T) {
	clock := newTestClock()
	const budget = 512 << 10
	store, _ := openTestStore(t, testConfig(512<<10, budget), clock, nil)

	// Flush often enough that the byte-bounded queue never overflows: this test
	// is about the storage budget, not about queue pressure.
	for i := 0; i < 400; i++ {
		enqueue(store, fmt.Sprintf("b-%03d", i), randomBody(t, 8<<10, int64(i)))
		if i%4 == 0 {
			flush(t, store)
		}
	}
	flush(t, store)

	stats := store.Stats()
	if used := stats.DatabaseBytes + stats.WALBytes; used > budget {
		t.Fatalf("physical storage %d exceeds the %d byte budget", used, budget)
	}
	if stats.Dropped == 0 {
		t.Fatal("expected snapshots to be dropped under budget pressure")
	}
	if got := statusOf(t, store, "b-399"); got != StatusCaptured {
		t.Fatalf("newest snapshot status = %q, want %q", got, StatusCaptured)
	}
	if got := statusOf(t, store, "b-000"); got != StatusDropped {
		t.Fatalf("oldest snapshot status = %q, want %q", got, StatusDropped)
	}
	// The eviction really removed stored rows: the oldest stored snapshot is no
	// longer the oldest one produced.
	if oldest := oldestStoredID(t, store); oldest == "b-000" {
		t.Fatalf("oldest stored snapshot is still %q; nothing was evicted", oldest)
	}
	if rows := countRows(t, store); rows == 0 {
		t.Fatal("no snapshot survived although 400 were produced well above the budget")
	}

	if err := store.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if used := store.Stats(); used.DatabaseBytes+used.WALBytes > budget {
		t.Fatalf("cleanup left %d bytes, above the %d byte budget", used.DatabaseBytes+used.WALBytes, budget)
	}
	// Once every snapshot is expired the physical footprint collapses, which is
	// what "the budget is a storage limit" has to mean.
	clock.advance(2 * time.Hour)
	if err := store.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if left := store.Stats(); left.DatabaseBytes+left.WALBytes > budget/4 {
		t.Fatalf("after expiry the store still occupies %d bytes", left.DatabaseBytes+left.WALBytes)
	}
}

// TestBudgetEvictionReclaimsDisk verifies that eviction under budget pressure
// releases the disk space it evicted, not just the rows.
func TestBudgetEvictionReclaimsDisk(t *testing.T) {
	clock := newTestClock()
	const budget = 1 << 20
	store, _ := openTestStore(t, testConfig(64<<10, budget), clock, nil)

	for i := 0; i < 80; i++ {
		enqueue(store, fmt.Sprintf("e-%03d", i), randomBody(t, 32<<10, int64(i)))
		flush(t, store)
	}
	flush(t, store)

	peak := store.Stats()
	if total := peak.DatabaseBytes + peak.WALBytes; total > budget {
		t.Fatalf("physical storage %d exceeds the %d byte budget", total, budget)
	}
	if peak.Dropped == 0 {
		t.Fatal("expected evictions while writing 2.5MiB into a 1MiB budget")
	}

	// With everything expired the writer path must give the space back.
	clock.advance(2 * time.Hour)
	if err := store.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	after := store.Stats()
	total := after.DatabaseBytes + after.WALBytes
	if total >= budget/2 {
		t.Fatalf("database did not shrink after eviction: %d bytes remain", total)
	}
	if rows := countRows(t, store); rows != 0 {
		t.Fatalf("%d rows survived cleanup", rows)
	}
}

// TestCleanupKeepsAccurateExpiryMetadata verifies that removing an expired
// snapshot does not rewrite its lifetime: the retained evidence still reports
// the snapshot's original At and ExpiresAt, so an expired snapshot never claims
// a future ExpiresAt, and the evidence still ages out to unavailable.
func TestCleanupKeepsAccurateExpiryMetadata(t *testing.T) {
	clock := newTestClock()
	store, _ := openTestStore(t, testConfig(1<<20, 1<<30), clock, nil)
	enqueue(store, "ttl", []byte("expiring"))
	flush(t, store)

	live, err := store.Get("ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if live.Status != StatusCaptured {
		t.Fatalf("status = %q, want %q", live.Status, StatusCaptured)
	}

	clock.advance(time.Hour + time.Minute)
	expired, err := store.Get("ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if expired.Status != StatusExpired {
		t.Fatalf("status = %q, want %q", expired.Status, StatusExpired)
	}
	if !expired.ExpiresAt.Before(clock.Now()) {
		t.Fatalf("expired detail reports a future ExpiresAt %v (now %v)", expired.ExpiresAt, clock.Now())
	}
	if !expired.At.Equal(live.At) || !expired.ExpiresAt.Equal(live.ExpiresAt) {
		t.Fatalf("lazy expiry changed metadata: %+v -> %+v", live, expired)
	}

	if err := store.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	after, err := store.Get("ttl")
	if err != nil {
		t.Fatalf("Get after cleanup: %v", err)
	}
	if after.Status != StatusExpired {
		t.Fatalf("status after cleanup = %q, want %q", after.Status, StatusExpired)
	}
	if !after.At.Equal(live.At) || !after.ExpiresAt.Equal(live.ExpiresAt) {
		t.Fatalf("cleanup rewrote metadata: %+v -> %+v", live, after)
	}

	clock.advance(2 * time.Hour)
	if got := statusOf(t, store, "ttl"); got != StatusUnavailable {
		t.Fatalf("status after evidence aged out = %q, want %q", got, StatusUnavailable)
	}
}

// TestEvictionKeepsAccurateExpiryMetadata verifies the same invariant on the
// eviction path under storage pressure: an evicted snapshot is reported as
// dropped while still reporting its original lifetime, not the later evidence
// time used to remember the eviction.
func TestEvictionKeepsAccurateExpiryMetadata(t *testing.T) {
	clock := newTestClock()
	const budget = 256 << 10
	store, _ := openTestStore(t, testConfig(64<<10, budget), clock, nil)

	enqueue(store, "victim", randomBody(t, 32<<10, 1))
	flush(t, store)
	live, err := store.Get("victim")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if live.Status != StatusCaptured {
		t.Fatalf("victim status = %q, want %q", live.Status, StatusCaptured)
	}

	// Advance the clock before triggering eviction, so the evidence time
	// (now + retention) differs from the victim's original expiry (At +
	// retention): a rewrite would be observable.
	clock.advance(30 * time.Minute)
	for i := 0; i < 16; i++ {
		enqueue(store, fmt.Sprintf("filler-%02d", i), randomBody(t, 32<<10, int64(i+2)))
		flush(t, store)
		if statusOf(t, store, "victim") == StatusDropped {
			break
		}
	}

	evicted, err := store.Get("victim")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if evicted.Status != StatusDropped {
		t.Fatalf("victim status = %q, want %q", evicted.Status, StatusDropped)
	}
	if !evicted.At.Equal(live.At) || !evicted.ExpiresAt.Equal(live.ExpiresAt) {
		t.Fatalf("eviction rewrote metadata: %+v -> %+v", live, evicted)
	}
}
