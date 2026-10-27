package orders

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// Returned goods count as already taken off a line, so a later reversal cannot reverse them again,
// and the caller's lines are left untouched.
func TestWithReturnedAsVoided(t *testing.T) {
	voided := 1.0
	returnedLine := &ent.POSOrderLine{ID: uuid.New(), Quantity: 5, VoidedQty: &voided}
	untouched := &ent.POSOrderLine{ID: uuid.New(), Quantity: 3}
	fresh := &ent.POSOrderLine{ID: uuid.New(), Quantity: 2}

	out := WithReturnedAsVoided([]*ent.POSOrderLine{returnedLine, untouched, fresh},
		map[uuid.UUID]float64{returnedLine.ID: 2, fresh.ID: 2})

	if got := *out[0].VoidedQty; got != 3 {
		t.Fatalf("voided 1 plus returned 2: got %v, want 3", got)
	}
	if out[1].VoidedQty != nil {
		t.Fatal("a line with no return must keep its VoidedQty")
	}
	if got := *out[2].VoidedQty; got != 2 {
		t.Fatalf("fully returned line: got %v, want 2", got)
	}
	if *returnedLine.VoidedQty != 1 || fresh.VoidedQty != nil {
		t.Fatal("the caller's lines must not be modified")
	}
}
