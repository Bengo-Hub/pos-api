package printing

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// TestTenderedAndChange_ReadsSurplusFromPaymentData is the read side of the alpha-china-market
// order #000530 fix: cashPaymentData (payments package) writes the customer's real tendered
// amount + change into PaymentData when a cash tender exceeds the sale total; this proves
// TenderedAndChange reads it back correctly for the receipt (order 000530 itself: total 1,820,
// tendered 2,000, change 180).
func TestTenderedAndChange_ReadsSurplusFromPaymentData(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:printing_tendered_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	order, err := client.POSOrder.Create().
		SetTenantID(uuid.New()).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("000530").SetStatus("completed").
		SetSubtotal(1820).SetTaxTotal(0).SetTotalAmount(1820).SetPaidTotal(1820).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).
		SetTenderID(uuid.New()).
		SetAmount(1820).
		SetCurrency("KES").
		SetStatus("completed").
		SetPaymentData(map[string]any{"method": "cash", "amount_tendered": 2000.0, "change_due": 180.0}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	tendered, change := TenderedAndChange([]*ent.POSPayment{payment}, payment.Amount)
	if tendered != 2000 {
		t.Errorf("tendered = %v, want 2000", tendered)
	}
	if change != 180 {
		t.Errorf("change = %v, want 180", change)
	}
}

// TestTenderedAndChange_NoSurplusIsANoOp covers an exact-cash payment (no amount_tendered/
// change_due keys — either an exact sale, a non-cash tender, or any payment recorded before this
// fix existed): the result must be identical to the pre-fix behaviour, (amountPaid, 0).
func TestTenderedAndChange_NoSurplusIsANoOp(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:printing_tendered_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	order, err := client.POSOrder.Create().
		SetTenantID(uuid.New()).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("000531").SetStatus("completed").
		SetSubtotal(500).SetTaxTotal(0).SetTotalAmount(500).SetPaidTotal(500).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(500).SetCurrency("KES").SetStatus("completed").
		SetPaymentData(map[string]any{"method": "mpesa_manual"}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	tendered, change := TenderedAndChange([]*ent.POSPayment{payment}, payment.Amount)
	if tendered != 500 || change != 0 {
		t.Errorf("got (%v, %v), want (500, 0)", tendered, change)
	}

	// nil rows (no completed payment loaded) must not panic and stays a no-op too.
	tendered, change = TenderedAndChange(nil, 0)
	if tendered != 0 || change != 0 {
		t.Errorf("nil rows: got (%v, %v), want (0, 0)", tendered, change)
	}
}
