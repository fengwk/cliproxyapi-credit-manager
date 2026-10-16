package credit

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestTierSelectionReserveAndSettlement pins the tier contract: a reservation
// applies only a matching service tier (never a context tier selected from a
// coarse estimate), and settlement applies the matching service tier, else the
// highest context tier reached by the actual canonical prompt size, which
// includes cache read and cache write.
func TestTierSelectionReserveAndSettlement(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "tiers", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}},
		Tiers: []PriceTier{
			{Kind: priceTierContext, Threshold: 100, Label: "100K", Rates: Rates{Input: 5 * million}},
			{Kind: priceTierService, Service: "priority", Rates: Rates{Output: 9 * million}},
		}})
	// A service-tier reservation must ignore the context tier even though its
	// output estimate alone already exceeds the context threshold.
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 1000, OutputEstimate: 10, ServiceTier: "PRIORITY"})
	if req.HeldMicroUSD != 1090 {
		t.Fatalf("service-tier hold = %d, want 1090 (context tier must be ignored)", req.HeldMicroUSD)
	}
	// Settlement: the service tier still wins over the reached context tier.
	h.mustUsage(usageFor("u1", req, "m1", tokens(150, 0, 0, 10, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if key = h.key(key.ID); key.SpentMicroUSD != 240 {
		t.Fatalf("service-tier settlement spent = %d, want 240", key.SpentMicroUSD)
	}
	// Without a service tier the context tier applies at settlement, and the
	// context size counts uncached input plus cache read plus cache write.
	req2 := h.mustReserve(ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 10, OutputEstimate: 10})
	if req2.HeldMicroUSD != 30 {
		t.Fatalf("base hold = %d, want 30 (no context tier at reserve)", req2.HeldMicroUSD)
	}
	h.mustUsage(usageFor("u2", req2, "m1", tokens(50, 60, 0, 10, 0)))
	h.mustComplete(Completion{ID: req2.ID, TraceID: req2.TraceID, Outcome: OutcomeSucceeded})
	if key = h.key(key.ID); key.SpentMicroUSD != 240+270 {
		t.Fatalf("context settlement spent = %d, want 510", key.SpentMicroUSD)
	}
	// An unrecognized service tier falls back to the base rates.
	req3 := h.mustReserve(ReserveInput{ID: "r3", TraceID: "t3", KeyID: key.ID, RequestedModel: "m1",
		InputEstimate: 1, OutputEstimate: 10, ServiceTier: "weird"})
	h.mustUsage(usageFor("u3", req3, "m1", tokens(0, 0, 0, 10, 0)))
	h.mustComplete(Completion{ID: req3.ID, TraceID: req3.TraceID, Outcome: OutcomeSucceeded})
	if key = h.key(key.ID); key.SpentMicroUSD != 240+270+20 {
		t.Fatalf("unknown tier spent = %d, want 530", key.SpentMicroUSD)
	}
}

// TestServiceTierNormalization pins the canonical billing keys.
func TestServiceTierNormalization(t *testing.T) {
	for input, want := range map[string]string{
		"": "", " ": "", "auto": "", "default": "", "standard": "", "on-demand": "",
		"on_demand": "", "scale": "", "PRIORITY": "priority", "prio": "priority",
		"flex": "flex", " Priority ": "priority",
	} {
		if got := normalizeServiceTier(input); got != want {
			t.Fatalf("normalizeServiceTier(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestTokenCostRoundsOnceOverTheSum pins the exact integer contract: buckets are
// summed exactly and rounded up once, never per bucket, and a positive priced
// usage is never free.
func TestTokenCostRoundsOnceOverTheSum(t *testing.T) {
	one := Price{Rates: Rates{Input: 1, Output: 1}}
	got, err := computeCost(one, Tokens{Input: 1, Output: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("two single-microUSD buckets = %d, want one rounded microUSD", got)
	}
	single, err := computeCost(one, Tokens{Input: 1})
	if err != nil || single != 1 {
		t.Fatalf("single bucket = %d %v, want 1", single, err)
	}
	if zero, err := computeCost(Price{}, Tokens{Input: 1, Output: 1}); err != nil || zero != 0 {
		t.Fatalf("free price = %d %v, want 0", zero, err)
	}
}

// TestCostOverflowIsRejected protects the integer ledger from silent wraparound.
func TestCostOverflowIsRejected(t *testing.T) {
	if _, err := computeCost(Price{Rates: Rates{Input: math.MaxInt64}}, Tokens{Input: math.MaxInt64}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("token cost overflow err = %v, want ErrInvalid", err)
	}
	if _, err := perImageCost(Price{BillingMode: billingPerImage, PerImageMicroUSD: math.MaxInt64}, 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("per-image overflow err = %v, want ErrInvalid", err)
	}
	// The same overflow must fail the reservation rather than store a wrapped hold.
	h, key := billingHarness(t)
	if err := h.store.PutPrice(context.Background(), Price{Model: "huge", Rates: Rates{Input: math.MaxInt64}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Reserve(h.ctx(), ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID,
		RequestedModel: "huge", InputEstimate: math.MaxInt64}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflowing reserve err = %v, want ErrInvalid", err)
	}
}

// TestPerImageBillingOnceOnProvenSuccess pins the per-image financial contract:
// the frozen requested count is billed exactly once, on a proven successful
// lifecycle, never per attempt and never for a failed lifecycle.
func TestPerImageBillingOnceOnProvenSuccess(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img", ImageCount: 3})
	if req.HeldMicroUSD != 150_000 {
		t.Fatalf("per-image hold = %d, want 150000", req.HeldMicroUSD)
	}
	// A failed first attempt and a successful retry must still bill one count.
	attempt := usageFor("u-failed", req, "img", tokens(1, 0, 0, 1, 0))
	attempt.Failed = true
	h.mustUsage(attempt)
	h.mustUsage(usageFor("u-ok", req, "img", tokens(1, 0, 0, 1, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if key = h.key(key.ID); key.SpentMicroUSD != 150_000 || key.HeldMicroUSD != 0 {
		t.Fatalf("per-image spent=%d held=%d, want 150000/0", key.SpentMicroUSD, key.HeldMicroUSD)
	}
	// A replay of the successful attempt must not double charge.
	h.mustUsage(usageFor("u-ok", req, "img", tokens(1, 0, 0, 1, 0)))
	if key = h.key(key.ID); key.SpentMicroUSD != 150_000 {
		t.Fatalf("replay double charged: spent=%d", key.SpentMicroUSD)
	}
}

// TestPerImageFailedLifecycleIsNotCharged pins that a failed lifecycle releases
// the hold without billing the requested images.
func TestPerImageFailedLifecycleIsNotCharged(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img", ImageCount: 2})
	h.mustUsage(usageFor("u1", req, "img", tokens(1, 0, 0, 1, 0)))
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeFailed})
	if key = h.key(key.ID); key.SpentMicroUSD != 0 || key.HeldMicroUSD != 0 {
		t.Fatalf("failed lifecycle spent=%d held=%d, want 0/0", key.SpentMicroUSD, key.HeldMicroUSD)
	}
}

// TestPerImageSucceededWithoutUsageStaysPending pins that a successful lifecycle
// with no usage is not written off: the billed images stay held and the request
// stays pending until trustworthy usage arrives.
func TestPerImageSucceededWithoutUsageStaysPending(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img", ImageCount: 2})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state = %s, want pending while no usage proves the images", got.State)
	}
	if key = h.key(key.ID); key.HeldMicroUSD != 100_000 || key.SpentMicroUSD != 0 {
		t.Fatalf("pending per-image held=%d spent=%d, want 100000/0", key.HeldMicroUSD, key.SpentMicroUSD)
	}
	// A late trustworthy usage books the frozen count once and settles.
	h.mustUsage(usageFor("u-late", req, "img", tokens(1, 0, 0, 1, 0)))
	if key = h.key(key.ID); key.SpentMicroUSD != 100_000 || key.HeldMicroUSD != 0 {
		t.Fatalf("late usage spent=%d held=%d, want 100000/0", key.SpentMicroUSD, key.HeldMicroUSD)
	}
}

// TestPerImageUnknownUsageStaysPending pins that untrustworthy accounting never
// pays for images silently.
func TestPerImageUnknownUsageStaysPending(t *testing.T) {
	h, key := billingHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img", ImageCount: 1})
	unknown := usageFor("u-unknown", req, "img", Tokens{})
	unknown.Quality = QualityUnknown
	h.mustUsage(unknown)
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	if got := h.request(req.ID); got.State != statePending {
		t.Fatalf("state = %s, want pending for unknown accounting", got.State)
	}
	if key = h.key(key.ID); key.SpentMicroUSD != 0 || key.HeldMicroUSD != 50_000 {
		t.Fatalf("unknown accounting spent=%d held=%d, want 0/50000", key.SpentMicroUSD, key.HeldMicroUSD)
	}
}

// TestUsageServiceTierReplayAndFrozenFallback pins that the billed tier is
// persisted and part of the replay identity, that an omitted tier falls back to
// the tier frozen at admission, and that a mismatching tier conflicts instead of
// silently repricing.
func TestUsageServiceTierReplayAndFrozenFallback(t *testing.T) {
	h, key := billingHarness(t)
	h.mustRule(PricingRule{ID: "svc", MatchKind: MatchExact, Pattern: "m1", Priority: 5, Enabled: true,
		Price: Price{Rates: Rates{Input: million, Output: 2 * million}},
		Tiers: []PriceTier{
			{Kind: priceTierService, Service: "priority", Rates: Rates{Output: 9 * million}},
			{Kind: priceTierService, Service: "flex", Rates: Rates{Output: 4 * million}},
		}})
	req := h.mustReserve(ReserveInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1", ServiceTier: "priority"})
	usage := usageFor("u1", req, "m1", tokens(0, 0, 0, 10, 0))
	usage.ServiceTier = "priority"
	h.mustUsage(usage)
	if key = h.key(key.ID); key.SpentMicroUSD != 90 {
		t.Fatalf("priority spent = %d, want 90", key.SpentMicroUSD)
	}
	h.mustUsage(usage) // identical replay
	if key = h.key(key.ID); key.SpentMicroUSD != 90 {
		t.Fatalf("replay changed spend: %d", key.SpentMicroUSD)
	}
	conflict := usage
	conflict.ServiceTier = "flex"
	if err := h.store.RecordUsage(h.ctx(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatching tier replay err = %v, want ErrConflict", err)
	}
	// An omitted tier uses the tier frozen at admission and persists it.
	req2 := h.mustReserve(ReserveInput{ID: "r2", TraceID: "t2", KeyID: key.ID, RequestedModel: "m1", ServiceTier: "flex"})
	h.mustUsage(usageFor("u2", req2, "m1", tokens(0, 0, 0, 10, 0)))
	if key = h.key(key.ID); key.SpentMicroUSD != 130 {
		t.Fatalf("frozen flex spent = %d, want 130", key.SpentMicroUSD)
	}
	execs := h.request(req2.ID).Executions
	if len(execs) != 1 || execs[0].ServiceTier != "flex" {
		t.Fatalf("persisted tier = %+v, want flex", execs)
	}
}

// TestPricingMigrationUpgradesPopulatedBaseStore builds a pre-feature ledger with
// the base schema only, then opens it with the current code: the additive
// migration must run and the legacy request, which has no snapshot, must settle
// from its stored rate columns.
func TestPricingMigrationUpgradesPopulatedBaseStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbFileName)
	raw, err := sql.Open(driverName, dsnFor(dbPath))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := raw.Exec(schemaDDL); err != nil {
		t.Fatalf("base schema: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO keys(id,principal,scope,fingerprint,hmac,label,enabled,quota_micro_usd,daily_micro_usd,weekly_micro_usd,monthly_micro_usd,max_concurrent,allowed_models,expires_at,created_at,spent_micro_usd,held_micro_usd)
		 VALUES('k1','cmk:legacy','scope-legacy','fp','hmac','legacy',1,0,0,0,0,0,'[]',NULL,1000,0,0)`,
		`INSERT INTO prices(model,input_rate,output_rate,reasoning_rate,cache_read_rate,cache_write_rate,version)
		 VALUES('m1',1000000,2000000,NULL,0,0,1)`,
		`INSERT INTO requests(id,trace_id,key_id,model,state,outcome,status_code,started_at,completed_at,held_micro_usd,price_input,price_output,price_reasoning,price_cache_read,price_cache_write,price_version,token_estimate,charge_micro_usd,resolved,initial_hold_micro_usd)
		 VALUES('legacy-1','legacy-trace','k1','m1','pending','',0,1000,NULL,0,1000000,2000000,NULL,0,0,1,20,0,'',0)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed legacy store: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, pepperFileName), make([]byte, pepperLen), 0o600); err != nil {
		t.Fatalf("write pepper: %v", err)
	}
	s, err := Open(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if err := s.RecordUsage(ctx, Usage{
		ID: "legacy-usage", TraceID: "legacy-trace", KeyID: "k1", Model: "m1",
		Tokens: Tokens{Input: 10, Output: 5, Total: 15}, Quality: QualityComplete,
	}); err != nil {
		t.Fatalf("record legacy usage: %v", err)
	}
	if err := s.Complete(ctx, Completion{ID: "legacy-1", TraceID: "legacy-trace", Outcome: OutcomeSucceeded}); err != nil {
		t.Fatalf("complete legacy request: %v", err)
	}
	key, err := s.GetKey(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if key.SpentMicroUSD != 20 || key.HeldMicroUSD != 0 {
		t.Fatalf("legacy settle spent=%d held=%d, want 20/0 from the stored rate columns",
			key.SpentMicroUSD, key.HeldMicroUSD)
	}
}
