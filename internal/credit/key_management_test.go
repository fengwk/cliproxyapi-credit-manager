package credit

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const testModel = "m1"

// chargeKey admits, charges and settles one fully accounted execution for a key.
// It is the smallest unit that moves every durable counter the reset baseline
// touches, so the reset tests can build usage deterministically.
func chargeKey(h *harness, key Key, model, suffix string, amount int64, at time.Time) {
	h.t.Helper()
	req := h.mustAdmit(AdmitInput{
		ID: "req-" + suffix, TraceID: "trace-" + suffix, KeyID: key.ID,
		RequestedModel: model, At: at})
	h.mustUsage(Usage{
		ID: "u-" + suffix, TraceID: req.TraceID, KeyID: key.ID, Model: model,
		Raw:    pluginapi.UsageDetail{InputTokens: amount, TotalTokens: amount},
		Tokens: tokens(amount, 0, 0, 0, 0), Quality: QualityComplete, At: at})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded, At: at})
}

// A created key can be revealed exactly once per call, returns the original
// material, and the plaintext never leaks through the read models.
func TestRevealRoundTripAndNoLeak(t *testing.T) {
	h := newHarness(t)
	materialA := "sk-reveal-alpha-3f9c2a7e"
	materialB := "sk-reveal-bravo-8d1e4b6a"
	keyA, gotA := h.mustKey(KeySpec{Enabled: true, Material: materialA, CallerID: ""})
	keyB, gotB := h.mustKey(KeySpec{Enabled: true, Material: materialB})
	if gotA != materialA || gotB != materialB {
		t.Fatalf("create returned %q/%q", gotA, gotB)
	}
	revealed, err := h.store.RevealKey(h.ctx(), keyA.ID)
	if err != nil || revealed != materialA {
		t.Fatalf("reveal A = %q %v", revealed, err)
	}
	revealed, err = h.store.RevealKey(h.ctx(), keyB.ID)
	if err != nil || revealed != materialB {
		t.Fatalf("reveal B = %q %v", revealed, err)
	}
	// The list/get projection must never carry either plaintext, the ciphertext
	// or the other key's material.
	keys, err := h.store.ListKeys(h.ctx())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{materialA, materialB} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("list projection leaked %q: %s", secret, raw)
		}
	}
	if strings.Contains(string(raw), "material_enc") {
		t.Fatalf("list projection exposed ciphertext field: %s", raw)
	}
	// Revealing one key never returns a different key's material.
	if revealed, err := h.store.RevealKey(h.ctx(), keyA.ID); err != nil || revealed == materialB {
		t.Fatalf("reveal A returned %q %v", revealed, err)
	}
}

