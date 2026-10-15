package credit

import (
	"context"
	"database/sql"
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

// TestCleanupPrunesAgedPeriodsButKeepsTotals bounds the period counter tables:
// lifetime totals always stay, aged periods go once nothing can charge them,
// and an unresolved request keeps its own periods alive.
func TestCleanupPrunesAgedPeriodsButKeepsTotals(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million, Output: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()
	january := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	february := time.Date(2026, 2, 5, 9, 0, 0, 0, time.UTC)

	// A closed January request that books January periods and 100 tokens.
	h.clock.set(january)
	closed := h.mustReserve(ReserveInput{ID: "closed", TraceID: "t-closed", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 100})
	h.mustUsage(usageFor("u-closed", closed, "m1", tokens(100, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: closed.ID, TraceID: closed.TraceID, Outcome: OutcomeSucceeded})

	// An unresolved February request keeps its own periods alive.
	h.clock.set(february)
	stuck := h.mustReserve(ReserveInput{ID: "stuck", TraceID: "t-stuck", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 100})
	h.mustUsage(usageFor("u-stuck", stuck, "m1", tokens(100, 0, 0, 0, 0)))
	unknown := usageFor("u-stuck-unknown", stuck, "m1", Tokens{})
	unknown.Quality = QualityUnknown
	h.mustUsage(unknown)
	h.mustComplete(Completion{ID: stuck.ID, TraceID: stuck.TraceID, Outcome: OutcomeSucceeded})

	assertCounter := func(win, period string, want int64) {
		t.Helper()
		got, err := moneyCounter(ctx, h.store.db, key.ID, win, period)
		if err != nil {
			t.Fatalf("money counter %s/%s: %v", win, period, err)
		}
		if got != want {
			t.Fatalf("money counter %s/%s = %d, want %d", win, period, got, want)
		}
	}
	// Before cleanup both periods are recorded.
	assertCounter(windowDay, dayPeriod(january), 100)
	assertCounter(windowWeek, weekPeriod(january), 100)
	assertCounter(windowMonth, monthPeriod(january), 100)
	assertCounter(windowDay, dayPeriod(february), 100)
	assertCounter(windowTotal, "", 200)

	h.clock.set(testEpoch) // cutoff = 2026-03-15
	maint, err := h.store.Cleanup(ctx)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if maint.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (the closed January request)", maint.Deleted)
	}
	// Aged periods with no unresolved reference are gone.
	assertCounter(windowDay, dayPeriod(january), 0)
	assertCounter(windowWeek, weekPeriod(january), 0)
	assertCounter(windowMonth, monthPeriod(january), 0)
	// Periods an unresolved request may still charge are kept.
	assertCounter(windowDay, dayPeriod(february), 100)
	assertCounter(windowWeek, weekPeriod(february), 100)
	assertCounter(windowMonth, monthPeriod(february), 100)
	// The lifetime total is the durable record and survives everything.
	assertCounter(windowTotal, "", 200)

	tokenTotal, err := tokenCounter(ctx, h.store.db, key.ID, "m1", windowTotal, "")
	if err != nil {
		t.Fatalf("token total: %v", err)
	}
	if tokenTotal != 200 {
		t.Fatalf("lifetime tokens = %d, want 200", tokenTotal)
	}
	janTokens, err := tokenCounter(ctx, h.store.db, key.ID, "m1", windowDay, dayPeriod(january))
	if err != nil {
		t.Fatalf("january tokens: %v", err)
	}
	if janTokens != 0 {
		t.Fatalf("january token counter = %d, want pruned", janTokens)
	}

	// A late charge for the unresolved request still lands in its own period.
	if err := h.store.Resolve(ctx, stuck.ID, Resolution{Action: ActionCharge, AmountMicroUSD: 40,
		Reason: "late usage"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	assertCounter(windowDay, dayPeriod(february), 140)
	assertCounter(windowTotal, "", 240)
}

// TestCleanupWithoutRunKeepsPeriodHistory proves pruning only happens through
// Cleanup: a report and the counters stay untouched otherwise.
func TestCleanupWithoutRunKeepsPeriodHistory(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	january := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	h.clock.set(january)
	req := h.mustReserve(ReserveInput{ID: "old", TraceID: "t-old", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100})
	h.mustUsage(usageFor("u-old", req, "m1", tokens(100, 0, 0, 0, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	h.clock.set(testEpoch)

	// No cleanup ran: the aged history and its counters are intact.
	got, err := moneyCounter(h.ctx(), h.store.db, key.ID, windowMonth, monthPeriod(january))
	if err != nil || got != 100 {
		t.Fatalf("january counter without cleanup = %d, %v", got, err)
	}
	rep, err := h.store.Report(h.ctx(), Filter{From: january.Add(-time.Hour), To: testEpoch, PageSize: 10})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Total != 1 {
		t.Fatalf("total without cleanup = %d, want 1", rep.Total)
	}
}

// TestCleanupSurfacesBusyCheckpoint proves a blocked WAL truncation is reported
// instead of being presented as successful maintenance.
func TestCleanupSurfacesBusyCheckpoint(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()
	// History is needed so the WAL actually holds pages to fold back.
	for i := 0; i < 5; i++ {
		id := "r" + itoa(i)
		req := h.mustReserve(ReserveInput{ID: id, TraceID: "t-" + id, KeyID: key.ID,
			RequestedModel: "m1", InputEstimate: 1})
		h.mustUsage(usageFor("u-"+id, req, "m1", tokens(1, 0, 0, 0, 0)))
		h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	}

	// A second connection holds a read transaction open on the WAL.
	reader, err := sql.Open(driverName, dsnFor(h.store.dbPath))
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer reader.Close()
	reader.SetMaxOpenConns(1)
	conn, err := reader.Conn(ctx)
	if err != nil {
		t.Fatalf("reader conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("begin reader: %v", err)
	}
	var count int64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`).Scan(&count); err != nil {
		t.Fatalf("reader query: %v", err)
	}
	if count != 5 {
		t.Fatalf("reader sees %d requests, want 5", count)
	}

	if _, err := h.store.Cleanup(ctx); err == nil {
		t.Fatal("cleanup reported success while a reader blocked the WAL truncation")
	} else if !strings.Contains(err.Error(), "busy") {
		t.Fatalf("cleanup err = %v, want a busy checkpoint error", err)
	}
	// Once the reader releases the WAL, maintenance succeeds again.
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("commit reader: %v", err)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("close reader conn: %v", err)
	}
	if _, err := h.store.Cleanup(ctx); err != nil {
		t.Fatalf("cleanup after the reader left: %v", err)
	}
}

// assertHoldAligned checks the ledger invariant that survived this bug: the key
// hold always equals the holds of the requests that are still open.
func assertHoldAligned(t *testing.T, h *harness, keyID string) {
	t.Helper()
	held := h.key(keyID).HeldMicroUSD
	sum, err := counterValue(context.Background(), h.store.db,
		`SELECT COALESCE(SUM(held_micro_usd),0) FROM requests WHERE key_id = ? AND state IN (?,?)`,
		keyID, stateActive, statePending)
	if err != nil {
		t.Fatalf("sum unresolved holds: %v", err)
	}
	if held != sum {
		t.Fatalf("key hold %d does not match unresolved requests %d", held, sum)
	}
}

// TestCleanupKeepsRequestReopenedByLateExecution reproduces the removal race:
// candidates are collected, a late execution reopens a request and restores its
// hold, and the delete path must then leave that request, its history and the
// hold alone.
func TestCleanupKeepsRequestReopenedByLateExecution(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	ctx := h.ctx()
	old := testEpoch.AddDate(0, -4, 0)

	// A settled request the retention window has already passed.
	req := h.mustReserve(ReserveInput{ID: "old", TraceID: "t-old", KeyID: key.ID,
		RequestedModel: "m1", InputEstimate: 5, At: old})
	first := usageFor("u-old", req, "m1", tokens(5, 0, 0, 0, 0))
	first.At = old
	h.mustUsage(first)
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded, At: old})
	spent := h.key(key.ID).SpentMicroUSD
	assertHoldAligned(t, h, key.ID)

	cutoff := h.store.retentionFrom(h.clock.now())
	stale, err := expiredRequestIDs(ctx, h.store.db, cutoff)
	if err != nil {
		t.Fatalf("expired candidates: %v", err)
	}
	if len(stale) != 1 || stale[0] != "old" {
		t.Fatalf("candidates = %v, want [old]", stale)
	}

	// The late execution arrives after the candidate list was collected.
	late := usageFor("u-late", req, "m1", Tokens{})
	late.Quality = QualityUnknown
	h.mustUsage(late)
	if got := h.request(req.ID); got.State != statePending || got.HeldMicroUSD != 5 {
		t.Fatalf("reopened request = %+v, want pending with the hold restored", got)
	}
	assertHoldAligned(t, h, key.ID)

	// Deleting the stale list must not touch the reopened request.
	var deleted int64
	if err := h.store.inTx(ctx, func(tx *sql.Tx) error {
		n, err := deleteHistory(ctx, tx, stale, cutoff)
		deleted = n
		return err
	}); err != nil {
		t.Fatalf("delete stale batch: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted %d requests, want the reopened one kept", deleted)
	}
	kept := func(stage string) {
		t.Helper()
		if got := h.request(req.ID); got.State != statePending || got.HeldMicroUSD != 5 {
			t.Fatalf("%s: request = %+v, want the pending accounting kept", stage, got)
		}
		n, err := counterValue(ctx, h.store.db, `SELECT COUNT(*) FROM usages WHERE request_id = ?`, req.ID)
		if err != nil || n != 2 {
			t.Fatalf("%s: usages = %d (%v), want both retained", stage, n, err)
		}
		assertHoldAligned(t, h, key.ID)
	}
	kept("after the stale delete")

	// A full maintenance run behaves the same way.
	maintenance, err := h.store.Cleanup(ctx)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if maintenance.Deleted != 0 {
		t.Fatalf("cleanup deleted %d, want nothing while the request is unresolved", maintenance.Deleted)
	}
	kept("after cleanup")

	// Once an administrator closes it the history is collectable, and the key
	// hold still agrees with the requests that remain.
	if err := h.store.Resolve(ctx, req.ID, Resolution{Action: ActionRelease, Reason: "operator"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	maintenance, err = h.store.Cleanup(ctx)
	if err != nil {
		t.Fatalf("cleanup after resolve: %v", err)
	}
	if maintenance.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", maintenance.Deleted)
	}
	if n, err := counterValue(ctx, h.store.db, `SELECT COUNT(*) FROM requests WHERE id = ?`, req.ID); err != nil || n != 0 {
		t.Fatalf("request survived cleanup: %d (%v)", n, err)
	}
	if n, err := counterValue(ctx, h.store.db, `SELECT COUNT(*) FROM usages WHERE request_id = ?`, req.ID); err != nil || n != 0 {
		t.Fatalf("executions survived cleanup: %d (%v)", n, err)
	}
	assertHoldAligned(t, h, key.ID)
	if k := h.key(key.ID); k.HeldMicroUSD != 0 || k.SpentMicroUSD != spent {
		t.Fatalf("ledger after cleanup = %+v, want held 0 and spent %d", k, spent)
	}
}
