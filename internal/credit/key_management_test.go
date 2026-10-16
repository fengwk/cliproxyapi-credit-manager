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
	req := h.mustReserve(ReserveInput{
		ID: "req-" + suffix, TraceID: "trace-" + suffix, KeyID: key.ID,
		RequestedModel: model, InputEstimate: amount, At: at,
	})
	h.mustUsage(Usage{
		ID: "u-" + suffix, TraceID: req.TraceID, KeyID: key.ID, Model: model,
		Raw:    pluginapi.UsageDetail{InputTokens: amount, TotalTokens: amount},
		Tokens: tokens(amount, 0, 0, 0, 0), Quality: QualityComplete, At: at,
	})
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

// A key without recoverable material (any pre-existing ledger) must report a
// conflict and a rotate requirement instead of fabricating a plaintext.
func TestRevealLegacyKeyConflicts(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	// Simulates a v1 key row: verifier present, no encrypted material.
	if _, err := h.store.db.Exec(`UPDATE keys SET key_material_enc = NULL WHERE id = ?`, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RevealKey(h.ctx(), key.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy reveal err = %v, want conflict", err)
	}
	// Rotating installs recoverable material and keeps the accepted material.
	rotated, newMaterial, err := h.store.RotateKey(h.ctx(), key.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ID != key.ID || rotated.Principal != key.Principal || rotated.Scope != key.Scope {
		t.Fatalf("rotate changed identity: %+v", rotated)
	}
	if revealed, err := h.store.RevealKey(h.ctx(), key.ID); err != nil || revealed != newMaterial {
		t.Fatalf("reveal after rotate = %q %v", revealed, err)
	}
	if _, err := h.store.Authenticate(h.ctx(), newMaterial); err != nil {
		t.Fatalf("rotated material rejected: %v", err)
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
// preserving the records, the ledger and the ability to settle in flight work.
func TestSoftDeleteRejectsAndSettles(t *testing.T) {
	h := newHarness(t)
	h.mustPrice(testModel, Rates{Input: 1_000_000})
	key, material := h.mustKey(KeySpec{Enabled: true})
	// An unresolved execution admitted before deletion must still be able to
	// settle afterwards.
	req := h.mustReserve(ReserveInput{ID: "req-inflight", TraceID: "trace-inflight",
		KeyID: key.ID, RequestedModel: testModel, InputEstimate: 100})

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
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "req-new", TraceID: "trace-new",
		KeyID: key.ID, RequestedModel: testModel, InputEstimate: 10}); !errors.Is(err, ErrUnauthorized) {
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
	// Settle the in flight request after deletion: the charge counts and the
	// hold is released.
	h.mustUsage(Usage{ID: "u-inflight", TraceID: req.TraceID, KeyID: key.ID, Model: testModel,
		Raw:    pluginapi.UsageDetail{InputTokens: 100, TotalTokens: 100},
		Tokens: tokens(100, 0, 0, 0, 0), Quality: QualityComplete})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	settled := h.key(key.ID)
	if settled.SpentMicroUSD != 100 || settled.HeldMicroUSD != 0 {
		t.Fatalf("deleted key ledger not settled: %+v", settled)
	}
	keys, err := h.store.ListKeys(h.ctx())
	if err != nil || len(keys) != 1 || keys[0].DeletedAt == nil {
		t.Fatalf("deleted key not listed: %+v %v", keys, err)
	}
}

// Resetting a window stores a baseline and never rewrites durable counters, so
// a late charge still counts and the true lifetime total is preserved.
func TestResetBaselinePeriodRolloverLateUsageCleanup(t *testing.T) {
	h := newHarness(t)
	h.mustPrice(testModel, Rates{Input: 1_000_000})
	key, material := h.mustKey(KeySpec{Enabled: true})
	day1 := h.clock.now()
	chargeKey(h, key, testModel, "a", 100, day1)
	if got := h.key(key.ID).SpentMicroUSD; got != 100 {
		t.Fatalf("spent after first charge = %d", got)
	}
	if _, err := h.store.ResetSpend(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Daily: true, Reason: "goodwill"}); err != nil {
		t.Fatal(err)
	}
	// The durable total is untouched; only the effective view is cleared.
	effective := h.key(key.ID)
	if effective.SpentMicroUSD != 100 || effective.QuotaUsedMicroUSD != 0 || effective.DailyUsedMicroUSD != 0 {
		t.Fatalf("after reset = %+v", effective)
	}
	// A late charge for work admitted on day1 arrives after day1 rolled over.
	late := h.mustReserve(ReserveInput{ID: "req-late", TraceID: "trace-late",
		KeyID: key.ID, RequestedModel: testModel, InputEstimate: 50, At: day1})
	h.clock.advance(24 * time.Hour)
	h.mustUsage(Usage{ID: "u-late", TraceID: late.TraceID, KeyID: key.ID, Model: testModel,
		Raw:    pluginapi.UsageDetail{InputTokens: 50, TotalTokens: 50},
		Tokens: tokens(50, 0, 0, 0, 0), Quality: QualityComplete, At: h.clock.now()})
	h.mustComplete(Completion{ID: late.ID, TraceID: late.TraceID, Outcome: OutcomeSucceeded, At: h.clock.now()})
	// It is booked into the day1 counter but the day1 baseline still applies to
	// the lifetime view, so the effective total grows by exactly the late charge.
	if got := h.key(key.ID); got.SpentMicroUSD != 150 || got.QuotaUsedMicroUSD != 50 {
		t.Fatalf("late charge not counted: %+v", got)
	}
	var day1Counter int64
	if err := h.store.db.QueryRow(`SELECT micro_usd FROM key_money_counters WHERE key_id = ? AND win = 'day' AND period = ?`,
		key.ID, dayPeriod(day1)).Scan(&day1Counter); err != nil {
		t.Fatal(err)
	}
	if day1Counter != 150 {
		t.Fatalf("day1 durable counter = %d, want 150", day1Counter)
	}
	// A new period starts with no baseline: today's usage counts from zero.
	chargeKey(h, key, testModel, "c", 30, h.clock.now())
	got := h.key(key.ID)
	if got.SpentMicroUSD != 180 || got.QuotaUsedMicroUSD != 80 || got.DailyUsedMicroUSD != 30 {
		t.Fatalf("after rollover = %+v", got)
	}
	// A repeated reset only raises the baseline and can never go negative.
	for range 2 {
		if _, err := h.store.ResetSpend(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Reason: "repeat"}); err != nil {
			t.Fatal(err)
		}
		if got := h.key(key.ID); got.QuotaUsedMicroUSD != 0 || got.SpentMicroUSD != 180 {
			t.Fatalf("repeated reset = %+v", got)
		}
	}
	// Rotation keeps the baseline keyed by key id and the identity stable.
	rotated, _, err := h.store.RotateKey(h.ctx(), key.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Scope != key.Scope || rotated.QuotaUsedMicroUSD != 0 || rotated.SpentMicroUSD != 180 {
		t.Fatalf("rotate changed reset view: %+v", rotated)
	}
	if _, err := h.store.Authenticate(h.ctx(), material); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old material still valid after rotate: %v", err)
	}
	// Cleanup deletes disposable history but never the lifetime counter or its
	// baseline, so the effective total survives.
	h.clock.advance(120 * 24 * time.Hour)
	if _, err := h.store.Cleanup(h.ctx()); err != nil {
		t.Fatal(err)
	}
	after := h.key(key.ID)
	if after.SpentMicroUSD != 180 || after.QuotaUsedMicroUSD != 0 {
		t.Fatalf("after cleanup = %+v", after)
	}
	var totalCounter int64
	if err := h.store.db.QueryRow(`SELECT micro_usd FROM key_money_counters WHERE key_id = ? AND win = 'total'`,
		key.ID).Scan(&totalCounter); err != nil {
		t.Fatal(err)
	}
	if totalCounter != 180 {
		t.Fatalf("lifetime counter = %d, want 180", totalCounter)
	}
}

// A reset reopens quota headroom but never forgives the holds of unresolved
// requests, so two concurrent admissions cannot jointly exceed the limit.
func TestResetBaselineReopensQuotaButKeepsHolds(t *testing.T) {
	h := newHarness(t)
	h.mustPrice(testModel, Rates{Input: 1_000_000})
	key, _ := h.mustKey(KeySpec{Enabled: true, QuotaMicroUSD: 100})
	at := h.clock.now()
	chargeKey(h, key, testModel, "a", 100, at)
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "blocked", TraceID: "blocked",
		KeyID: key.ID, RequestedModel: testModel, InputEstimate: 100, At: at}); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota not enforced: %v", err)
	}
	if _, err := h.store.ResetSpend(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Reason: "reset"}); err != nil {
		t.Fatal(err)
	}
	// Effective usage is now zero, so one hold fits.
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "first", TraceID: "first",
		KeyID: key.ID, RequestedModel: testModel, InputEstimate: 100, At: at}); err != nil {
		t.Fatalf("reset did not reopen quota: %v", err)
	}
	// The hold from the first admission still occupies the budget.
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "second", TraceID: "second",
		KeyID: key.ID, RequestedModel: testModel, InputEstimate: 100, At: at}); !errors.Is(err, ErrQuota) {
		t.Fatalf("hold not counted after reset: %v", err)
	}
}

