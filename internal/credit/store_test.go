package credit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	rep, err := h.store.Report(h.ctx(), Filter{
		From:     testEpoch.AddDate(-1, 0, 0),
		To:       h.clock.now().Add(time.Hour),
		Page:     1,
		PageSize: 200,
	})
	if err != nil {
		h.t.Fatalf("report: %v", err)
	}
	for _, item := range rep.Items {
		if item.ID == id {
			return item
		}
	}
	h.t.Fatalf("request %s not found in report", id)
	return Request{}
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
