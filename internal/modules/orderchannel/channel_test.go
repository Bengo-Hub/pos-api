package orderchannel

import "testing"

func TestOf(t *testing.T) {
	online := func(ft string) map[string]any {
		m := map[string]any{"online_order_id": "a1"}
		if ft != "" {
			m["fulfillment_type"] = ft
		}
		return m
	}
	cases := []struct {
		name    string
		subtype string
		meta    map[string]any
		want    Channel
	}{
		{"pos dine-in", "dine_in", nil, DineIn},
		{"empty subtype is dine-in", "", map[string]any{}, DineIn},
		{"unknown subtype is dine-in", "something", nil, DineIn},
		{"pos takeaway", "takeaway", nil, Takeaway},
		{"pos delivery", "delivery", nil, Delivery},
		{"room service", "room_service", nil, RoomService},
		{"bar tab", "bar_tab", nil, BarTab},
		{"services job", "service_job", nil, ServiceJob},
		{"online pickup is a takeaway subtype", "takeaway", online("pickup"), OnlinePickup},
		{"online delivery by fulfilment type", "delivery", online("delivery"), OnlineDelivery},
		{"online delivery without fulfilment type falls back to subtype", "delivery", online(""), OnlineDelivery},
		{"online without fulfilment type and takeaway subtype is pickup", "takeaway", online(""), OnlinePickup},
		{"blank online id is not online", "takeaway", map[string]any{"online_order_id": "  "}, Takeaway},
	}
	for _, c := range cases {
		if got := Of(c.subtype, c.meta); got != c.want {
			t.Errorf("%s: Of(%q) = %q, want %q", c.name, c.subtype, got, c.want)
		}
	}
}

func TestIsCounterHandover(t *testing.T) {
	for _, c := range []Channel{Takeaway, Delivery, OnlinePickup, OnlineDelivery} {
		if !c.IsCounterHandover() {
			t.Errorf("%s should hand over at the counter", c)
		}
	}
	for _, c := range []Channel{DineIn, RoomService, BarTab, Retail, ServiceJob} {
		if c.IsCounterHandover() {
			t.Errorf("%s should not hand over at the counter", c)
		}
	}
}

func TestOnlineLabel(t *testing.T) {
	if got := OnlineLabel("takeaway", nil); got != "" {
		t.Errorf("POS order label = %q, want empty", got)
	}
	if got := OnlineLabel("takeaway", map[string]any{"online_order_id": "x", "fulfillment_type": "pickup"}); got != "Online pickup" {
		t.Errorf("pickup label = %q", got)
	}
	meta := map[string]any{"online_order_id": "x", "fulfillment_type": "delivery", "scheduled_for_label": "Fri 18:30"}
	if got := OnlineLabel("delivery", meta); got != "Online delivery for Fri 18:30" {
		t.Errorf("scheduled delivery label = %q", got)
	}
	if Source(meta) != SourceOnline || Source(nil) != SourcePOS {
		t.Errorf("Source mismatch")
	}
}
