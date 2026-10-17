package credit

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A wildcard cannot invent the actual model that is missing from a usage log.
// Permissions still use the separately recorded requested model.
func TestMissingActualModelStaysUnpricedWithCatchAllRule(t *testing.T) {
	h := newHarness(t)
	key, _ := h.mustKey(KeySpec{Enabled: true})
	if err := h.store.PutPricingRule(h.ctx(), PricingRule{
		ID: "catch-all", MatchKind: MatchGlob, Pattern: "*", Enabled: true,
		Price: Price{Rates: Rates{}},
	}); err != nil {
		t.Fatal(err)
	}
	req := h.mustAdmit(AdmitInput{ID: "missing-model", TraceID: "trace", KeyID: key.ID, RequestedModel: "alias"})
	h.mustUsage(Usage{ID: "usage", TraceID: req.TraceID, KeyID: key.ID,
		Tokens: Tokens{Input: 1, Total: 1}, Quality: QualityComplete})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	report := h.report(key.ID)
	if report.Summary.UnpricedExecutions != 1 || report.Items[0].Executions[0].CostMicroUSD != nil {
		t.Fatalf("missing actual model became a priced free execution: %+v", report)
	}
	if report.Summary.CompleteExecutions != 1 || report.Items[0].State != stateSettled {
		t.Fatalf("missing model must not erase complete token accounting: %+v", report)
	}
}

// A broken pricing table may prevent a management read, but none of the request
// lifecycle writes is allowed to depend on that table.
func TestLifecycleDoesNotConsultUnavailablePricingTables(t *testing.T) {
	h := newHarness(t)
	key, material := h.mustKey(KeySpec{Enabled: true, QuotaMicroUSD: 1})
	for _, table := range []string{"prices", "pricing_rules"} {
		if _, err := h.store.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.Authenticate(h.ctx(), material); err != nil {
		t.Fatalf("price-independent authentication: %v", err)
	}
	req := h.mustAdmit(AdmitInput{ID: "no-price-storage", TraceID: "trace", KeyID: key.ID, RequestedModel: "m"})
	h.mustUsage(Usage{ID: "usage", TraceID: req.TraceID, KeyID: key.ID, Model: "m",
		Tokens: Tokens{Input: 10, Total: 10}, Quality: QualityComplete})
	h.mustComplete(Completion{ID: req.ID, TraceID: req.TraceID, Outcome: OutcomeSucceeded})
	row, err := loadRequestByID(h.ctx(), h.store.db, req.ID)
	if err != nil || row.state != stateSettled {
		t.Fatalf("price-independent completion: row=%+v error=%v", row, err)
	}
}

// A prior monetary schema is never upgraded or emptied. The version marker and
// its stored monetary facts must survive a refused open.
func TestOpenRejectsMonetaryFormatOneUntouched(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbFileName)
	db, err := sql.Open(driverName, dsnFor(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE requests (id TEXT PRIMARY KEY, price_snapshot TEXT NOT NULL, charge_micro_usd INTEGER NOT NULL)",
		`INSERT INTO requests VALUES ('old-request', '{"sentinel":"old-price"}', 123)`,
		"PRAGMA user_version = 1",
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pepperFileName), make([]byte, pepperLen), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(Options{DataDir: dir})
	if store != nil {
		store.Close()
	}
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("format 1 open error = %v, want ErrSchemaVersion", err)
	}
	db, err = sql.Open(driverName, dsnFor(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if version := dbUserVersion(t, db); version != 1 {
		t.Fatalf("format changed to %d", version)
	}
	var snapshot string
	var amount int64
	if err := db.QueryRow("SELECT price_snapshot, charge_micro_usd FROM requests WHERE id = 'old-request'").Scan(&snapshot, &amount); err != nil {
		t.Fatal(err)
	}
	if snapshot != `{"sentinel":"old-price"}` || amount != 123 || userObjectExists(t, db, "usages") {
		t.Fatalf("old format was changed: snapshot=%q amount=%d", snapshot, amount)
	}
}
