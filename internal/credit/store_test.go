package credit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// testEpoch is a fixed UTC clock origin; tests never sleep.
var testEpoch = time.Date(2026, time.June, 15, 12, 0, 0, 0, time.UTC)

type testClock struct{ ns atomic.Int64 }

func newClock(t time.Time) *testClock {
	c := &testClock{}
	c.set(t)
	return c
}

func (c *testClock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) set(t time.Time)         { c.ns.Store(t.UTC().UnixNano()) }
func (c *testClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

type harness struct {
	t     *testing.T
	clock *testClock
	store *Store
	dir   string
}

func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	clock := newClock(testEpoch)
	dir := t.TempDir()
	o := Options{DataDir: dir, Now: clock.now}
	for _, f := range opts {
		f(&o)
	}
	s, err := Open(o)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &harness{t: t, clock: clock, store: s, dir: dir}
}

func (h *harness) ctx() context.Context { return context.Background() }

func (h *harness) mustKey(spec KeySpec) (Key, string) {
	h.t.Helper()
	k, material, err := h.store.CreateKey(h.ctx(), spec)
	if err != nil {
		h.t.Fatalf("create key: %v", err)
	}
	return k, material
}

func (h *harness) mustPrice(model string, rates Rates) {
	h.t.Helper()
	if err := h.store.PutPrice(h.ctx(), Price{Model: model, Rates: rates}); err != nil {
		h.t.Fatalf("put price: %v", err)
	}
}

func (h *harness) mustReserve(in ReserveInput) Request {
	h.t.Helper()
	if in.At.IsZero() {
		in.At = h.clock.now()
	}
	req, err := h.store.Reserve(h.ctx(), in)
	if err != nil {
		h.t.Fatalf("reserve %s: %v", in.ID, err)
	}
	return req
}

func (h *harness) mustUsage(u Usage) {
	h.t.Helper()
	if u.At.IsZero() {
		u.At = h.clock.now()
	}
	if err := h.store.RecordUsage(h.ctx(), u); err != nil {
		h.t.Fatalf("record usage %s: %v", u.ID, err)
	}
}

func (h *harness) mustComplete(c Completion) {
	h.t.Helper()
	if c.At.IsZero() {
		c.At = h.clock.now()
	}
	if err := h.store.Complete(h.ctx(), c); err != nil {
		h.t.Fatalf("complete %s: %v", c.ID, err)
	}
}

func (h *harness) key(id string) Key {
	h.t.Helper()
	k, err := h.store.GetKey(h.ctx(), id)
	if err != nil {
		h.t.Fatalf("get key %s: %v", id, err)
	}
	return k
}

func (h *harness) request(id string) Request {
	h.t.Helper()
	for _, item := range h.report("").Items {
		if item.ID == id {
			return item
		}
	}
	h.t.Fatalf("request %s not found in report", id)
	return Request{}
}

// report reads the full test-window report, optionally narrowed to one key.
func (h *harness) report(keyID string) Report {
	h.t.Helper()
	rep, err := h.store.Report(h.ctx(), Filter{
		KeyID:    keyID,
		From:     testEpoch.AddDate(-1, 0, 0),
		To:       h.clock.now().Add(time.Hour),
		Page:     1,
		PageSize: 200,
	})
	if err != nil {
		h.t.Fatalf("report: %v", err)
	}
	return rep
}

func tokens(in, cr, cw, out, reasoning int64) Tokens {
	return Tokens{
		Input:      in,
		CacheRead:  cr,
		CacheWrite: cw,
		Output:     out,
		Reasoning:  reasoning,
		Total:      in + cr + cw + out + reasoning,
	}
}

// usageFor builds a plausible complete execution event.
func usageFor(id string, req Request, model string, tk Tokens) Usage {
	return Usage{
		ID:        id,
		RequestID: req.ID,
		TraceID:   req.TraceID,
		KeyID:     req.KeyID,
		Provider:  "openai",
		AuthID:    "auth-1",
		Model:     model,
		Raw:       pluginapi.UsageDetail{InputTokens: tk.Input, OutputTokens: tk.Output, TotalTokens: tk.Total},
		Tokens:    tk,
		Quality:   QualityComplete,
	}
}

