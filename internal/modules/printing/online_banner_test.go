package printing

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

func TestOnlineOrderBanner(t *testing.T) {
	if got := OnlineOrderBanner(&ent.POSOrder{Metadata: map[string]any{"table_number": "5"}}); got != "" {
		t.Fatalf("POS-native order must have no online banner, got %q", got)
	}
	pickup := &ent.POSOrder{Metadata: map[string]any{"online_order_id": "x", "fulfillment_type": "pickup"}}
	if got := OnlineOrderBanner(pickup); got != "*** ONLINE PICKUP ***" {
		t.Fatalf("got %q", got)
	}
	delivery := &ent.POSOrder{Metadata: map[string]any{
		"online_order_id":     "x",
		"fulfillment_type":    "delivery",
		"scheduled_for_label": "Fri 18:30",
		"order_notes":         "no onions",
	}}
	if got := OnlineOrderBanner(delivery); got != "*** ONLINE DELIVERY FOR Fri 18:30 | no onions ***" {
		t.Fatalf("got %q", got)
	}
}

func TestStationTicketDataPrintsModifiersNotesAndBanner(t *testing.T) {
	order := &ent.POSOrder{OrderNumber: "CC-1", Metadata: map[string]any{"online_order_id": "x"}}
	d := StationTicketData(order, "Kitchen", []map[string]any{
		{"name": "Burger", "quantity": 2.0, "modifiers": []string{"No onions", "Extra cheese"}, "notes": "well done"},
		{"name": "Chips", "quantity": 1.0},
	})
	if d.Banner != "*** ONLINE PICKUP ***" {
		t.Fatalf("banner = %q", d.Banner)
	}
	if d.Items[0].Notes != "No onions, Extra cheese | well done" || d.Items[0].Quantity != 2 {
		t.Fatalf("item 0 = %+v", d.Items[0])
	}
	if d.Items[1].Notes != "" {
		t.Fatalf("plain item must have no notes: %+v", d.Items[1])
	}
	// A delta chit's own banner still wins over the online banner.
	if dd := StationTicketDataWithBanner(order, "Kitchen", nil, "*** ADDITIONAL ITEMS ***"); dd.Banner != "*** ADDITIONAL ITEMS ***" {
		t.Fatalf("delta banner lost: %q", dd.Banner)
	}
}
