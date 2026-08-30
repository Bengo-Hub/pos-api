package saleedit

import (
	"context"
	"testing"

	"github.com/google/uuid"

	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
)

// TestEdit_NonFiscalized_IncreaseOnCashSale_CollectsNowWithoutCustomer is the regression test
// for the live BOI Enterprises bug (order 001514, MR KIM KIMTEC MALABA): a 100%-cash sale's
// Edit-Sale increase used to hardcode SellingScheme="credit" regardless of the original tender,
// requiring a customer/phone and posting a phantom AR debt for money that was really just a
// cash top-up. With an active "cash" tender configured and the original order NOT on-account,
// the increase must succeed with NO customer at all, record a real completed payment for the
// incremental amount, and leave metadata.on_account unset.
func TestEdit_NonFiscalized_IncreaseOnCashSale_CollectsNowWithoutCustomer(t *testing.T) {
	svc, client := newOrchestratorTestService(t)
	tid := uuid.New()
	outletID := uuid.New()
	cashTender, err := client.Tender.Create().
		SetTenantID(tid).SetName("Cash").SetType("cash").SetIsActive(true).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed cash tender: %v", err)
	}
	order := seedOrchestratorOrder(t, client, tid, outletID, 5, 100) // total 500, no customer
	// A real cash sale always has a matching completed POSPayment row backing paid_total (unlike
	// seedOrchestratorOrder's bare SetPaidTotal, which only exists to keep unrelated tests
	// simple) — seed one here so recomputePaidTotal's real sum-of-payments logic has something
	// real to sum, matching production.
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(cashTender.ID).SetAmount(500).SetCurrency("KES").
		SetStatus("completed").SetPaymentData(map[string]any{"method": "cash"}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed original cash payment: %v", err)
	}
	line := onlyLine(t, client, order.ID)

	result, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "add item for walk-in cash sale", RequestedBy: uuid.New(),
		Lines: []EditLine{
			{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 5, UnitPrice: 100}, // unchanged
			{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "New Item", Quantity: 1, UnitPrice: 50},
		},
	})
	if err != nil {
		t.Fatalf("expected the increase to succeed with no customer (cash tender available, original sale not on-account), got: %v", err)
	}
	if result.Kind != "increase" {
		t.Fatalf("expected kind=increase, got %q", result.Kind)
	}

	reloaded, err := client.POSOrder.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloaded.TotalAmount != 550 {
		t.Errorf("order TotalAmount = %.2f, want 550", reloaded.TotalAmount)
	}
	if reloaded.PaidTotal != 550 {
		t.Errorf("order PaidTotal = %.2f, want 550 (the incremental 50 must be recorded as collected)", reloaded.PaidTotal)
	}
	if on, _ := reloaded.Metadata["on_account"].(bool); on {
		t.Errorf("metadata.on_account = true, want unset — a cash top-up must never be classified as a credit sale")
	}

	pays, err := client.POSPayment.Query().
		Where(entpospayment.OrderID(order.ID), entpospayment.Amount(50)).All(context.Background())
	if err != nil {
		t.Fatalf("load payments: %v", err)
	}
	if len(pays) != 1 {
		t.Fatalf("expected exactly 1 payment row recorded for the 50 increment, got %d", len(pays))
	}
	if method, _ := pays[0].PaymentData["method"].(string); method != "cash" {
		t.Errorf("payment method = %q, want cash", method)
	}
}

