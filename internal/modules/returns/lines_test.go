package returns

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func customerReturn(order uuid.UUID, outlet uuid.UUID, lines ...LineInput) CreateReturnRequest {
	return CreateReturnRequest{OrderID: order, OutletID: outlet, ReturnType: "refund", Reason: "test", Lines: lines, RequestedBy: uuid.New()}
}

// Over-returning was accepted before 2026-10-02 (two gram-auto-spares returns claim double the
// quantity sold). Pending returns hold quantity too, so two open returns cannot claim one unit.
func TestCreateReturn_RejectsOverReturnAcrossOpenReturns(t *testing.T) {
	svc, client := newTestService(t)
	tid, outlet := uuid.New(), uuid.New()
	order := seedOrder(t, client, tid, outlet, "completed") // 1 line, qty 2
	lineID := firstLineID(t, client, order.ID)

	if _, err := svc.CreateReturn(context.Background(), tid, customerReturn(order.ID, outlet, LineInput{OrderLineID: lineID, Quantity: 3})); err == nil ||
		!strings.Contains(err.Error(), "only 2 left") {
		t.Fatalf("want over-return rejected with remaining qty, got %v", err)
	}
	if _, err := svc.CreateReturn(context.Background(), tid, customerReturn(order.ID, outlet, LineInput{OrderLineID: lineID, Quantity: 2})); err != nil {
		t.Fatalf("returning the full qty should pass: %v", err)
	}
	if _, err := svc.CreateReturn(context.Background(), tid, customerReturn(order.ID, outlet, LineInput{OrderLineID: lineID, Quantity: 1})); err == nil ||
		!strings.Contains(err.Error(), "only 0 left") {
		t.Fatalf("a second open return must not claim already-claimed qty, got %v", err)
	}
}

// The order line is authoritative: SKU/name come from it, the refund is prorated from what the
// line charged, a foreign line is refused, and the outlet defaults to the sale's outlet.
func TestCreateReturn_UsesOrderLineAsSourceOfTruth(t *testing.T) {
	svc, client := newTestService(t)
	tid, outlet := uuid.New(), uuid.New()
	order := seedOrder(t, client, tid, outlet, "completed") // qty 2, total 500
	lineID := firstLineID(t, client, order.ID)

	ret, err := svc.CreateReturn(context.Background(), tid, customerReturn(order.ID, uuid.Nil,
		LineInput{OrderLineID: lineID, SKU: "TAMPERED", Name: "x", Quantity: 1, UnitPrice: 9999, TotalPrice: 9999}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ret.OutletID != outlet {
		t.Errorf("outlet should default to the sale's outlet %s, got %s", outlet, ret.OutletID)
	}
	if ret.RefundAmount != 250 {
		t.Errorf("refund should be prorated from the line total (500/2), got %.2f", ret.RefundAmount)
	}
	lines, _ := client.POSReturnLine.Query().All(context.Background())
	if len(lines) != 1 || lines[0].Sku != "SKU-1" || lines[0].Name != "Sample Item" {
		t.Errorf("line SKU/name must come from the order line, got %+v", lines)
	}

	if _, err := svc.CreateReturn(context.Background(), tid, customerReturn(order.ID, outlet,
		LineInput{OrderLineID: uuid.New(), SKU: "NOPE", Name: "Ghost", Quantity: 1})); err == nil ||
		!strings.Contains(err.Error(), "not on this sale") {
		t.Fatalf("a line from another sale must be refused, got %v", err)
	}
	if _, err := svc.CreateReturn(context.Background(), tid, customerReturn(order.ID, outlet,
		LineInput{OrderLineID: lineID, Quantity: 0})); err == nil {
		t.Fatalf("zero quantity must be refused")
	}
}

// Older clients sent no order_line_id: an unambiguous SKU still resolves.
func TestMatchOrderLineBySKU(t *testing.T) {
	svc, client := newTestService(t)
	tid, outlet := uuid.New(), uuid.New()
	order := seedOrder(t, client, tid, outlet, "completed")
	ret, err := svc.CreateReturn(context.Background(), tid, customerReturn(order.ID, outlet, LineInput{SKU: "SKU-1", Quantity: 1}))
	if err != nil {
		t.Fatalf("sku-only line should resolve: %v", err)
	}
	if ret.RefundAmount != 250 {
		t.Errorf("want 250, got %.2f", ret.RefundAmount)
	}
}