// Token budgets honour the same reset baselines as money budgets.
func TestResetTokenBudget(t *testing.T) {
	h := newHarness(t)
	h.mustPrice(testModel, Rates{Input: 1_000_000})
	key, _ := h.mustKey(KeySpec{Enabled: true,
		ModelTokenLimits: []ModelTokenLimit{{Model: testModel, Total: 100}}})
	at := h.clock.now()
	chargeKey(h, key, testModel, "a", 100, at)
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "blocked", TraceID: "blocked",
		KeyID: key.ID, RequestedModel: testModel, InputEstimate: 100, At: at}); !errors.Is(err, ErrQuota) {
		t.Fatalf("token limit not enforced: %v", err)
	}
	if _, err := h.store.ResetSpend(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Reason: "reset"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "allowed", TraceID: "allowed",
		KeyID: key.ID, RequestedModel: testModel, InputEstimate: 100, At: at}); err != nil {
		t.Fatalf("token reset did not reopen budget: %v", err)
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
	if _, err := h.store.ResetSpend(h.ctx(), ResetSpec{KeyID: key.ID, Total: true, Reason: "audit"}); err != nil {
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
	if _, err := h.store.ResetSpend(h.ctx(), ResetSpec{IDs: []string{key.ID, "missing"},
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
	if got := h.key(key.ID); got.QuotaUsedMicroUSD != 100 {
		t.Fatalf("failed reset changed use: %+v", got)
	}
	if err := h.store.DeleteKey(h.ctx(), "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
	final, _ := h.store.Audit(h.ctx(), AuditFilter{PageSize: 100})
	if final.Total != before.Total {
		t.Fatalf("failed delete wrote audit: %d", final.Total)
	}
}
