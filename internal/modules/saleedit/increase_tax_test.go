package saleedit

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/returns"
	"github.com/bengobox/pos-service/internal/modules/reversals"
)

// TestEdit_NonFiscalized_IncreaseAppliesOutletFallbackTax is the regression test for the live bug
// found 2026-08-07: applyInPlaceIncrease's added-line tax computation only ever looked at the
// client-submitted TaxRate (nil for any item with no catalog tax_rate) via the old lineTaxAmount
// helper — so a brand-new line added through Edit Sale to a fallback-VAT tenant's order was
// posted with ZERO tax, silently, from the moment it was created. This mirrors the matching
// CreateOrder/AddOrderLines bug fixed in orders.ResolveLineTaxes (see that function's own doc
// comment) — applyInPlaceIncrease must resolve an added line's tax the SAME way, including the
// outlet's flat fallback VAT rate, not just whatever rate the client happened to send.
func TestEdit_NonFiscalized_IncreaseAppliesOutletFallbackTax(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:saleedit_orch_"+uuid.NewString()+"?mode=memory&cache=shared")
	t.Cleanup(func() { _ = client.Close() })
	// TaxRatePercent:16 with no OutletSetting row seeded — OutletFallbackTaxRate falls through to
	// this service-level default, exactly like a real outlet with no per-outlet VAT override.
	orderSvc := orders.NewService(client, orders.Config{DefaultCurrency: "KES", TaxRatePercent: 16}, zap.NewNop())
	revSvc := reversals.NewService(zap.NewNop(), client, orderSvc, nil, nil)
	returnsSvc := returns.NewService(zap.NewNop(), client, nil, nil)
	returnsSvc.SetOrderService(orderSvc)
	svc := NewService(zap.NewNop(), client, revSvc)
	svc.SetOrderService(orderSvc)
	svc.SetReturnsService(returnsSvc)

	tid := uuid.New()
	outletID := uuid.New()
	order := seedOrchestratorOrder(t, client, tid, outletID, 5, 100) // total 500, untaxed line
	line := onlyLine(t, client, order.ID)

	_, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "add item", RequestedBy: uuid.New(),
		CustomerName: "Jane Doe", CustomerIdentifier: "+254700000001",
		Lines: []EditLine{
			{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 5, UnitPrice: 100}, // unchanged
			{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "New Item", Quantity: 1, UnitPrice: 100},
		},
	})
	if err != nil {
		t.Fatalf("Edit failed: %v", err)
	}

	lines, err := client.POSOrderLine.Query().Where(entposorderline.OrderID(order.ID)).All(context.Background())
	if err != nil || len(lines) != 2 {
		t.Fatalf("expected 2 lines on the order, got %d, err=%v", len(lines), err)
	}
	var newLine *ent.POSOrderLine
	for _, l := range lines {
		if l.Sku == "SKU-2" {
			newLine = l
		}
	}
	if newLine == nil {
		t.Fatal("added line SKU-2 not found")
	}
	if newLine.TaxAmount == nil || *newLine.TaxAmount != 16 {
		t.Errorf("added line TaxAmount = %v, want 16 (100 * outlet fallback 16%%)", newLine.TaxAmount)
	}
	if newLine.TaxRate == nil || *newLine.TaxRate != 16 {
		t.Errorf("added line TaxRate = %v, want 16", newLine.TaxRate)
	}

	reloaded, err := client.POSOrder.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloaded.TaxTotal != 16 {
		t.Errorf("order TaxTotal = %v, want 16", reloaded.TaxTotal)
	}
	// 500 (unchanged SKU-1) + 100 (new SKU-2) + 16 (fallback tax on the new line only) = 616.
	if reloaded.TotalAmount != 616 {
		t.Errorf("order TotalAmount = %v, want 616", reloaded.TotalAmount)
	}
}
