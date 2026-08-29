package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/posorder"
)

// TestResolveSaleSettlement_OnAccountWithoutTenderRow is the regression test for a
// live bug: a payment's tender type is derived from a `tenders` table lookup by tender_id, but a
// tenant with zero rows in that table (confirmed live: boi-enterprises) can never resolve one —
// tType stays "", so a check against the raw tType (instead of the PaymentData["method"]-fallback
// variable already used for the complimentary case) permanently sees onAccount=0 regardless of
// the real payment method, silently misclassifying every credit sale as "cash" and causing the
// pos.sale.finalized subscriber to post a spurious, full-amount "settled" AR line alongside the
// real credit_sale line for the same order (boi-enterprises orders 000187, 000278 — confirmed).
func TestResolveSaleSettlement_OnAccountWithoutTenderRow(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	orderID := uuid.New()
	seedOrderForPayment(t, client, orderID)
	// seedCashPayment assigns a random tender_id with no matching Tender row — exactly the
	// zero-rows-in-tenders scenario.
	seedCashPayment(t, svc, orderID, TenderOnAccount)

	order, err := client.POSOrder.Query().Where(posorder.ID(orderID)).Only(context.Background())
	if err != nil {
		t.Fatalf("load order: %v", err)
	}

	scheme, onAccount, _, _, _ := svc.resolveSaleSettlement(context.Background(), order)
	if scheme != "credit" {
		t.Errorf("scheme = %q, want %q (order.TotalAmount=%v fully on-account)", scheme, "credit", order.TotalAmount)
	}
	if onAccount != order.TotalAmount {
		t.Errorf("onAccount = %v, want %v (the full order total)", onAccount, order.TotalAmount)
	}
}