// TestEdit_NonFiscalized_IncreaseOnGenuineCreditSale_StaysCredit confirms a real on-account
// sale's increase is UNCHANGED by this fix — still requires a customer and still bills the
// customer's account — even when an active cash tender exists, proving the fix only changes
// the DEFAULT for a cash-originated sale, not genuine credit sales (matches the live BOI order
// 001554, MR ALBERT BUNGOMA, where this exact code path already produced a correct result).
func TestEdit_NonFiscalized_IncreaseOnGenuineCreditSale_StaysCredit(t *testing.T) {
	svc, client := newOrchestratorTestService(t)
	tid := uuid.New()
	outletID := uuid.New()
	if _, err := client.Tender.Create().
		SetTenantID(tid).SetName("Cash").SetType("cash").SetIsActive(true).
		Save(context.Background()); err != nil {
		t.Fatalf("seed cash tender: %v", err)
	}
	order := seedOrchestratorOrder(t, client, tid, outletID, 5, 100)
	if _, err := order.Update().SetMetadata(map[string]any{"on_account": true}).Save(context.Background()); err != nil {
		t.Fatalf("seed on_account metadata: %v", err)
	}
	line := onlyLine(t, client, order.ID)

	// No customer supplied — must still be refused exactly as before this fix.
	if _, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "add item to credit sale", RequestedBy: uuid.New(),
		Lines: []EditLine{
			{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 5, UnitPrice: 100},
			{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "New Item", Quantity: 1, UnitPrice: 50},
		},
	}); err == nil {
		t.Fatal("expected the increase to still be refused without a customer for a genuine credit sale")
	}

	result, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "add item to credit sale, with customer", RequestedBy: uuid.New(),
		CustomerName: "Jane Doe", CustomerIdentifier: "+254700000003",
		Lines: []EditLine{
			{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 5, UnitPrice: 100},
			{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "New Item", Quantity: 1, UnitPrice: 50},
		},
	})
	if err != nil {
		t.Fatalf("expected the increase to succeed once a customer is attached: %v", err)
	}
	if result.Kind != "increase" {
		t.Fatalf("expected kind=increase, got %q", result.Kind)
	}

	// No payment row should have been fabricated — the increment is still owed, not collected.
	pays, err := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).All(context.Background())
	if err != nil {
		t.Fatalf("load payments: %v", err)
	}
	if len(pays) != 0 {
		t.Errorf("expected 0 payment rows for a genuine credit increase, got %d", len(pays))
	}
	reloaded, err := client.POSOrder.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloaded.PaidTotal != 500 {
		t.Errorf("order PaidTotal = %.2f, want unchanged 500 (nothing new was collected)", reloaded.PaidTotal)
	}
}

// TestEdit_NonFiscalized_IncreaseSettlement_ExplicitCreditOverride confirms an admin can
// deliberately bill a cash-originated sale's increase to a customer's account by setting
// IncreaseSettlement="credit" explicitly, even with a cash tender available.
func TestEdit_NonFiscalized_IncreaseSettlement_ExplicitCreditOverride(t *testing.T) {
	svc, client := newOrchestratorTestService(t)
	tid := uuid.New()
	outletID := uuid.New()
	if _, err := client.Tender.Create().
		SetTenantID(tid).SetName("Cash").SetType("cash").SetIsActive(true).
		Save(context.Background()); err != nil {
		t.Fatalf("seed cash tender: %v", err)
	}
	order := seedOrchestratorOrder(t, client, tid, outletID, 5, 100)
	line := onlyLine(t, client, order.ID)

	if _, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "bill this addition on credit", RequestedBy: uuid.New(),
		CustomerName: "Jane Doe", CustomerIdentifier: "+254700000004",
		IncreaseSettlement: "credit",
		Lines: []EditLine{
			{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 5, UnitPrice: 100},
			{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "New Item", Quantity: 1, UnitPrice: 50},
		},
	}); err != nil {
		t.Fatalf("expected explicit credit override to succeed: %v", err)
	}

	reloaded, err := client.POSOrder.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if on, _ := reloaded.Metadata["on_account"].(bool); !on {
		t.Errorf("metadata.on_account = %v, want true — explicit credit override must still bill the customer's account", on)
	}
	pays, err := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).All(context.Background())
	if err != nil {
		t.Fatalf("load payments: %v", err)
	}
	if len(pays) != 0 {
		t.Errorf("expected 0 payment rows for an explicit credit override, got %d", len(pays))
	}
}
