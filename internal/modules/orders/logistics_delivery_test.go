package orders

import (
	"testing"
	"time"
)

var deliveryNow = time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)

func TestDispatchStateFor(t *testing.T) {
	cases := map[string]string{
		"created": DispatchDispatched, "unassigned": DispatchDispatched,
		"assigned": DispatchRiderAssigned, "accepted": DispatchRiderAssigned,
		"en_route_pickup": DispatchRiderArriving, "arrived_pickup": DispatchRiderArriving,
		"picked_up": DispatchOutForDelivery, "en_route": DispatchOutForDelivery, "arrived_dropoff": DispatchOutForDelivery,
		"delivered": DispatchDelivered, "completed": DispatchDelivered,
		"failed": DispatchFailed, "cancelled": DispatchCancelled,
		"sla_breached": "", "eta_updated": "",
	}
	for status, want := range cases {
		if got := dispatchStateFor(status); got != want {
			t.Errorf("dispatchStateFor(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestApplyDeliveryEventProgressNeverRegresses(t *testing.T) {
	meta := map[string]any{"dispatch_status": DispatchOutForDelivery, "logistics_task_id": "t1"}
	upd := applyDeliveryEvent(meta, 1000, 0, "assigned", map[string]any{}, deliveryNow)
	if upd.changed || upd.meta["dispatch_status"] != DispatchOutForDelivery {
		t.Fatalf("late assigned regressed the order: %+v", upd)
	}
	// The input map is never mutated.
	if _, ok := meta["dispatch_updated_at"]; ok {
		t.Fatal("input metadata was mutated")
	}
}

func TestApplyDeliveryEventRiderDetails(t *testing.T) {
	upd := applyDeliveryEvent(map[string]any{"dispatch_status": DispatchDispatched}, 1000, 0, "assigned",
		map[string]any{"fleet_member_id": "r1", "rider_name": "Otieno", "rider_phone": "0711"}, deliveryNow)
	if !upd.changed || upd.meta["rider_name"] != "Otieno" || upd.meta["rider_phone"] != "0711" || upd.meta["rider_id"] != "r1" {
		t.Fatalf("rider details not stamped: %+v", upd.meta)
	}
	if upd.complete {
		t.Fatal("assignment must not close the order")
	}
	picked := applyDeliveryEvent(upd.meta, 1000, 0, "picked_up", map[string]any{}, deliveryNow)
	if picked.meta["dispatch_status"] != DispatchOutForDelivery || picked.meta["left_counter_at"] == nil {
		t.Fatalf("pickup not recorded: %+v", picked.meta)
	}
}

func TestApplyDeliveryEventUnassignedBeforePickup(t *testing.T) {
	meta := map[string]any{"dispatch_status": DispatchRiderAssigned, "rider_id": "r1", "rider_name": "Otieno"}
	upd := applyDeliveryEvent(meta, 500, 0, "unassigned", map[string]any{"rider_name": "Otieno"}, deliveryNow)
	if upd.meta["dispatch_status"] != DispatchDispatched || upd.meta["rider_name"] != nil || upd.meta["rider_id"] != nil {
		t.Fatalf("unassigned did not reopen the dispatch: %+v", upd.meta)
	}
	// After the rider left with the order, a stale unassigned is ignored.
	gone := applyDeliveryEvent(map[string]any{"dispatch_status": DispatchOutForDelivery, "rider_name": "Otieno"}, 500, 0, "unassigned", nil, deliveryNow)
	if gone.changed || gone.meta["rider_name"] != "Otieno" {
		t.Fatalf("stale unassigned applied: %+v", gone.meta)
	}
}

func TestApplyDeliveryEventFailedFreesRedispatch(t *testing.T) {
	meta := map[string]any{"dispatch_status": DispatchOutForDelivery, "logistics_task_id": "t1"}
	upd := applyDeliveryEvent(meta, 500, 0, "failed", map[string]any{"reason": " customer unreachable "}, deliveryNow)
	if upd.meta["dispatch_status"] != DispatchFailed || upd.meta["logistics_task_id"] != nil ||
		upd.meta["ended_logistics_task_id"] != "t1" || upd.meta["dispatch_reason"] != "customer unreachable" {
		t.Fatalf("failed dispatch not reset: %+v", upd.meta)
	}
	if upd.complete {
		t.Fatal("a failed delivery must not close the order")
	}
}

func TestApplyDeliveryEventDeliveredUnpaidStaysOnQueue(t *testing.T) {
	upd := applyDeliveryEvent(map[string]any{"dispatch_status": DispatchOutForDelivery}, 850, 0, "delivered",
		map[string]any{"cash_collected": true, "amount_collected": 850.0, "collection_method": "mpesa", "collection_reference": "sgh12ab"}, deliveryNow)
	if upd.complete || upd.meta["collected"] != nil {
		t.Fatalf("unpaid delivery closed: %+v", upd.meta)
	}
	if upd.meta["dispatch_status"] != DispatchDelivered || upd.meta["cod_collected"] != true ||
		upd.meta["cod_amount_collected"] != 850.0 || upd.meta["cod_method"] != "mpesa" || upd.meta["cod_reference"] != "SGH12AB" {
		t.Fatalf("cash at the door not recorded: %+v", upd.meta)
	}
	// Delivered is terminal: a duplicate completed event changes nothing.
	again := applyDeliveryEvent(upd.meta, 850, 0, "completed", nil, deliveryNow)
	if again.changed {
		t.Fatal("duplicate delivered event applied twice")
	}
}

func TestApplyDeliveryEventDeliveredPaidCloses(t *testing.T) {
	upd := applyDeliveryEvent(map[string]any{"dispatch_status": DispatchOutForDelivery}, 850, 850, "completed", nil, deliveryNow)
	if !upd.complete || upd.meta["collected"] != true || upd.meta["cod_collected"] != nil {
		t.Fatalf("paid delivery not closed: %+v", upd.meta)
	}
}
