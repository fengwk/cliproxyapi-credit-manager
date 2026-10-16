package credit

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestExpiryRejectsUnrepresentableInstants pins the persistence boundary of
// expires_at: the column stores int64 nanoseconds since the Unix epoch, so an
// instant whose UnixNano() would overflow (year 2500) or whose zero value is
// the year-1 zero Time must be rejected instead of being silently stored as a
// wrapped past timestamp or as NULL (which means "never expires").
func TestExpiryRejectsUnrepresentableInstants(t *testing.T) {
	h := newHarness(t)

	// Instants inside the int64 nanosecond range survive the round trip
	// exactly, including both ends of the representable window.
	accepted := []time.Time{
		time.Date(2026, time.June, 15, 12, 0, 0, 0, time.UTC),
		time.Date(1678, time.January, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2262, time.April, 11, 23, 47, 16, 854775807, time.UTC),
	}
	for _, want := range accepted {
		key, _, err := h.store.CreateKey(h.ctx(), KeySpec{Enabled: true, ExpiresAt: &want})
		if err != nil {
			t.Fatalf("create with expiry %s: %v", want, err)
		}
		if key.Spec.ExpiresAt == nil {
			t.Fatalf("expiry %s was persisted as unlimited", want)
		}
		if !key.Spec.ExpiresAt.Equal(want) {
			t.Fatalf("expiry round trip = %s, want %s", key.Spec.ExpiresAt, want)
		}
		if got := h.key(key.ID).Spec.ExpiresAt; got == nil || !got.Equal(want) {
			t.Fatalf("reloaded expiry = %v, want %s", got, want)
		}
	}

	// Year 0001 is the zero Time (used to become an unlimited NULL expiry) and
	// year 2500 overflows int64 nanoseconds (used to become a wrong past time).
	zero := time.Time{}
	overflow := time.Date(2500, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, bad := range []time.Time{zero, overflow} {
		if _, _, err := h.store.CreateKey(h.ctx(), KeySpec{Enabled: true, ExpiresAt: &bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("create with expiry %s err = %v, want ErrInvalid", bad, err)
		}
	}
	// A nil expiry is genuinely different: it is still accepted and means no
	// expiry at all.
	if key, _, err := h.store.CreateKey(h.ctx(), KeySpec{Enabled: true}); err != nil || key.Spec.ExpiresAt != nil {
		t.Fatalf("nil expiry: expiry=%v err=%v, want accepted without expiry", key.Spec.ExpiresAt, err)
	}

	// A rejected update must not write anything: the stored expiry and the
	// key's usability stay exactly as they were.
	key, material := h.mustKey(KeySpec{Enabled: true})
	spec := key.Spec
	spec.ExpiresAt = &overflow
	if _, err := h.store.UpdateKey(h.ctx(), key.ID, spec); !errors.Is(err, ErrInvalid) {
		t.Fatalf("update with unrepresentable expiry err = %v, want ErrInvalid", err)
	}
	if got := h.key(key.ID).Spec.ExpiresAt; got != nil {
		t.Fatalf("rejected update stored expiry %s", got)
	}
	if _, err := h.store.Authenticate(h.ctx(), material); err != nil {
		t.Fatalf("key became unusable after a rejected update: %v", err)
	}
}

// TestAuthenticateHonoursRepresentableExpiry proves the expiry that is persisted
// is really enforced in both directions, so a representable expiry is never
// accidentally treated as unlimited and a far future one is never treated as
// already past by an overflowed instant.
func TestAuthenticateHonoursRepresentableExpiry(t *testing.T) {
	h := newHarness(t)
	past := testEpoch.Add(-time.Hour)
	expired, expiredMaterial := h.mustKey(KeySpec{Enabled: true, ExpiresAt: &past})
	if _, err := h.store.Authenticate(h.ctx(), expiredMaterial); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("past expiry err = %v, want ErrUnauthorized", err)
	}
	if got := h.key(expired.ID).Spec.ExpiresAt; got == nil || !got.Equal(past) {
		t.Fatalf("past expiry round trip = %v, want %s", got, past)
	}

	future := time.Date(2262, time.April, 11, 23, 47, 16, 854775807, time.UTC)
	_, futureMaterial := h.mustKey(KeySpec{Enabled: true, ExpiresAt: &future})
	if _, err := h.store.Authenticate(h.ctx(), futureMaterial); err != nil {
		t.Fatalf("far future expiry must still authenticate: %v", err)
	}
}

// TestOpenRejectsWhitespaceDataDir keeps the configured path verbatim: trimming
// a padded path would silently relocate the ledger into a different directory
// than the one the operator and the health/capture views point at.
func TestOpenRejectsWhitespaceDataDir(t *testing.T) {
	base := t.TempDir()
	padded := []string{
		"",
		" ",
		"   ",
		"\t",
		filepath.Join(base, "data "),
		" " + filepath.Join(base, "data"),
	}
	for _, dir := range padded {
		s, err := Open(Options{DataDir: dir})
		if s != nil {
			_ = s.Close()
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Open(%q) err = %v, want ErrInvalid", dir, err)
		}
	}
	// The trim target of the padded inputs must never have been created.
	if _, err := os.Stat(filepath.Join(base, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a whitespace data dir created a sibling ledger: %v", err)
	}

	// A normal path, including directory names with embedded spaces, works and
	// is used verbatim.
	good := filepath.Join(base, "ledger dir", "nested dir")
	s, err := Open(Options{DataDir: good})
	if err != nil {
		t.Fatalf("open path with embedded spaces: %v", err)
	}
	defer s.Close()
	for _, name := range []string{dbFileName, pepperFileName} {
		if _, err := os.Stat(filepath.Join(good, name)); err != nil {
			t.Fatalf("missing %s in %s: %v", name, good, err)
		}
	}
}

// TestReportRejectsOffsetOverflow pins that an extreme page whose OFFSET would
// wrap negative is rejected. SQLite reads a negative OFFSET as zero, so the old
// arithmetic returned the first page under a huge page id instead of failing.
func TestReportRejectsOffsetOverflow(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: million, Output: million})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
	from, to := testEpoch.Add(-time.Hour), testEpoch.Add(time.Hour)

	// (MaxInt64-1)*500 overflows int64; with at least one stored request the
	// wrapped negative offset used to be read as the first page.
	if _, err := h.store.Report(h.ctx(), Filter{From: from, To: to, Page: math.MaxInt64, PageSize: maxPageSize}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflowing page err = %v, want ErrInvalid", err)
	}

	// The largest page whose offset still fits in int64 is a safe, empty page.
	size := maxPageSize
	page := int(math.MaxInt64/size) + 1
	rep, err := h.store.Report(h.ctx(), Filter{From: from, To: to, Page: page, PageSize: size})
	if err != nil {
		t.Fatalf("safe extreme page: %v", err)
	}
	if len(rep.Items) != 0 || rep.Total != 1 || rep.Page != page || rep.PageSize != size {
		t.Fatalf("safe extreme page = items %d total %d page %d size %d", len(rep.Items), rep.Total, rep.Page, rep.PageSize)
	}
}