func TestOpenCreatesDatabaseAndPepper(t *testing.T) {
	h := newHarness(t)
	info, err := os.Stat(filepath.Join(h.dir, dbFileName))
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("database file is empty")
	}
	pepper, err := os.Stat(filepath.Join(h.dir, pepperFileName))
	if err != nil {
		t.Fatalf("stat pepper: %v", err)
	}
	// The pepper must never be group/world readable.
	if perm := pepper.Mode().Perm(); perm != 0o600 {
		t.Fatalf("pepper permissions = %o, want 600", perm)
	}
	var mode string
	if err := h.store.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode = %q, want wal", mode)
	}
	var autoVacuum int
	if err := h.store.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		t.Fatalf("read auto_vacuum: %v", err)
	}
	if autoVacuum == 0 {
		t.Fatal("auto_vacuum is disabled; retention cleanup could not reclaim space")
	}
	// The ledger is the durable record: an acknowledged commit must survive
	// power loss even though the host event queue is memory only.
	var sync int
	if err := h.store.db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatalf("read synchronous: %v", err)
	}
	if sync != 2 {
		t.Fatalf("synchronous = %d, want 2 (FULL)", sync)
	}
}

// TestRestartMarksActivePending verifies a restart neither frees holds nor
// silently settles interrupted executions.
func TestRestartMarksActivePending(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: 1_000_000, Output: 2_000_000})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	h.mustReserve(ReserveInput{ID: "req-1", TraceID: "trace-1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 100, OutputEstimate: 100})

	if err := h.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(Options{DataDir: h.dir, Now: h.clock.now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	req := Request{}
	rep, err := reopened.Report(context.Background(), Filter{From: testEpoch.AddDate(-1, 0, 0),
		To: h.clock.now().Add(time.Hour), PageSize: 10})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(rep.Items))
	}
	req = rep.Items[0]
	if req.State != statePending {
		t.Fatalf("state after restart = %q, want %q", req.State, statePending)
	}
	if rep.Summary.Pending != 1 {
		t.Fatalf("pending summary = %d, want 1", rep.Summary.Pending)
	}
	k, err := reopened.GetKey(context.Background(), key.ID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	// The hold survives a restart; interrupted work is never free.
	if k.HeldMicroUSD != req.HeldMicroUSD || k.HeldMicroUSD == 0 {
		t.Fatalf("held after restart = %d, request hold = %d", k.HeldMicroUSD, req.HeldMicroUSD)
	}
	if k.SpentMicroUSD != 0 {
		t.Fatalf("spent after restart = %d, want 0", k.SpentMicroUSD)
	}
	// A late usage can still close the restarted request.
	h2 := &harness{t: t, clock: h.clock, store: reopened, dir: h.dir}
	h2.mustUsage(usageFor("usage-1", req, "m1", tokens(100, 0, 0, 100, 0)))
	h2.mustComplete(Completion{ID: "req-1", TraceID: "trace-1", Outcome: OutcomeSucceeded, At: h.clock.now()})
	settled := h2.request("req-1")
	if settled.State != stateSettled {
		t.Fatalf("state = %q, want settled", settled.State)
	}
}

func TestKeyMaterialIsHashedAndPermissionsEnforced(t *testing.T) {
	h := newHarness(t)
	key, material := h.mustKey(KeySpec{Label: "team-a", Enabled: true})
	if len(material) != len(materialPrefix)+materialChars {
		t.Fatalf("material length = %d, want %d", len(material), len(materialPrefix)+materialChars)
	}
	if key.Spec.Material != "" {
		t.Fatal("returned KeySpec must not carry material")
	}
	got, err := h.store.Authenticate(h.ctx(), material)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != key.ID || got.Principal != key.Principal || got.Scope != key.Scope {
		t.Fatalf("authenticated key mismatch: %+v", got)
	}
	if _, err := h.store.Authenticate(h.ctx(), material+"x"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong material err = %v, want ErrUnauthorized", err)
	}
	// Raw material must not be stored anywhere in the database file.
	raw, err := os.ReadFile(filepath.Join(h.dir, dbFileName))
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if contains(raw, []byte(material)) {
		t.Fatal("plaintext key material found in the database file")
	}
	// Disabling the key stops authentication.
	spec := key.Spec
	spec.Enabled = false
	if _, err := h.store.UpdateKey(h.ctx(), key.ID, spec); err != nil {
		t.Fatalf("disable key: %v", err)
	}
	if _, err := h.store.Authenticate(h.ctx(), material); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled key err = %v, want ErrUnauthorized", err)
	}
	// Expiry also rejects.
	spec.Enabled = true
	expired := testEpoch.Add(-time.Minute)
	spec.ExpiresAt = &expired
	if _, err := h.store.UpdateKey(h.ctx(), key.ID, spec); err != nil {
		t.Fatalf("expire key: %v", err)
	}
	if _, err := h.store.Authenticate(h.ctx(), material); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired key err = %v, want ErrUnauthorized", err)
	}
}

