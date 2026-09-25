package orders

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

func TestKDSTicketItemCarriesQuantityModifiersAndNotes(t *testing.T) {
	line := &ent.POSOrderLine{
		ID:       uuid.New(),
		Sku:      "LATTE",
		Name:     "Cafe Latte",
		Quantity: 2,
		Metadata: map[string]any{
			"modifiers": []any{
				map[string]any{"group_name": "Milk", "option_name": "Oat"},
				map[string]any{"option_name": "Extra shot"},
			},
			"notes": " no sugar ",
		},
	}
	item := kdsTicketItem(line)
	if item["quantity"] != 2.0 || item["qty"] != 2.0 {
		t.Fatalf("quantity must be present under both keys: %v", item)
	}
	mods, _ := item["modifiers"].([]string)
	if len(mods) != 2 || mods[0] != "Milk: Oat" || mods[1] != "Extra shot" {
		t.Fatalf("modifier labels wrong: %v", mods)
	}
	if item["notes"] != "no sugar" {
		t.Fatalf("notes wrong: %v", item["notes"])
	}
}

func TestKDSTicketItemPrefersPersistedModifiers(t *testing.T) {
	line := &ent.POSOrderLine{
		Name:     "Burger",
		Quantity: 1,
		Metadata: map[string]any{"modifiers": []any{map[string]any{"option_name": "stale"}}},
		Edges:    ent.POSOrderLineEdges{Modifiers: []*ent.POSLineModifier{{Name: "No onions"}}},
	}
	mods, _ := kdsTicketItem(line)["modifiers"].([]string)
	if len(mods) != 1 || mods[0] != "No onions" {
		t.Fatalf("persisted modifier rows must win: %v", mods)
	}
}

func TestKDSTicketItemPlainLine(t *testing.T) {
	item := kdsTicketItem(&ent.POSOrderLine{Name: "Water", Quantity: 1})
	if _, ok := item["modifiers"]; ok {
		t.Fatalf("no modifiers expected: %v", item)
	}
	if _, ok := item["notes"]; ok {
		t.Fatalf("no notes expected: %v", item)
	}
}

func TestFulfillmentRouting(t *testing.T) {
	src, sub, ch := fulfillmentRouting("delivery")
	if src != "online_delivery" || sub != "delivery" || ch != channelDelivery {
		t.Fatalf("delivery routing wrong: %s %s %s", src, sub, ch)
	}
	src, sub, ch = fulfillmentRouting("pickup")
	if src != "click_and_collect" || sub != "takeaway" || ch != channelClickAndCollect {
		t.Fatalf("pickup routing wrong: %s %s %s", src, sub, ch)
	}
	if _, sub, _ := fulfillmentRouting("anything-else"); sub != "takeaway" {
		t.Fatalf("unknown fulfilment should default to pickup, got %s", sub)
	}
}

func TestOnlineOrderMetadataPrepaidVsPayOnCollection(t *testing.T) {
	nairobi := time.FixedZone("EAT", 3*3600)
	paid := onlineOrderMetadata(map[string]interface{}{
		"payment_method": "mpesa",
		"payment_status": "paid",
		"grand_total":    1200.0,
		"order_number":   "ORD-7",
	}, "abc", "click_and_collect", "takeaway", "pickup", nairobi)
	if paid["prepaid"] != true || paid["amount_due"] != 0.0 {
		t.Fatalf("prepaid order must owe nothing at the counter: %v", paid)
	}

	cod := onlineOrderMetadata(map[string]interface{}{
		"payment_method":   "cod",
		"payment_status":   "cod_pending",
		"grand_total":      850.0,
		"delivery_address": "Moi Avenue 12",
		"instructions":     "Ring the bell",
		"scheduled_for":    "2026-10-02T15:30:00Z",
	}, "def", "online_delivery", "delivery", "delivery", nairobi)
	if cod["prepaid"] != false || cod["amount_due"] != 850.0 {
		t.Fatalf("COD order must show the amount to collect: %v", cod)
	}
	if cod["delivery_address"] != "Moi Avenue 12" || cod["order_notes"] != "Ring the bell" {
		t.Fatalf("destination/notes missing: %v", cod)
	}
	if cod["scheduled_for_label"] != "Fri 18:30" {
		t.Fatalf("scheduled label must be in the tenant timezone, got %v", cod["scheduled_for_label"])
	}
}

func TestAppointmentWindow(t *testing.T) {
	nairobi := time.FixedZone("EAT", 3*3600)
	svc := confirmedItemData{
		Name:     "Haircut",
		Quantity: 1,
		Metadata: map[string]interface{}{
			"appointment_date": "2026-10-01",
			"appointment_time": "14:30",
			"duration_minutes": 45.0,
		},
	}
	start, end := appointmentWindow(svc, nairobi)
	if start.Hour() != 14 || start.Minute() != 30 || start.Location() != nairobi {
		t.Fatalf("start wrong: %v", start)
	}
	if end.Sub(start) != 45*time.Minute {
		t.Fatalf("duration wrong: %v", end.Sub(start))
	}

	// Two back-to-back units of the same service book twice the time; no duration falls back to 60.
	svc.Quantity = 2
	delete(svc.Metadata, "duration_minutes")
	start, end = appointmentWindow(svc, nairobi)
	if end.Sub(start) != 120*time.Minute {
		t.Fatalf("expected 120 min for two units at the default length, got %v", end.Sub(start))
	}

	// A booking without a usable date still lands on the calendar (now) instead of failing.
	start, _ = appointmentWindow(confirmedItemData{Quantity: 1}, nairobi)
	if time.Since(start) > time.Minute {
		t.Fatalf("missing date should book now, got %v", start)
	}
}

func TestAppointmentNotes(t *testing.T) {
	notes := appointmentNotes("ORD-9", confirmedItemData{
		Name:     "Full service",
		Metadata: map[string]interface{}{"deposit_percent": 30.0},
	}, true, "Bring the logbook")
	for _, want := range []string{"ORD-9", "Full service", "paid online", "deposit 30%", "Bring the logbook"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("notes %q missing %q", notes, want)
		}
	}
	if !strings.Contains(appointmentNotes("ORD-1", confirmedItemData{Name: "Nails"}, false, ""), "pay at the appointment") {
		t.Fatal("unpaid booking must say it is paid at the appointment")
	}
}

func TestConfirmedItemIsService(t *testing.T) {
	if (confirmedItemData{}).isService() {
		t.Fatal("plain line is not a service")
	}
	if !(confirmedItemData{Metadata: map[string]interface{}{"is_service": true}}).isService() {
		t.Fatal("is_service line must be a service")
	}
}

func TestIsLifecycleStatus(t *testing.T) {
	for _, s := range []string{"cancelled", "out_for_delivery", "delivered", "completed", "refunded"} {
		if !isLifecycleStatus(s) {
			t.Fatalf("%s should be mirrored", s)
		}
	}
	for _, s := range []string{"confirmed", "preparing", "ready", "pending"} {
		if isLifecycleStatus(s) {
			t.Fatalf("%s should not be mirrored", s)
		}
	}
}
