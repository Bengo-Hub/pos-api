package saleedit

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// applyInPlaceIncrease is the TRUE in-place path for a non-fiscalized order's added lines /
// qty increases: new POSOrderLine rows (or bumped quantities) are written directly onto the
// SAME order — no new order, no new receipt. Mints a fresh reference id (editID, already
// created by the caller as the POSSaleEdit row's own id) for the inventory/GL/AR calls, so
// they never collide with the original sale's own idempotency-guarded postings (which are
// keyed on the ORDER id, permanently). The incremental value always posts as a receivable
// (Dr AR / Cr Revenue) regardless of the original sale's tender — until the till actually
// collects it (via the existing Record-Payment/settle-credit flow, unchanged), it IS a
// receivable; posting a "cash received" entry here would be false (no cash has changed
// hands yet). This mirrors exactly what a fresh unpaid/on-account sale would post.
func (s *Service) applyInPlaceIncrease(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, editID uuid.UUID, diff lineDiff, req EditSaleRequest) error {
	var incrementalAmount, incrementalTax, incrementalCost float64
	consumptionItems := make([]inventory.ConsumptionItem, 0, len(diff.added)+len(diff.increased))

	for _, a := range diff.added {
		total := round2(a.UnitPrice * a.Quantity)
		taxAmt := lineTaxAmount(total, a.PriceIncludesTax, a.TaxRate)
		create := s.client.POSOrderLine.Create().
			SetOrderID(order.ID).
			SetCatalogItemID(a.CatalogItemID).
			SetSku(a.SKU).
			SetName(a.Name).
			SetQuantity(a.Quantity).
			SetUnitPrice(a.UnitPrice).
			SetTotalPrice(total).
			SetPriceIncludesTax(a.PriceIncludesTax).
			SetMetadata(map[string]any{"sale_edit_id": editID.String(), "edit_added": true})
		if a.TaxCodeID != "" {
			create = create.SetTaxCodeID(a.TaxCodeID)
		}
		if a.TaxRate != nil {
			create = create.SetTaxRate(*a.TaxRate)
		}
		if taxAmt != nil {
			create = create.SetTaxAmount(*taxAmt)
		}
		if _, err := create.Save(ctx); err != nil {
			return fmt.Errorf("create line: %w", err)
		}
		incrementalAmount += total
		if taxAmt != nil && !a.PriceIncludesTax {
			incrementalTax += *taxAmt
		}
		if a.SKU != "" {
			consumptionItems = append(consumptionItems, inventory.ConsumptionItem{SKU: a.SKU, Quantity: a.Quantity})
		}
	}

	for _, inc := range diff.increased {
		deltaTotal := round2(inc.Req.UnitPrice * inc.ByQty)
		newQty := round2(inc.Line.Quantity + inc.ByQty)
		newTotal := round2(inc.Line.TotalPrice + deltaTotal)
		if _, err := inc.Line.Update().SetQuantity(newQty).SetTotalPrice(newTotal).Save(ctx); err != nil {
			return fmt.Errorf("bump line %s: %w", inc.Line.ID, err)
		}
		incrementalAmount += deltaTotal
		if inc.Line.TaxAmount != nil && !inc.Line.PriceIncludesTax && inc.Line.Quantity > 0 {
			// Prorate the line's own existing tax rate onto the incremental quantity.
			perUnitTax := *inc.Line.TaxAmount / inc.Line.Quantity
			incrementalTax += round2(perUnitTax * inc.ByQty)
		}
		if inc.Line.Sku != "" {
			consumptionItems = append(consumptionItems, inventory.ConsumptionItem{SKU: inc.Line.Sku, Quantity: inc.ByQty})
		}
	}

	if incrementalAmount <= 0.009 {
		return nil // nothing of value was actually added (e.g. a zero-price line)
	}

	if _, err := orders.RecomputeTotalsWithClient(ctx, s.client, tenantID, order.ID); err != nil {
		return fmt.Errorf("recompute totals: %w", err)
	}

	// The incremental amount posts as a receivable, not collected cash (see the treasury call
	// below) — stamp on_account so orders.DerivePaymentStatus/ComputeSettlement, the sole source
	// of truth for "how much is still owed," stop reading this order as fully paid now that its
	// total exceeds what was actually collected at checkout. Found live during E2E verification
	// 2026-08-05: a cash sale bumped from 500->1500 via this path kept showing payment_status
	// "paid" with amount_due 1400, because "status==completed" was previously read as an
	// unconditional proxy for "fully paid," an invariant this increase path breaks by design.
	if !orders.IsOnAccount(order.Metadata) {
		md := make(map[string]any, len(order.Metadata)+1)
		for k, v := range order.Metadata {
			md[k] = v
		}
		md["on_account"] = true
		if _, err := order.Update().SetMetadata(md).Save(ctx); err != nil {
			return fmt.Errorf("stamp on_account metadata: %w", err)
		}
	}

	if s.inventoryClient != nil && len(consumptionItems) > 0 {
		if _, err := s.inventoryClient.RecordConsumption(ctx, tenantID.String(), inventory.ConsumptionRequest{
			OrderID: order.ID.String(), Items: consumptionItems,
			Reason: "sale_edit_increase", IdempotencyKey: "pos-edit-increase-" + editID.String(),
		}); err != nil {
			s.log.Warn("edit increase: inventory consumption failed (non-fatal; can be reconciled)",
				zap.String("order_id", order.ID.String()), zap.String("edit_id", editID.String()), zap.Error(err))
		}
	}

	if s.treasuryClient != nil {
		crmContactID, customerName, customerIdentifier := orders.ResolveOrderCustomer(ctx, s.client, tenantID, order.ID)
		if req.CustomerName != "" {
			customerName = req.CustomerName
		}
		if req.CustomerIdentifier != "" {
			customerIdentifier = req.CustomerIdentifier
		}
		if req.CrmContactID != nil {
			crmContactID = req.CrmContactID.String()
		}
		if _, err := s.treasuryClient.PostSaleEditGL(ctx, req.TenantSlug, treasury.SaleEditGLRequest{
			ReferenceID: editID.String(), OrderID: order.ID.String(), OrderNumber: order.OrderNumber,
			OutletID: order.OutletID.String(), SellingScheme: "credit",
			Amount: incrementalAmount, TaxAmount: incrementalTax, CreditAmount: incrementalAmount, CostAmount: incrementalCost,
			Currency: order.Currency, CrmContactID: crmContactID, CustomerIdentifier: customerIdentifier, CustomerName: customerName,
			UserID: req.RequestedBy.String(), Description: "Edit Sale increase — " + order.OrderNumber,
		}); err != nil {
			s.log.Warn("edit increase: treasury GL post failed (non-fatal; can be reconciled)",
				zap.String("order_id", order.ID.String()), zap.String("edit_id", editID.String()), zap.Error(err))
		}
	}

	return nil
}