func contains(haystack, needle []byte) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

func TestKeyRotationAndDuplicates(t *testing.T) {
	h := newHarness(t)
	key, material := h.mustKey(KeySpec{Enabled: true})
	rotated, fresh, err := h.store.RotateKey(h.ctx(), key.ID, "")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if fresh == material {
		t.Fatal("rotation reused the previous material")
	}
	if rotated.Principal != key.Principal || rotated.Scope != key.Scope {
		t.Fatal("rotation changed the stable principal/scope")
	}
	if _, err := h.store.Authenticate(h.ctx(), material); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old material err = %v, want ErrUnauthorized", err)
	}
	if _, err := h.store.Authenticate(h.ctx(), fresh); err != nil {
		t.Fatalf("new material: %v", err)
	}
	// Rotating onto the current material is a conflict, not a silent no-op.
	if _, _, err := h.store.RotateKey(h.ctx(), key.ID, fresh); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat rotation err = %v, want ErrConflict", err)
	}
	// Custom material is accepted verbatim.
	custom := "sk-custom-material-0001"
	if _, got, err := h.store.RotateKey(h.ctx(), key.ID, custom); err != nil || got != custom {
		t.Fatalf("custom rotate = %q, %v", got, err)
	}
	other, _ := h.mustKey(KeySpec{Enabled: true})
	if _, _, err := h.store.RotateKey(h.ctx(), other.ID, custom); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate material err = %v, want ErrConflict", err)
	}
	if _, _, err := h.store.CreateKey(h.ctx(), KeySpec{Material: custom, Enabled: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("create duplicate material err = %v, want ErrConflict", err)
	}
	if _, err := h.store.GetKey(h.ctx(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key err = %v, want ErrNotFound", err)
	}
	byScope, err := h.store.KeyByScope(h.ctx(), key.Scope)
	if err != nil || byScope.ID != key.ID {
		t.Fatalf("key by scope = %v, %v", byScope.ID, err)
	}
	byPrincipal, err := h.store.KeyByPrincipal(h.ctx(), key.Principal)
	if err != nil || byPrincipal.ID != key.ID {
		t.Fatalf("key by principal = %v, %v", byPrincipal.ID, err)
	}
	if _, err := h.store.Authenticate(h.ctx(), ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty material err = %v, want ErrUnauthorized", err)
	}
}

func TestListKeysIncludesLimits(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{
		Enabled:       true,
		AllowedModels: []string{"m1"},
		ModelTokenLimits: []ModelTokenLimit{
			{Model: "m1", Total: 1000, Daily: 10},
		},
	})
	keys, err := h.store.ListKeys(h.ctx())
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(keys))
	}
	if got := keys[0].Spec.ModelTokenLimits; len(got) != 1 || got[0].Model != "m1" || got[0].Daily != 10 {
		t.Fatalf("token limits = %+v", got)
	}
	if keys[0].Spec.Material != "" {
		t.Fatal("listed spec must not carry material")
	}
	if keys[0].ID != key.ID {
		t.Fatalf("listed id = %s, want %s", keys[0].ID, key.ID)
	}
}

func TestNormalizeSpecRejectsInvalid(t *testing.T) {
	cases := []KeySpec{
		{QuotaMicroUSD: -1},
		{MaxConcurrent: -1},
		{AllowedModels: []string{"*"}},
		{ModelTokenLimits: []ModelTokenLimit{{Model: "m", Total: -5}}},
		{ModelTokenLimits: []ModelTokenLimit{{Model: "m"}, {Model: "m"}}},
	}
	for i, spec := range cases {
		if _, err := normalizeSpec(spec); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d err = %v, want ErrInvalid", i, err)
		}
	}
	spec, err := normalizeSpec(KeySpec{AllowedModels: []string{" m1 ", "m1", "m2"}})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if fmt.Sprint(spec.AllowedModels) != "[m1 m2]" {
		t.Fatalf("allowed models = %v", spec.AllowedModels)
	}
}

