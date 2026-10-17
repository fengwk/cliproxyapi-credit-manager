package credit

import (
	"database/sql"
	"fmt"
	"runtime"
	"testing"
)

// A price and its enabled override change atomically. A preview must see one
// complete book, never the base price from one revision and rules from another.
func TestPreviewPriceUsesConsistentPriceBookSnapshot(t *testing.T) {
	h := newHarness(t)
	const model = "preview-snapshot"
	h.mustPrice(model, Rates{Input: 1})
	if err := h.store.PutPricingRule(h.ctx(), PricingRule{
		ID: "override", MatchKind: MatchExact, Pattern: model, Priority: 10,
		Price: Price{Rates: Rates{Input: 4}},
	}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 512; i++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			err := h.store.inTx(h.ctx(), func(tx *sql.Tx) error {
				enabled := i%2 == 1
				rate := int64(1)
				if enabled {
					rate = 2
				}
				if _, err := tx.ExecContext(h.ctx(), "UPDATE prices SET input_rate = ? WHERE model = ?", rate, model); err != nil {
					return err
				}
				_, err := tx.ExecContext(h.ctx(), "UPDATE pricing_rules SET enabled = ? WHERE id = 'override'", boolInt(enabled))
				return err
			})
			if err != nil {
				done <- fmt.Errorf("change price book: %w", err)
				return
			}
			runtime.Gosched()
		}
		done <- nil
	}()
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 256; i++ {
		preview, err := h.store.PreviewPrice(h.ctx(), model, "", microPerUnit)
		if err != nil {
			t.Fatal(err)
		}
		if preview.CostMicroUSD != 1 && preview.CostMicroUSD != 4 {
			t.Fatalf("mixed price book: got %d, want base 1 or override 4", preview.CostMicroUSD)
		}
	}
}