// lineTaxAmount computes a line's tax amount from its rate, mirroring
// orders.Service.EditOrderLine's own formula (rate× total, or the inclusive-price backout).
func lineTaxAmount(total float64, priceIncludesTax bool, taxRate *float64) *float64 {
	if taxRate == nil || *taxRate <= 0 {
		return nil
	}
	rate := *taxRate / 100
	amt := total * rate
	if priceIncludesTax {
		amt = total - total/(1+rate)
	}
	v := round2(amt)
	return &v
}

// createAddendumOrder is the fiscalized-increase path — UNCHANGED behavior from before this
// rework: a linked sub-order with its own docket/receipt, created through the ordinary
// create→finalize pipeline (fresh inventory consumption, GL, and a fresh KRA invoice since
// the tenant IS eTIMS-integrated), tagged metadata.related_order_id for lineage.
func (s *Service) createAddendumOrder(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, diff lineDiff, req EditSaleRequest) (uuid.UUID, error) {
	if s.orderSvc == nil {
		return uuid.Nil, fmt.Errorf("addendum order creation is not available (order service unwired)")
	}
	lines := make([]orders.OrderLineInput, 0, len(diff.added)+len(diff.increased))
	for _, a := range diff.added {
		lines = append(lines, orders.OrderLineInput{
			CatalogItemID: a.CatalogItemID, SKU: a.SKU, Name: a.Name, Quantity: a.Quantity,
			UnitPrice: a.UnitPrice, TotalPrice: round2(a.UnitPrice * a.Quantity),
			TaxCodeID: a.TaxCodeID, PriceIncludesTax: a.PriceIncludesTax, TaxRate: a.TaxRate,
		})
	}
	for _, inc := range diff.increased {
		lines = append(lines, orders.OrderLineInput{
			CatalogItemID: inc.Line.CatalogItemID, SKU: inc.Line.Sku, Name: inc.Line.Name, Quantity: inc.ByQty,
			UnitPrice: inc.Req.UnitPrice, TotalPrice: round2(inc.Req.UnitPrice * inc.ByQty),
			TaxCodeID: inc.Req.TaxCodeID, PriceIncludesTax: inc.Req.PriceIncludesTax, TaxRate: inc.Req.TaxRate,
		})
	}
	if len(lines) == 0 {
		return uuid.Nil, nil
	}

	customerName, customerPhone := "", ""
	if order.CustomerName != nil {
		customerName = *order.CustomerName
	}
	if order.CustomerPhone != nil {
		customerPhone = *order.CustomerPhone
	}
	if req.CustomerName != "" {
		customerName = req.CustomerName
	}
	if req.CustomerIdentifier != "" {
		customerPhone = req.CustomerIdentifier
	}

	addendum, err := s.orderSvc.CreateOrder(ctx, orders.CreateOrderRequest{
		TenantID: tenantID, TenantSlug: req.TenantSlug, OutletID: order.OutletID, UserID: req.RequestedBy,
		// An addendum sub-order must always match its parent's currency, not the outlet's
		// currently-configured one (which could theoretically have changed since the sale).
		Currency: order.Currency, Lines: lines, OrderSubtype: "retail", Source: "back_office",
		CustomerName: customerName, CustomerPhone: customerPhone,
		Metadata: map[string]any{
			"related_order_id": order.ID.String(),
			"edit_addendum":    true,
			"edit_reason":      req.Reason,
		},
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create addendum order: %w", err)
	}
	return addendum.ID, nil
}
