package credit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRetentionCutoffClampsMonthEnd(t *testing.T) {
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC), time.Date(2026, 2, 28, 10, 0, 0, 0, time.UTC)},
		{time.Date(2024, 5, 31, 10, 0, 0, 0, time.UTC), time.Date(2024, 2, 29, 10, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC), time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 1, 15, 8, 30, 0, 0, time.UTC), time.Date(2025, 10, 15, 8, 30, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		if got := addMonthsClamped(tc.now, -3); !got.Equal(tc.want) {
			t.Fatalf("cutoff for %s = %s, want %s", tc.now, got, tc.want)
		}
	}
}

func TestCleanupPurgesOnlyClosedHistory(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million, Output: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	january := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)

	// One old settled request that must be purged.
	h.clock.set(january)
	old := h.mustReserve(ReserveInput{ID: "old", TraceID: "t-old", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100, OutputEstimate: 100})
	h.mustUsage(usageFor("u-old", old, "m1", tokens(100, 0, 0, 100, 0)))
	h.mustComplete(Completion{ID: old.ID, TraceID: old.TraceID, Outcome: OutcomeSucceeded})

	// One old request that is still unresolved: never purged.
	unresolved := h.mustReserve(ReserveInput{ID: "stuck", TraceID: "t-stuck", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 0})
	h.mustComplete(Completion{ID: unresolved.ID, TraceID: unresolved.TraceID, Outcome: OutcomeSucceeded})

	// One recent settled request that stays inside retention.
	h.clock.set(testEpoch)
	recent := h.mustReserve(ReserveInput{ID: "recent", TraceID: "t-recent", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 0})
	h.mustUsage(usageFor("u-recent", recent, "m1", tokens(10, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: recent.ID, TraceID: recent.TraceID, Outcome: OutcomeSucceeded})

	spentBefore := h.key(key.ID).SpentMicroUSD
	maint, err := h.store.Cleanup(h.ctx())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if maint.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the old settled request)", maint.Deleted)
	}
	var requests, usages int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&requests); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM usages`).Scan(&usages); err != nil {
		t.Fatalf("count usages: %v", err)
	}
	if requests != 2 || usages != 1 {
		t.Fatalf("remaining requests/usages = %d/%d, want 2/1", requests, usages)
	}
	// Durable accounting survives the purge.
	if got := h.key(key.ID).SpentMicroUSD; got != spentBefore {
		t.Fatalf("spent after cleanup = %d, want %d", got, spentBefore)
	}
	june, err := moneyCounter(h.ctx(), h.store.db, key.ID, windowMonth, "2026-01")
	if err != nil {
		t.Fatalf("january counter: %v", err)
	}
	if june != 200 {
		t.Fatalf("january counter = %d, want 200", june)
	}
	// The purged trace must not resurrect a request from a late event.
	late := usageFor("u-late", Request{ID: "old", TraceID: "t-old", KeyID: key.ID}, "m1", tokens(1, 0, 0, 0, 0))
	late.RequestID = "old"
	if err := h.store.RecordUsage(h.ctx(), late); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late usage err = %v, want ErrNotFound", err)
	}
	var after int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&after); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if after != 2 {
		t.Fatalf("requests after late usage = %d, want 2", after)
	}
	// Deleting history must not restore spending power: the key is still at its
	// lifetime limit.
	spec := h.key(key.ID).Spec
	spec.QuotaMicroUSD = spentBefore - 1
	if _, err := h.store.UpdateKey(h.ctx(), key.ID, spec); err != nil {
		t.Fatalf("update key: %v", err)
	}
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "after-purge", TraceID: "t-after",
		KeyID: key.ID, RequestedModel: "m1", InputEstimate: 100}); !errors.Is(err, ErrQuota) {
		t.Fatalf("reserve after purge err = %v, want ErrQuota", err)
	}
	// The unresolved request can still be resolved by an administrator.
	if err := h.store.Resolve(h.ctx(), "stuck", Resolution{Action: ActionRelease, Reason: "manual"}); err != nil {
		t.Fatalf("resolve stuck: %v", err)
	}
}

// TestCleanupReclaimsDatabaseAndWAL verifies that the files actually shrink.
func TestCleanupReclaimsDatabaseAndWAL(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()
	pad := strings.Repeat("p", 400)
	for i := 0; i < 800; i++ {
		id := "bulk-" + itoa(i)
		req := h.mustReserve(ReserveInput{ID: id, TraceID: "t-" + id, KeyID: key.ID, RequestedModel: "m1",
			InputEstimate: 1})
		u := usageFor("u-"+id, req, "m1", tokens(1, 0, 0, 0, 0))
		u.Executor = pad
		u.ResponseModel = pad
		h.mustUsage(u)
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	}
	beforeDB, beforeWAL := h.store.fileSizes()
	if beforeDB == 0 {
		t.Fatal("database file was not created")
	}
	beforeTotal := beforeDB + beforeWAL
	// Move beyond retention so every closed row is eligible.
	h.clock.set(testEpoch.AddDate(0, 6, 0))
	maint, err := h.store.Cleanup(ctx)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if maint.Deleted != 800 {
		t.Fatalf("deleted = %d, want 800", maint.Deleted)
	}
	// Both the database and the WAL must end up smaller than the pre-cleanup
	// footprint; the WAL is truncated to zero.
	if total := maint.DatabaseBytes + maint.WALBytes; total >= beforeTotal {
		t.Fatalf("storage did not shrink: %d -> %d (db %d, wal %d)",
			beforeTotal, total, maint.DatabaseBytes, maint.WALBytes)
	}
	if maint.DatabaseBytes >= beforeDB {
		t.Fatalf("database did not shrink below its loaded size: %d -> %d", beforeDB, maint.DatabaseBytes)
	}
	if maint.WALBytes != 0 {
		t.Fatalf("wal after truncate = %d, want 0", maint.WALBytes)
	}
	var remaining int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("requests remaining = %d, want 0", remaining)
	}
	// Counters are kept even though all history is gone.
	total, err := moneyCounter(ctx, h.store.db, key.ID, windowTotal, "")
	if err != nil {
		t.Fatalf("counter: %v", err)
	}
	if total != 800 {
		t.Fatalf("lifetime counter = %d, want 800", total)
	}
	used, err := tokenCounter(ctx, h.store.db, key.ID, "m1", windowTotal, "")
	if err != nil {
		t.Fatalf("token counter: %v", err)
	}
	if used != 800 {
		t.Fatalf("lifetime tokens = %d, want 800", used)
	}
}

func TestCleanupKeepsCurrentStateWhenNothingExpires(t *testing.T) {
	h := newHarness(t)
	buildDataset(t, h)
	beforeDB, beforeWAL := h.store.fileSizes()
	beforeTotal := beforeDB + beforeWAL
	maint, err := h.store.Cleanup(h.ctx())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if maint.Deleted != 0 {
		t.Fatalf("deleted = %d, want 0", maint.Deleted)
	}
	after := maint.DatabaseBytes + maint.WALBytes
	// A no-op cleanup only moves the WAL into the database; it must not grow
	// the retained footprint.
	if after > beforeTotal {
		t.Fatalf("storage grew without deletions: %d -> %d", beforeTotal, after)
	}
	rep, err := h.store.Report(h.ctx(), Filter{From: testEpoch.AddDate(0, 0, -1), To: testEpoch.AddDate(0, 0, 5)})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Total != 5 {
		t.Fatalf("total after no-op cleanup = %d, want 5", rep.Total)
	}
}

func TestCleanupIsRepeatable(t *testing.T) {
	h := newHarness(t)
	buildDataset(t, h)
	h.clock.set(testEpoch.AddDate(0, 6, 0))
	first, err := h.store.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	second, err := h.store.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if second.Deleted != 0 {
		t.Fatalf("second cleanup deleted %d, want 0", second.Deleted)
	}
	// The pending request has no completion and stays forever until resolved.
	if first.Deleted != 4 {
		t.Fatalf("first cleanup deleted %d, want 4", first.Deleted)
	}
}
