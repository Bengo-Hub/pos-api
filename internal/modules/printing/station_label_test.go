package printing

import (
	"bytes"
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
)

func name(s string) *string { return &s }

func TestStationOrderLabel(t *testing.T) {
	cases := []struct {
		name     string
		order    *ent.POSOrder
		wantType string
		want     []string
	}{
		{
			name:     "till dine-in",
			order:    &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeDineIn, CustomerName: name("Walk in"), Metadata: map[string]any{"table_number": "5"}},
			wantType: "DINE-IN",
			want:     []string{"Source:  POS"},
		},
		{
			name:     "till takeaway names the customer",
			order:    &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeTakeaway, CustomerName: name("Achieng")},
			wantType: "TAKEAWAY",
			want:     []string{"Source:  POS", "For:     Achieng"},
		},
		{
			name:     "till delivery",
			order:    &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeDelivery},
			wantType: "DELIVERY",
			want:     []string{"Source:  POS"},
		},
		{
			name: "online pickup",
			order: &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeTakeaway, CustomerName: name("Brian"),
				Metadata: map[string]any{"online_order_id": "x", "online_order_no": "000045", "fulfillment_type": "pickup"}},
			wantType: "ONLINE PICKUP",
			want:     []string{"Source:  Online store #000045", "For:     Brian"},
		},
		{
			name: "scheduled online delivery with a note",
			order: &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeDelivery, Metadata: map[string]any{
				"online_order_id": "x", "fulfillment_type": "delivery", "scheduled_for_label": "Fri 18:30", "order_notes": "no onions"}},
			wantType: "ONLINE DELIVERY",
			want:     []string{"Source:  Online store", "Ready by: Fri 18:30", "Note:    no onions"},
		},
		{
			name:     "room service",
			order:    &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeRoomService},
			wantType: "ROOM SERVICE",
			want:     []string{"Source:  POS"},
		},
	}
	for _, c := range cases {
		gotType, got := StationOrderLabel(c.order)
		if gotType != c.wantType || len(got) != len(c.want) {
			t.Errorf("%s: got %q %q", c.name, gotType, got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: line %d = %q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

func TestStationTicketDataPrintsModifiersNotesAndOrderType(t *testing.T) {
	order := &ent.POSOrder{OrderNumber: "CC-1", OrderSubtype: posorder.OrderSubtypeTakeaway, Metadata: map[string]any{"online_order_id": "x"}}
	d := StationTicketData(order, "Kitchen", []map[string]any{
		{"name": "Burger", "quantity": 2.0, "modifiers": []string{"No onions", "Extra cheese"}, "notes": "well done"},
		{"name": "Chips", "quantity": 1.0},
	})
	if d.OrderType != "ONLINE PICKUP" || d.Banner != "" {
		t.Fatalf("type = %q banner = %q", d.OrderType, d.Banner)
	}
	if d.Items[0].Notes != "No onions, Extra cheese | well done" || d.Items[0].Quantity != 2 {
		t.Fatalf("item 0 = %+v", d.Items[0])
	}
	if d.Items[1].Notes != "" {
		t.Fatalf("plain item must have no notes: %+v", d.Items[1])
	}
	// A delta chit keeps the order type and adds its own banner.
	dd := StationTicketDataWithBanner(order, "Kitchen", nil, "*** ADDITIONAL ITEMS ***")
	if dd.Banner != "*** ADDITIONAL ITEMS ***" || dd.OrderType != "ONLINE PICKUP" {
		t.Fatalf("delta chit lost a label: type=%q banner=%q", dd.OrderType, dd.Banner)
	}
	out := BuildReceipt(dd)
	for _, want := range []string{"ONLINE PICKUP", "ADDITIONAL ITEMS", "Source:  Online store"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("printed chit is missing %q", want)
		}
	}
}
