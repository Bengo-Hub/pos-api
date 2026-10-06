package handlers

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

func TestBuildDeliveryTaskRequestCashOnDelivery(t *testing.T) {
	order := &ent.POSOrder{
		ID: uuid.New(), OutletID: uuid.New(), OrderNumber: "000123",
		TotalAmount: 1250.5, PaidTotal: 250,
		CustomerName: strp("Wanjiku"), CustomerPhone: strp("0722000000"),
		Metadata: map[string]any{"delivery_address": "Ngong Rd", "delivery_notes": "Gate B"},
	}
	req := buildDeliveryTaskRequest(order, "Urban Loft Cafe")
	if req.SourceService != "pos" || req.ExternalReference != order.ID.String() {
		t.Fatalf("task reference = %q/%q", req.SourceService, req.ExternalReference)
	}
	if req.PickupAddress != "Urban Loft Cafe" || req.DropoffAddress != "Ngong Rd" || req.Instructions != "Gate B" {
		t.Fatalf("addresses = %+v", req)
	}
	if got := req.Metadata["cash_on_delivery"]; got != 1000.5 {
		t.Fatalf("cash_on_delivery = %v, want the unpaid 1000.5", got)
	}

	order.PaidTotal = order.TotalAmount
	if _, ok := buildDeliveryTaskRequest(order, "").Metadata["cash_on_delivery"]; ok {
		t.Fatal("a paid order must not ask the rider to collect cash")
	}
}
