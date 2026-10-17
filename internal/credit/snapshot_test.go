package credit

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// mutateSnapshot rewrites the stored price snapshot of one request, modelling a
// damaged or tampered ledger row that bypassed the validating writer.
func mutateSnapshot(t *testing.T, h *harness, requestID string, mutate func(*priceSnapshot)) {
	t.Helper()
	var raw string
	if err := h.store.db.QueryRow(
		`SELECT price_snapshot FROM requests WHERE id = ?`, requestID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var snapshot priceSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatal(err)
	}
	mutate(&snapshot)
	updated, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.db.Exec(
		`UPDATE requests SET price_snapshot = ? WHERE id = ?`, string(updated), requestID); err != nil {
		t.Fatal(err)
	}
}

// TestDamagedRequestSnapshotIsRefused pins that the frozen snapshot is the only
// price source and must fail closed: an empty, corrupt, incomplete or
// inconsistent snapshot is an error, never a zero priced or default priced
// request.
func TestDamagedRequestSnapshotIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"empty", ""},
		{"corrupt json", "{not json"},
		{"missing price", `{"tiers":[],"service_tier":"priority"}`},
		{"null price", `{"price":null}`},
		{"missing model", `{"price":{"rates":{"input":1000000}}}`},
		{"model mismatch", `{"price":{"model":"other","rates":{"input":1000000}}}`},
		{"negative rate", `{"price":{"model":"m1","rates":{"input":-1}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustPrice("m1", Rates{Input: million})
			key, _ := h.mustKey(KeySpec{Enabled: true})
			req := h.mustAdmit(AdmitInput{ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "m1"})
			if _, err := h.store.db.Exec(
				`UPDATE requests SET price_snapshot = ? WHERE id = ?`, tc.payload, req.ID); err != nil {
				t.Fatal(err)
			}
			// Settlement must refuse the damaged row instead of booking money.
			if err := h.store.RecordUsage(h.ctx(),
				usageFor("u1", req, "m1", tokens(10, 0, 0, 5, 0))); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) {
				t.Fatalf("record usage err = %v, want a snapshot validation error", err)
			}
			// Every read path that needs the frozen price fails the same way.
			if _, err := h.store.Report(h.ctx(), Filter{
				From: testEpoch.Add(-24 * time.Hour), To: testEpoch.Add(24 * time.Hour),
				Page: 1, PageSize: 10}); err == nil {
				t.Fatal("report accepted a damaged price snapshot")
			}
			if key = h.key(key.ID); key.SpentMicroUSD != 0 {
				t.Fatalf("damaged snapshot booked %d microUSD", key.SpentMicroUSD)
			}
		})
	}
}

// TestExplicitZeroPriceSnapshotIsAccepted pins the other side of the same rule:
// an explicit all-zero rate card is a real (free) price, not a missing one, so
// the snapshot validator accepts it and every read path returns it unchanged.
// (The end-to-end free settlement is already pinned by the existing zero rate
// test; this asserts the snapshot itself survives validation.)
func TestExplicitZeroPriceSnapshotIsAccepted(t *testing.T) {
	h := newHarness(t)
	h.mustPrice("free", Rates{})
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustAdmit(AdmitInput{ID: "r0", TraceID: "t0", KeyID: key.ID, RequestedModel: "free"})
	frozen := h.request(req.ID)
	if frozen.Price.Model != "free" || frozen.Price.Rates != (Rates{}) {
		t.Fatalf("frozen free price = %+v, want an explicit all-zero card", frozen.Price)
	}
}

// TestPerImageSnapshotCountIsNotDefaulted pins that a per-image request never
// falls back to one image: a corrupted frozen count fails settlement instead of
// being billed as a different number.
func TestPerImageSnapshotCountIsNotDefaulted(t *testing.T) {
	h := newHarness(t)
	if err := h.store.PutPrice(h.ctx(), Price{
		Model: "img", BillingMode: billingPerImage, PerImageMicroUSD: 50_000}); err != nil {
		t.Fatal(err)
	}
	key, _ := h.mustKey(KeySpec{Enabled: true})
	req := h.mustAdmit(AdmitInput{
		ID: "r1", TraceID: "t1", KeyID: key.ID, RequestedModel: "img",
		ImageCount: 3, ImageCountDeclared: true})
	mutateSnapshot(t, h, req.ID, func(s *priceSnapshot) { s.ImageCount = 0 })
	if err := h.store.RecordUsage(h.ctx(),
		usageFor("u1", req, "img", tokens(1, 0, 0, 1, 0))); err == nil {
		t.Fatal("a per-image snapshot without a count was accepted")
	}
	if err := h.store.Complete(h.ctx(), Completion{
		ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded}); err == nil {
		t.Fatal("completion accepted a per-image snapshot without a count")
	}
	if key = h.key(key.ID); key.SpentMicroUSD != 0 {
		t.Fatalf("corrupted per-image count booked %d microUSD", key.SpentMicroUSD)
	}
}
