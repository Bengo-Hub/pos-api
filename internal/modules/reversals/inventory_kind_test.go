package reversals

import (
	"testing"

	"github.com/bengobox/pos-service/internal/modules/inventory"
)

func TestInventoryKind(t *testing.T) {
	cases := map[string]string{
		ReasonPrefixDeleteSale + "duplicate": inventory.ReversalVoid,
		ReasonPrefixEditSale + "qty typo":    inventory.ReversalEdit,
		"customer refused goods":             inventory.ReversalReversal,
		"Edit sale info only (not a prefix)": inventory.ReversalReversal,
	}
	for reason, want := range cases {
		if got := InventoryKind(reason); got != want {
			t.Errorf("InventoryKind(%q) = %q, want %q", reason, got, want)
		}
	}
}