func TestPriceValidationAndVersioning(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("m1", Rates{Input: 10})
	prices, err := h.store.ListPrices(h.ctx())
	if err != nil || len(prices) != 1 {
		t.Fatalf("list prices = %v, %v", prices, err)
	}
	if prices[0].Version != 1 {
		t.Fatalf("version = %d, want 1", prices[0].Version)
	}
	h.mustPrice("m1", Rates{Input: 20})
	prices, _ = h.store.ListPrices(h.ctx())
	if prices[0].Version != 2 || prices[0].Rates.Input != 20 {
		t.Fatalf("repriced = %+v", prices[0])
	}
	if err := h.store.PutPrice(h.ctx(), Price{Model: "m2", Rates: Rates{Input: -1}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative rate err = %v, want ErrInvalid", err)
	}
	if err := h.store.PutPrice(h.ctx(), Price{Model: "  ", Rates: Rates{}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty model err = %v, want ErrInvalid", err)
	}
}

func TestCostRoundsUpOnce(t *testing.T) {
	price := Price{Model: "m", Rates: Rates{Input: 1_000_000, Output: 3_000_000, CacheRead: 100_000}}
	// 1 input token at 1 microUSD/1M plus 1 cache-read token at 0.1 = 1.1 -> 2.
	cost, err := computeCost(price, tokens(1, 1, 0, 0, 0))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if cost != 2 {
		t.Fatalf("cost = %d, want 2 (single ceiling over the sum)", cost)
	}
	// Reasoning falls back to the output rate when unset.
	reasoning, err := computeCost(price, tokens(0, 0, 0, 0, 1_000_000))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if reasoning != 3_000_000 {
		t.Fatalf("reasoning cost = %d, want 3000000", reasoning)
	}
	explicit := int64(7)
	withReasoning, err := computeCost(Price{Model: "m", Rates: Rates{Reasoning: &explicit}},
		tokens(0, 0, 0, 0, 1_000_000))
	if err != nil || withReasoning != 7 {
		t.Fatalf("explicit reasoning = %d, %v", withReasoning, err)
	}
	// All-zero rates mean free, which is explicit and allowed.
	free, err := computeCost(Price{Model: "m"}, tokens(10, 10, 10, 10, 10))
	if err != nil || free != 0 {
		t.Fatalf("free cost = %d, %v", free, err)
	}
	// Absurd rates must overflow safely rather than wrap.
	if _, err := computeCost(Price{Model: "m", Rates: Rates{Input: 1 << 62}},
		Tokens{Input: 1 << 40, Total: 1 << 40}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflow err = %v, want ErrInvalid", err)
	}
}

func TestTokenValidation(t *testing.T) {
	if _, err := normalizeTokens(Tokens{Input: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative err = %v", err)
	}
	if _, err := normalizeTokens(Tokens{Input: 10, Output: 5, Total: 99}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("inconsistent err = %v", err)
	}
	got, err := normalizeTokens(Tokens{Input: 10, Output: 5})
	if err != nil || got.Total != 15 {
		t.Fatalf("derived total = %+v, %v", got, err)
	}
	if _, err := normalizeTokens(Tokens{Input: 1 << 62, Output: 1 << 62}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflow err = %v", err)
	}
	if _, err := normalizeRaw(pluginapi.UsageDetail{InputTokens: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("raw negative err = %v", err)
	}
	if _, err := normalizeQuality("guess"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("quality err = %v", err)
	}
}

// TestUpdateKeyRejectsMaterial keeps key ownership changes explicit: material
// only ever changes through RotateKey.
func TestUpdateKeyRejectsMaterial(t *testing.T) {
	h := newHarness(t)
	key, material := h.mustKey(KeySpec{Label: "one", Enabled: true})
	spec := key.Spec
	spec.Label = "renamed"
	spec.Material = "sk-injected-material-0001"
	if _, err := h.store.UpdateKey(h.ctx(), key.ID, spec); !errors.Is(err, ErrInvalid) {
		t.Fatalf("material on update err = %v, want ErrInvalid", err)
	}
	// The rejected update changed nothing, in particular not the material.
	spec.Material = ""
	updated, err := h.store.UpdateKey(h.ctx(), key.ID, spec)
	if err != nil {
		t.Fatalf("update without material: %v", err)
	}
	if updated.Spec.Label != "renamed" {
		t.Fatalf("label = %q, want renamed", updated.Spec.Label)
	}
	if _, err := h.store.Authenticate(h.ctx(), material); err != nil {
		t.Fatalf("original material stopped working: %v", err)
	}
	if _, err := h.store.Authenticate(h.ctx(), "sk-injected-material-0001"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("injected material err = %v, want ErrUnauthorized", err)
	}
}

// callerScopeVectors were produced by an independent implementation of the host
// derivation (python hashlib.sha256 over "cli-proxy-api:caller-scope:v1\x00" +
// principal), not by this package. They pin the exact wire value the host puts
// into caller_scope metadata.
var callerScopeVectors = []struct {
	principal string
	scope     string
}{
	{"cmk:11111111-2222-4333-8444-555555555555", "0924e47f31032f9fe4db5e1962c74e5dce602db2a9bc0cc7ce21c5b3937a8e64"},
	{"cmk:abcdefab-1234-4567-89ab-cdefabcdefab", "4ea269ea8219a15517aa8eaed744ed8dc2db213a21588efa1eea9c94d45bb40a"},
}

// TestCallerScopeMatchesHostContract verifies the scope against external
// vectors so a local mistake cannot be masked by using the same code twice.
func TestCallerScopeMatchesHostContract(t *testing.T) {
	for _, v := range callerScopeVectors {
		if got := callerScope(v.principal); got != v.scope {
			t.Fatalf("callerScope(%q) = %q, want %q", v.principal, got, v.scope)
		}
		// The host trims the value and returns an empty scope for empty input.
		if got := callerScope("  " + v.principal + "\t"); got != v.scope {
			t.Fatalf("callerScope with padding = %q, want %q", got, v.scope)
		}
	}
	if got := callerScope(""); got != "" {
		t.Fatalf("callerScope(empty) = %q, want empty", got)
	}
	if got := callerScope("   "); got != "" {
		t.Fatalf("callerScope(blank) = %q, want empty", got)
	}
}

// TestKeyScopeIsHostCallerScope proves the stored scope is exactly the host
// derivation for the key principal, and that KeyByScope resolves it.
func TestKeyScopeIsHostCallerScope(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	if !strings.HasPrefix(key.Principal, principalPrefix) {
		t.Fatalf("principal = %q, want %s prefix", key.Principal, principalPrefix)
	}
	want := callerScope(key.Principal)
	if key.Scope != want {
		t.Fatalf("scope = %q, want caller scope %q", key.Scope, want)
	}
	if len(key.Scope) != 64 {
		t.Fatalf("scope = %q, want 64 hex characters", key.Scope)
	}
	// A generated key must not use the retired random scope format.
	if strings.HasPrefix(key.Scope, "cms:") {
		t.Fatalf("scope %q still uses the random prefix", key.Scope)
	}
	resolved, err := h.store.KeyByScope(h.ctx(), want)
	if err != nil {
		t.Fatalf("key by scope: %v", err)
	}
	if resolved.ID != key.ID || resolved.Principal != key.Principal {
		t.Fatalf("resolved key = %+v", resolved)
	}
	// The native layer resolves every request through this scope, so an
	// unrelated value must stay unknown.
	if _, err := h.store.KeyByScope(h.ctx(), callerScope("cmk:00000000-0000-4000-8000-000000000000")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown scope err = %v, want ErrNotFound", err)
	}
	if _, err := h.store.KeyByScope(h.ctx(), "cms:not-a-scope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy scope err = %v, want ErrNotFound", err)
	}
	// Two keys never share a scope.
	other, _ := h.mustKey(KeySpec{Enabled: true})
	if other.Scope == key.Scope {
		t.Fatal("two keys share one scope")
	}
	if _, err := h.store.KeyByScope(h.ctx(), other.Scope); err != nil {
		t.Fatalf("resolving the second key: %v", err)
	}
}

// TestScopeStableAcrossRotationAndRestart keeps the principal/scope pair
// stable for the whole life of a key.
func TestScopeStableAcrossRotationAndRestart(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	// A key created with custom material uses the same derivation.
	custom, _ := h.mustKey(KeySpec{Material: "sk-custom-scope-material-1", Enabled: true})
	if custom.Scope != callerScope(custom.Principal) {
		t.Fatalf("custom key scope = %q", custom.Scope)
	}
	if custom.Scope == key.Scope {
		t.Fatal("distinct keys must not share a scope")
	}
	// Rotation keeps the identity of the key intact.
	rotated, _, err := h.store.RotateKey(h.ctx(), key.ID, "")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.Scope != key.Scope || rotated.Principal != key.Principal {
		t.Fatalf("rotation changed identity: %+v", rotated)
	}
	if err := h.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(Options{DataDir: h.dir, Now: h.clock.now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	for _, want := range []Key{key, custom} {
		reloaded, err := reopened.KeyByScope(context.Background(), want.Scope)
		if err != nil {
			t.Fatalf("key by scope after restart: %v", err)
		}
		if reloaded.ID != want.ID || reloaded.Scope != want.Scope || reloaded.Principal != want.Principal {
			t.Fatalf("reloaded key = %+v, want %+v", reloaded, want)
		}
		if reloaded.Scope != callerScope(reloaded.Principal) {
			t.Fatalf("persisted scope %q is not the host derivation of %q", reloaded.Scope, reloaded.Principal)
		}
	}
}

// TestOpenTakesLockBeforeTouchingPepper proves the directory lock is acquired
// before the pepper is read or created: an instance that loses the race must
// fail with ErrLocked without leaving a pepper behind for the owner to trip on.
func TestOpenTakesLockBeforeTouchingPepper(t *testing.T) {
	dir := t.TempDir()
	// Stand in for an owner that holds the directory but has not written its
	// pepper yet, which is exactly the window a second instance would race into.
	lock, err := acquireDirLock(filepath.Join(dir, lockFileName))
	if err != nil {
		t.Fatalf("acquire dir lock: %v", err)
	}
	defer func() { _ = lock.release() }()

	if _, err := Open(Options{DataDir: dir}); !errors.Is(err, ErrLocked) {
		t.Fatalf("open err = %v, want ErrLocked", err)
	}
	if _, err := os.Stat(filepath.Join(dir, pepperFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pepper was touched while another instance held the lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, dbFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database was created while another instance held the lock: %v", err)
	}
}

// TestKeyMaterialValidationMatrix pins the caller supplied material contract for
// creation and rotation. CPA trims material on its way through the host, so a
// value that cannot round trip unchanged is rejected rather than rewritten, and
// only a truly empty value is generated.
func TestKeyMaterialValidationMatrix(t *testing.T) {
	invalid := []struct {
		name     string
		material string
	}{
		{"too short", "1234567"},
		{"too long", strings.Repeat("A", materialMaxLen+1)},
		{"leading space", " 12345678"},
		{"trailing space", "12345678 "},
		{"tab inside", "1234\t5678"},
		{"newline", "1234\n5678"},
		{"carriage return", "1234\r5678"},
		{"nul byte", "1234\x005678"},
		{"delete", "1234\x7f5678"},
		{"invalid utf8", "1234\xff5678"},
		{"blank", "   "},
		{"whitespace only", "\t\n"},
	}
	// validMaterials keeps the boundary values (minimum and maximum length)
	// while staying unique per phase, because rotating onto material that is
	// already registered is a conflict.
	validMaterials := func(tag string) []string {
		return []string{
			"1234" + tag + "5678",
			"sk-" + strings.Repeat("A", materialMaxLen-3-len(tag)) + tag,
			"密钥-" + tag + "-abcdefgh",
			"material " + tag + " with inner spaces",
			"sk-" + tag + "_base32_LIKE_but_charset_is_free",
		}
	}
	valid := validMaterials("c")
	h := newHarness(t)
	ctx := h.ctx()

	keys, err := h.store.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	for _, tc := range invalid {
		_, _, err := h.store.CreateKey(ctx, KeySpec{Material: tc.material, Enabled: true})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("create with %s err = %v, want ErrInvalid", tc.name, err)
		}
	}
	after, err := h.store.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(after) != len(keys) {
		t.Fatalf("rejected creation changed the store: %d keys, want %d", len(after), len(keys))
	}
	// A generated key and every valid custom material are accepted verbatim.
	if key, material, err := h.store.CreateKey(ctx, KeySpec{Enabled: true}); err != nil || material == "" {
		t.Fatalf("generated key = %v/%q", key.ID, material)
	}
	created := map[string]string{}
	for _, m := range valid {
		key, got, err := h.store.CreateKey(ctx, KeySpec{Material: m, Enabled: true})
		if err != nil {
			t.Fatalf("create %q: %v", m, err)
		}
		if got != m {
			t.Fatalf("returned material %q, want the exact input %q", got, m)
		}
		if _, err := h.store.Authenticate(ctx, m); err != nil {
			t.Fatalf("authenticate %q: %v", m, err)
		}
		created[key.ID] = m
	}

	// Rotation uses the same matrix and never leaves a trace when it fails.
	target, current, err := h.store.CreateKey(ctx, KeySpec{Material: "original-material", Enabled: true})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	before := h.key(target.ID)
	for _, tc := range invalid {
		if _, _, err := h.store.RotateKey(ctx, target.ID, tc.material); !errors.Is(err, ErrInvalid) {
			t.Fatalf("rotate with %s err = %v, want ErrInvalid", tc.name, err)
		}
		now := h.key(target.ID)
		if now.Fingerprint != before.Fingerprint {
			t.Fatalf("rejected rotation with %s changed the key", tc.name)
		}
		if _, err := h.store.Authenticate(ctx, current); err != nil {
			t.Fatalf("original material stopped working after %s: %v", tc.name, err)
		}
	}
	rotated := validMaterials("r")
	for _, m := range rotated {
		key, got, err := h.store.RotateKey(ctx, target.ID, m)
		if err != nil {
			t.Fatalf("rotate %q: %v", m, err)
		}
		if got != m || key.ID != target.ID {
			t.Fatalf("rotate returned %q for %s", got, key.ID)
		}
		if _, err := h.store.Authenticate(ctx, m); err != nil {
			t.Fatalf("authenticate rotated %q: %v", m, err)
		}
		current = m
	}
	// Rotation without custom material still generates fresh material.
	_, generated, err := h.store.RotateKey(ctx, target.ID, "")
	if err != nil || generated == "" {
		t.Fatalf("generated rotation = %q/%v", generated, err)
	}
	current = generated

	// UpdateKey refuses every non empty material, including blank values, so a
	// caller cannot believe a rotation happened when it did not.
	spec := h.key(target.ID).Spec
	for _, m := range []string{"new-material-value", "   ", "\t", current} {
		spec.Material = m
		if _, err := h.store.UpdateKey(ctx, target.ID, spec); !errors.Is(err, ErrInvalid) {
			t.Fatalf("update with material %q err = %v, want ErrInvalid", m, err)
		}
	}
	spec.Material = ""
	if _, err := h.store.UpdateKey(ctx, target.ID, spec); err != nil {
		t.Fatalf("update without material: %v", err)
	}

	// Lookup rejects oversized and empty input before hashing, and never
	// normalises the opaque value.
	if _, err := h.store.Authenticate(ctx, strings.Repeat("A", materialMaxLen+1)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("oversized lookup err = %v, want ErrUnauthorized", err)
	}
	if _, err := h.store.Authenticate(ctx, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty lookup err = %v, want ErrUnauthorized", err)
	}
	if _, err := h.store.Authenticate(ctx, " "+current+" "); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("lookup must not trim the opaque value")
	}
	if _, err := h.store.Authenticate(ctx, current); err != nil {
		t.Fatalf("the exact stored material must still authenticate: %v", err)
	}
	if len(created) != len(valid) {
		t.Fatalf("created %d custom keys, want %d", len(created), len(valid))
	}
}

// TestOpenFailsClosedWhenDataDirCannotBeSynced proves the directory sync is not
// optional: a directory whose entry cannot be flushed must abort Open before the
// ledger exists, and retrying must not create one either.
func TestOpenFailsClosedWhenDataDirCannotBeSynced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced, so the sync cannot be made to fail")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "data")
	// Write and traverse but not read: the pepper can still be created inside,
	// while the directory itself cannot be opened to flush its entry.
	if err := os.Mkdir(dir, 0o300); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	for attempt := 1; attempt <= 2; attempt++ {
		s, err := Open(Options{DataDir: dir})
		if err == nil {
			_ = s.Close()
			t.Fatalf("attempt %d: open succeeded without a durable secret", attempt)
		}
		if s != nil {
			t.Fatalf("attempt %d: store was half built", attempt)
		}
		if _, statErr := os.Stat(filepath.Join(dir, dbFileName)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("attempt %d: ledger created without a durable secret: %v", attempt, statErr)
		}
	}
}
