package reversals

import (
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
)

func TestReversalRefundInputs(t *testing.T) {
	inclusive := &ent.POSOrderLine{ID: uuid.New(), PriceIncludesTax: true, TaxCodeID: "VAT-16"}
	exclusive := &ent.POSOrderLine{ID: uuid.New(), PriceIncludesTax: false, TaxCodeID: "VAT-16"}
	rev := []entschema.ReversalLineJSON{
		{LineID: inclusive.ID, SKU: "A", Name: "Inclusive", Quantity: 2, Amount: 232, TaxAmount: 32},
		{LineID: exclusive.ID, SKU: "B", Name: "Exclusive", Quantity: 1, Amount: 116, TaxAmount: 16},
		{LineID: uuid.New(), SKU: "C", Name: "Unknown line", Quantity: 1, Amount: 50},
	}
	got := reversalRefundInputs(rev, []*ent.POSOrderLine{inclusive, exclusive})
	if len(got) != 3 {
		t.Fatalf("want 3 inputs, got %d", len(got))
	}
	if got[0].Gross != 232 || got[0].Tax != 32 || got[0].TaxCode != "VAT-16" || got[0].Quantity != 2 {
		t.Fatalf("inclusive line wrong: %+v", got[0])
	}
	// An exclusive line's amount has its tax added on top; the sale was sent without it.
	if math.Abs(got[1].Gross-100) > 1e-9 || got[1].Tax != 16 {
		t.Fatalf("exclusive line wrong: %+v", got[1])
	}
	// A reversed line that is no longer on the order keeps its amount and has no tax code.
	if got[2].Gross != 50 || got[2].TaxCode != "" {
		t.Fatalf("unmatched line wrong: %+v", got[2])
	}
}