// The material is sealed with AES-256-GCM derived from the pepper: it is stored
// as ciphertext (never the plaintext) and a different pepper cannot open it.
func TestKeyMaterialCiphertextAtRestAndPepperIntegrity(t *testing.T) {
	h := newHarness(t)
	material := "sk-atrest-4f9c2a7e-secret-value"
	key, _ := h.mustKey(KeySpec{Enabled: true, Material: material})

	var blob []byte
	if err := h.store.db.QueryRow(`SELECT key_material_enc FROM keys WHERE id = ?`, key.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if len(blob) == 0 {
		t.Fatal("stored key material is empty")
	}
	if bytes.Contains(blob, []byte(material)) {
		t.Fatalf("stored blob contains the plaintext")
	}
	// The durable files themselves must not contain the plaintext either.
	for _, path := range []string{h.store.dbPath, h.store.walPath} {
		if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, []byte(material)) {
			t.Fatalf("plaintext found at rest in %s", filepath.Base(path))
		}
	}
	// A tampered blob is rejected rather than decrypted into a wrong value.
	tampered := bytes.Clone(blob)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := decryptMaterial(h.store.pepper, tampered); err == nil {
		t.Fatal("tampered ciphertext decrypted successfully")
	}
	// A different pepper must not open a blob sealed with the real pepper.
	other := bytes.Clone(h.store.pepper)
	other[0] ^= 0xff
	if _, err := decryptMaterial(other, blob); err == nil {
		t.Fatal("material decrypted under the wrong pepper")
	}
	// The pepper is persisted: the same material is recoverable after reopen.
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{DataDir: h.dir, Now: h.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got, err := reopened.RevealKey(h.ctx(), key.ID); err != nil || got != material {
		t.Fatalf("reopen reveal = %q %v", got, err)
	}
}

// The encrypted material is NOT NULL: a key row can never be written without
// it, so the ciphertext is guaranteed to exist for every key the store creates.
func TestKeyMaterialColumnRejectsNull(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	if _, err := h.store.db.Exec(`UPDATE keys SET key_material_enc = NULL WHERE id = ?`, key.ID); err == nil {
		t.Fatal("key_material_enc accepted NULL")
	}
}

// A corrupted ciphertext must be refused without fabricating a plaintext and
// without leaving an audit row behind, so a damaged blob is never disclosed and
// never recorded as a successful disclosure.
func TestRevealCorruptMaterialConflictsWithoutAudit(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true, Material: "sk-corrupt-5c1b-secret"})
	var blob []byte
	if err := h.store.db.QueryRow(`SELECT key_material_enc FROM keys WHERE id = ?`, key.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(blob)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := h.store.db.Exec(`UPDATE keys SET key_material_enc = ? WHERE id = ?`, tampered, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RevealKey(h.ctx(), key.ID); err == nil {
		t.Fatal("corrupt material was revealed")
	}
	var audits int64
	if err := h.store.db.QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE key_id = ? AND action = ?`, key.ID, ActionReveal).
		Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Fatalf("corrupt reveal wrote %d audit rows, want 0", audits)
	}
	// The key stays usable: only disclosure is impossible until it is rotated.
	if _, err := h.store.Authenticate(h.ctx(), "sk-corrupt-5c1b-secret"); err != nil {
		t.Fatalf("stored material stopped authenticating: %v", err)
	}
}

// Rotation keeps the identity and scope stable and never reveals the new
// material through the read models.
func TestRotateKeepsIdentityAndScope(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	before := h.key(key.ID)
	rotated, material, err := h.store.RotateKey(h.ctx(), key.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ID != before.ID || rotated.Principal != before.Principal || rotated.Scope != before.Scope {
		t.Fatalf("rotate changed identity: %+v -> %+v", before, rotated)
	}
	byScope, err := h.store.KeyByScope(h.ctx(), before.Scope)
	if err != nil || byScope.ID != before.ID {
		t.Fatalf("scope lookup after rotate = %+v %v", byScope, err)
	}
	if byScope.Scope != callerScope(before.Principal) {
		t.Fatalf("scope is no longer the principal's trusted scope")
	}
	if _, err := h.store.Authenticate(h.ctx(), material); err != nil {
		t.Fatal(err)
	}
}

// Soft delete permanently disables authentication, re-enable and rotation while
// preserving the records, durable token counters and the ability to account for
// in flight work.
func TestSoftDeleteRejectsAndSettles(t *testing.T) {
	h := newHarness(t)
	h.mustPrice(testModel, Rates{Input: 1_000_000})
	key, material := h.mustKey(KeySpec{Enabled: true})
	// An unresolved execution admitted before deletion must still be able to
	// settle afterwards.
	req := h.mustAdmit(AdmitInput{ID: "req-inflight", TraceID: "trace-inflight",
		KeyID: key.ID, RequestedModel: testModel})

	if err := h.store.DeleteKey(h.ctx(), key.ID, "compromised"); err != nil {
		t.Fatal(err)
	}
	// Idempotent repeat.
	if err := h.store.DeleteKey(h.ctx(), key.ID, "again"); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
	// Records are preserved and marked.
	deleted := h.key(key.ID)
	if deleted.DeletedAt == nil || deleted.Spec.Enabled {
		t.Fatalf("deleted key = %+v", deleted)
	}
	// A deleted key rejects authentication, re-enable, rotation and admission.
	if _, err := h.store.Authenticate(h.ctx(), material); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("deleted key authenticated: %v", err)
	}
	if _, err := h.store.UpdateKey(h.ctx(), key.ID, KeySpec{Enabled: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-enable err = %v, want conflict", err)
	}
	if _, _, err := h.store.RotateKey(h.ctx(), key.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotate err = %v, want conflict", err)
	}
	if _, err := h.store.Admit(h.ctx(), AdmitInput{ID: "req-new", TraceID: "trace-new",
		KeyID: key.ID, RequestedModel: testModel}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("admission on deleted key err = %v, want unauthorized", err)
	}
	if _, err := h.store.RevealKey(h.ctx(), key.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("reveal deleted err = %v, want conflict", err)
	}
	// The scope lookup still resolves so in flight accounting can settle.
	byScope, err := h.store.KeyByScope(h.ctx(), key.Scope)
	if err != nil || byScope.DeletedAt == nil {
		t.Fatalf("scope lookup of deleted key = %+v %v", byScope, err)
	}
	// Settle the in flight request after deletion: the actual tokens count.
	h.mustUsage(Usage{ID: "u-inflight", TraceID: req.TraceID, KeyID: key.ID, Model: testModel,
		Raw:    pluginapi.UsageDetail{InputTokens: 100, TotalTokens: 100},
		Tokens: tokens(100, 0, 0, 0, 0), Quality: QualityComplete})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if used, err := tokenCounter(h.ctx(), h.store.db, key.ID, testModel, windowTotal, ""); err != nil || used != 100 {
		t.Fatalf("deleted key token counter = %d (%v), want 100", used, err)
	}
	if cost, unpriced := h.costOf(key.ID); cost != 100 || unpriced != 0 {
		t.Fatalf("derived cost after deletion = %d/%d, want 100", cost, unpriced)
	}
	keys, err := h.store.ListKeys(h.ctx())
	if err != nil || len(keys) != 1 || keys[0].DeletedAt == nil {
		t.Fatalf("deleted key not listed: %+v %v", keys, err)
	}
}

// Resetting a window stores a baseline and never rewrites durable counters, so
// late tokens still count and the true lifetime total is preserved.
func TestResetBaselinePeriodRolloverLateUsageCleanup(t *testing.T) {
	h := newHarness(t)
	h.mustPrice(testModel, Rates{Input: 1_000_000})
	key, material := h.mustKey(KeySpec{Enabled: true})
	day1 := h.clock.now()
	chargeKey(h, key, testModel, "a", 100, day1)
	readCounter := func(win, period string) int64 {
		t.Helper()
		got, err := tokenCounter(h.ctx(), h.store.db, key.ID, testModel, win, period)
		if err != nil {
			t.Fatalf("token counter %s/%s: %v", win, period, err)
		}
		return got
	}
	readBaseline := func(win, period string) int64 {
		t.Helper()
		got, err := baselineToken(h.ctx(), h.store.db, key.ID, testModel, win, period)
		if err != nil {
			t.Fatalf("token baseline %s/%s: %v", win, period, err)
		}
		return got
	}
	if got := readCounter(windowTotal, ""); got != 100 {
		t.Fatalf("tokens after first charge = %d", got)
	}
	reset, err := h.store.ResetTokens(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Daily: true, Reason: "goodwill"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reset.Windows) != 2 {
		t.Fatalf("reset windows = %v, want total and day", reset.Windows)
	}
	// The durable counters are untouched; only the effective view is cleared.
	if got := readCounter(windowTotal, ""); got != 100 {
		t.Fatalf("durable total after reset = %d, want 100", got)
	}
	if got := readBaseline(windowTotal, ""); got != 100 {
		t.Fatalf("total baseline = %d, want 100", got)
	}
	if got := readBaseline(windowDay, dayPeriod(day1)); got != 100 {
		t.Fatalf("day baseline = %d, want 100", got)
	}
	// A late execution for work admitted on day1 arrives after day1 rolled
	// over. It is counted in the admission period, and the day1 baseline makes
	// only the excess effective.
	late := h.mustAdmit(AdmitInput{ID: "req-late", TraceID: "trace-late",
		KeyID: key.ID, RequestedModel: testModel, At: day1})
	h.clock.advance(24 * time.Hour)
	h.mustUsage(Usage{ID: "u-late", TraceID: late.TraceID, KeyID: key.ID, Model: testModel,
		Raw:    pluginapi.UsageDetail{InputTokens: 50, TotalTokens: 50},
		Tokens: tokens(50, 0, 0, 0, 0), Quality: QualityComplete, At: h.clock.now()})
	h.mustComplete(Completion{ID: late.ID, TraceID: late.TraceID, Outcome: OutcomeSucceeded, At: h.clock.now()})
	if got := readCounter(windowDay, dayPeriod(day1)); got != 150 {
		t.Fatalf("day1 durable counter = %d, want 150", got)
	}
	if got := readCounter(windowTotal, ""); got != 150 {
		t.Fatalf("lifetime counter = %d, want 150", got)
	}
	if effective, err := effectiveToken(h.ctx(), h.store.db, key.ID, testModel, windowTotal, "", 150); err != nil || effective != 50 {
		t.Fatalf("effective lifetime = %d (%v), want the 50 late tokens", effective, err)
	}
	// A new period starts with no day baseline: today's usage counts from zero.
	chargeKey(h, key, testModel, "c", 30, h.clock.now())
	if got := readCounter(windowDay, dayPeriod(h.clock.now())); got != 30 {
		t.Fatalf("day2 durable counter = %d, want 30 (the late usage belongs to day1)", got)
	}
	if got := readCounter(windowTotal, ""); got != 180 {
		t.Fatalf("lifetime counter = %d, want 180", got)
	}
	if effective, err := effectiveToken(h.ctx(), h.store.db, key.ID, testModel, windowDay, dayPeriod(h.clock.now()), 30); err != nil || effective != 30 {
		t.Fatalf("effective day2 = %d (%v), want the unbaselined 30", effective, err)
	}
	// A repeated reset only raises the baseline and can never go negative.
	for range 2 {
		if _, err := h.store.ResetTokens(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Reason: "repeat"}); err != nil {
			t.Fatal(err)
		}
		if got := readBaseline(windowTotal, ""); got != 180 {
			t.Fatalf("repeated reset baseline = %d, want 180", got)
		}
		if effective, err := effectiveToken(h.ctx(), h.store.db, key.ID, testModel, windowTotal, "", 180); err != nil || effective != 0 {
			t.Fatalf("repeated reset effective = %d (%v), want 0", effective, err)
		}
	}
	// Rotation keeps the baseline keyed by key id and the identity stable.
	rotated, _, err := h.store.RotateKey(h.ctx(), key.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Scope != key.Scope {
		t.Fatalf("rotate changed identity: %+v", rotated)
	}
	if got := readBaseline(windowTotal, ""); got != 180 {
		t.Fatalf("baseline after rotate = %d, want 180", got)
	}
	if _, err := h.store.Authenticate(h.ctx(), material); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old material still valid after rotate: %v", err)
	}
	// Cleanup deletes disposable history but never the lifetime counter or its
	// baseline, so the effective view is preserved.
	h.clock.advance(120 * 24 * time.Hour)
	if _, err := h.store.Cleanup(h.ctx()); err != nil {
		t.Fatal(err)
	}
	if got := readCounter(windowTotal, ""); got != 180 {
		t.Fatalf("lifetime counter after cleanup = %d, want 180", got)
	}
	if effective, err := effectiveToken(h.ctx(), h.store.db, key.ID, testModel, windowTotal, "", 180); err != nil || effective != 0 {
		t.Fatalf("effective after cleanup = %d (%v), want 0", effective, err)
	}
}

// A dollar budget never gates admission; only token limits and token resets
// do. The four budget fields are display metadata retained on the spec.
func TestMoneyBudgetsNeverGateAdmission(t *testing.T) {
	h := newHarness(t)
	h.mustPrice(testModel, Rates{Input: 1_000_000})
	key, _ := h.mustKey(KeySpec{Enabled: true, QuotaMicroUSD: 1,
		DailyMicroUSD: 1, WeeklyMicroUSD: 1, MonthlyMicroUSD: 1})
	at := h.clock.now()
	chargeKey(h, key, testModel, "a", 1_000_000, at)
	for i, id := range []string{"second", "third"} {
		if _, err := h.store.Admit(h.ctx(), AdmitInput{ID: id, TraceID: id,
			KeyID: key.ID, RequestedModel: testModel, At: at}); err != nil {
			t.Fatalf("request %d denied by a display budget: %v", i+1, err)
		}
	}
	// Derived cost exceeds every configured budget, and that is report-only.
	if cost, unpriced := h.costOf(key.ID); cost != 1_000_000 || unpriced != 0 {
		t.Fatalf("derived cost = %d/%d, want the priced million tokens", cost, unpriced)
	}
}

// Token budgets gate admission and token resets reopen their headroom without
// erasing the durable counters.
func TestResetTokenBudget(t *testing.T) {
	h := newHarness(t)
	h.mustPrice(testModel, Rates{Input: 1_000_000})
	key, _ := h.mustKey(KeySpec{Enabled: true,
		ModelTokenLimits: []ModelTokenLimit{{Model: testModel, Total: 100}}})
	at := h.clock.now()
	chargeKey(h, key, testModel, "a", 100, at)
	if _, err := h.store.Admit(h.ctx(), AdmitInput{ID: "blocked", TraceID: "blocked",
		KeyID: key.ID, RequestedModel: testModel, At: at}); !errors.Is(err, ErrQuota) {
		t.Fatalf("token limit not enforced: %v", err)
	}
	if _, err := h.store.ResetTokens(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Reason: "reset"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Admit(h.ctx(), AdmitInput{ID: "allowed", TraceID: "allowed",
		KeyID: key.ID, RequestedModel: testModel, At: at}); err != nil {
		t.Fatalf("token reset did not reopen budget: %v", err)
	}
	// Charging the reopened budget back to the limit denies the next request.
	chargeKey(h, key, testModel, "b", 100, at)
	if _, err := h.store.Admit(h.ctx(), AdmitInput{ID: "blocked-again", TraceID: "blocked-again",
		KeyID: key.ID, RequestedModel: testModel, At: at}); !errors.Is(err, ErrQuota) {
		t.Fatalf("re-accumulated tokens not enforced after reset: %v", err)
	}
	// The durable lifetime counter still reflects every real token.
	if got, err := tokenCounter(h.ctx(), h.store.db, key.ID, testModel, windowTotal, ""); err != nil || got != 200 {
		t.Fatalf("lifetime tokens = %d (%v), want the durable 200", got, err)
	}
}

// A caller is attribution metadata only: it never changes identity or
// authentication, and CRUD maintains it.
func TestCallerCRUDAndAttribution(t *testing.T) {
	h := newHarness(t)
	caller, err := h.store.CreateCaller(h.ctx(), CallerSpec{DisplayName: "Acme", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if caller.ID == "" {
		t.Fatal("caller id was not generated")
	}
	if _, err := h.store.CreateCaller(h.ctx(), CallerSpec{ID: caller.ID, DisplayName: "dup"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate caller err = %v, want conflict", err)
	}
	key, material := h.mustKey(KeySpec{Enabled: true, CallerID: caller.ID})
	if key.CallerID != caller.ID || h.key(key.ID).CallerID != caller.ID {
		t.Fatalf("caller attribution not stored: %+v", key)
	}
	// Unknown attribution is refused.
	if _, _, err := h.store.CreateKey(h.ctx(), KeySpec{Enabled: true, CallerID: "missing"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown caller create err = %v, want invalid", err)
	}
	if _, err := h.store.UpdateKey(h.ctx(), key.ID, KeySpec{Enabled: true, CallerID: "missing"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown caller update err = %v, want invalid", err)
	}
	// Disabling the caller is metadata only: the key still authenticates and its
	// scope is unchanged.
	if _, err := h.store.UpdateCaller(h.ctx(), CallerSpec{ID: caller.ID, DisplayName: "Acme Ltd", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Authenticate(h.ctx(), material); err != nil {
		t.Fatalf("caller change affected authentication: %v", err)
	}
	if got := h.key(key.ID); got.Scope != key.Scope {
		t.Fatalf("caller change altered scope: %+v", got)
	}
	if _, err := h.store.UpdateCaller(h.ctx(), CallerSpec{ID: "missing", DisplayName: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown caller update err = %v, want not found", err)
	}
	callers, err := h.store.ListCallers(h.ctx())
	if err != nil || len(callers) != 1 || callers[0].DisplayName != "Acme Ltd" || callers[0].Enabled {
		t.Fatalf("callers = %+v %v", callers, err)
	}
}

// The audit trail records every management action, supports key/caller filters
// and bounded paging, and never carries key material.
func TestAuditTrailFiltersAndPaging(t *testing.T) {
	h := newHarness(t)
	caller, err := h.store.CreateCaller(h.ctx(), CallerSpec{DisplayName: "Acme", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	material := "sk-audit-1f2e3d4c"
	key, _ := h.mustKey(KeySpec{Enabled: true, Material: material, CallerID: caller.ID})
	if _, err := h.store.UpdateKey(h.ctx(), key.ID, KeySpec{Enabled: true, CallerID: caller.ID, Label: "l"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.RotateKey(h.ctx(), key.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RevealKey(h.ctx(), key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ResetTokens(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Reason: "audit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpdateCaller(h.ctx(), CallerSpec{ID: caller.ID, DisplayName: "Acme 2", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeleteKey(h.ctx(), key.ID, "cleanup"); err != nil {
		t.Fatal(err)
	}
	page, err := h.store.Audit(h.ctx(), AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 8 {
		t.Fatalf("audit total = %d, want 8: %+v", page.Total, page.Items)
	}
	// Newest first and no plaintext anywhere in the trail.
	raw, _ := json.Marshal(page)
	if bytes.Contains(raw, []byte(material)) {
		t.Fatalf("audit leaked material: %s", raw)
	}
	if page.Items[0].Action != ActionDelete {
		t.Fatalf("audit not newest first: %+v", page.Items[0])
	}
	byKey, err := h.store.Audit(h.ctx(), AuditFilter{KeyID: key.ID, PageSize: 100})
	if err != nil || byKey.Total != 6 {
		t.Fatalf("audit by key = %d %v", byKey.Total, err)
	}
	for _, item := range byKey.Items {
		if item.KeyID != key.ID {
			t.Fatalf("filter leaked another key: %+v", item)
		}
	}
	byCaller, err := h.store.Audit(h.ctx(), AuditFilter{CallerID: caller.ID, PageSize: 100})
	if err != nil || byCaller.Total != 8 {
		t.Fatalf("audit by caller = %d %v", byCaller.Total, err)
	}
	// Paging is bounded and consistent.
	first, err := h.store.Audit(h.ctx(), AuditFilter{Page: 1, PageSize: 3})
	if err != nil || len(first.Items) != 3 || first.PageSize != 3 {
		t.Fatalf("first page = %+v %v", first, err)
	}
	last, err := h.store.Audit(h.ctx(), AuditFilter{Page: 3, PageSize: 3})
	if err != nil || len(last.Items) != 2 {
		t.Fatalf("last page = %+v %v", last, err)
	}
	// Limit and an oversized page size both clamp to the bounded page.
	capped, err := h.store.Audit(h.ctx(), AuditFilter{Limit: 100000})
	if err != nil || capped.PageSize != maxPageSize {
		t.Fatalf("limit not bounded: %+v %v", capped, err)
	}
	if _, err := h.store.Audit(h.ctx(), AuditFilter{Page: math.MaxInt, PageSize: maxPageSize}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflow page err = %v, want invalid", err)
	}
}

// A failed action leaves no partial baseline and no phantom audit event.
func TestResetAndDeleteAreAuditTransactional(t *testing.T) {
	h := newHarness(t)
	h.mustPrice(testModel, Rates{Input: 1_000_000})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	chargeKey(h, key, testModel, "a", 100, h.clock.now())
	before, err := h.store.Audit(h.ctx(), AuditFilter{PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ResetTokens(h.ctx(), ResetSpec{IDs: []string{key.ID, "missing"},
		Total: true, Reason: "partial"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial reset err = %v, want not found", err)
	}
	after, err := h.store.Audit(h.ctx(), AuditFilter{PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if after.Total != before.Total {
		t.Fatalf("failed reset wrote audit: %d -> %d", before.Total, after.Total)
	}
	if got, err := tokenCounter(h.ctx(), h.store.db, key.ID, testModel, windowTotal, ""); err != nil || got != 100 {
		t.Fatalf("failed reset changed tokens = %d (%v), want 100", got, err)
	}
	if got, err := baselineToken(h.ctx(), h.store.db, key.ID, testModel, windowTotal, ""); err != nil || got != 0 {
		t.Fatalf("failed reset wrote a baseline = %d (%v), want none", got, err)
	}
	if err := h.store.DeleteKey(h.ctx(), "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
	final, _ := h.store.Audit(h.ctx(), AuditFilter{PageSize: 100})
	if final.Total != before.Total {
		t.Fatalf("failed delete wrote audit: %d", final.Total)
	}
}
