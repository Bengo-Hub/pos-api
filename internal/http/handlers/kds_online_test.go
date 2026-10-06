package handlers

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
)

func strp(s string) *string { return &s }

func TestTicketViewChannelAndLabel(t *testing.T) {
	cases := []struct {
		name        string
		ticket      *ent.KDSTicket
		order       *ent.POSOrder
		wantChannel string
		wantSource  string
		wantLabel   string
		wantName    string
	}{
		{
			name:        "dine-in shows its table",
			ticket:      &ent.KDSTicket{OrderSubtype: "dine_in", TableReference: "5"},
			order:       &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeDineIn, CustomerName: strp("Walk in")},
			wantChannel: "dine_in", wantSource: "pos", wantLabel: "Table 5",
		},
		{
			name:        "table name already prefixed is kept",
			ticket:      &ent.KDSTicket{OrderSubtype: "dine_in", TableReference: "Table 12"},
			order:       &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeDineIn},
			wantChannel: "dine_in", wantSource: "pos", wantLabel: "Table 12",
		},
		{
			name:        "POS takeaway is called by customer name",
			ticket:      &ent.KDSTicket{OrderSubtype: "takeaway"},
			order:       &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeTakeaway, CustomerName: strp("Achieng")},
			wantChannel: "takeaway", wantSource: "pos", wantName: "Achieng",
		},
		{
			name:   "online pickup is not a POS takeaway",
			ticket: &ent.KDSTicket{OrderSubtype: "takeaway"},
			order: &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeTakeaway, CustomerName: strp("Brian"),
				Metadata: map[string]any{"online_order_id": "x", "fulfillment_type": "pickup"}},
			wantChannel: "online_pickup", wantSource: "online", wantLabel: "Online pickup", wantName: "Brian",
		},
		{
			name:   "scheduled online delivery",
			ticket: &ent.KDSTicket{OrderSubtype: "delivery"},
			order: &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeDelivery,
				Metadata: map[string]any{"online_order_id": "x", "fulfillment_type": "delivery", "scheduled_for_label": "Fri 18:30"}},
			wantChannel: "online_delivery", wantSource: "online", wantLabel: "Online delivery for Fri 18:30",
		},
		{
			name:        "room service shows the room",
			ticket:      &ent.KDSTicket{OrderSubtype: "room_service", TableReference: "204"},
			order:       &ent.POSOrder{OrderSubtype: posorder.OrderSubtypeRoomService},
			wantChannel: "room_service", wantSource: "pos", wantLabel: "Room 204",
		},
		{
			name:        "order gone falls back to the ticket subtype",
			ticket:      &ent.KDSTicket{OrderSubtype: "takeaway"},
			order:       nil,
			wantChannel: "takeaway", wantSource: "pos",
		},
	}
	for _, c := range cases {
		v := ticketView(c.ticket, c.order)
		if v.Channel != c.wantChannel || v.OrderSource != c.wantSource || v.OrderLabel != c.wantLabel || v.CustomerName != c.wantName {
			t.Errorf("%s: got channel=%q source=%q label=%q name=%q", c.name, v.Channel, v.OrderSource, v.OrderLabel, v.CustomerName)
		}
		if v.Job != nil {
			t.Errorf("%s: non-job order must carry no job header", c.name)
		}
	}
}

func TestTicketViewServiceJobHeader(t *testing.T) {
	o := &ent.POSOrder{
		OrderSubtype: posorder.OrderSubtypeServiceJob, TotalAmount: 1500, PaidTotal: 500,
		CustomerName: strp("Print Co"), CustomerPhone: strp("0700000000"),
		Metadata: map[string]any{"job": map[string]any{"stage": "design"}},
	}
	v := ticketView(&ent.KDSTicket{OrderSubtype: "service_job"}, o)
	if v.Channel != "service_job" || v.Job == nil || v.Job.PaidTotal != 500 || v.Job.Details["stage"] != "design" || v.Job.CustomerName != "Print Co" {
		t.Fatalf("job view = %+v channel=%q", v.Job, v.Channel)
	}
	if v.CustomerName != "" {
		t.Errorf("job customer belongs in the job header only, got %q", v.CustomerName)
	}
}
