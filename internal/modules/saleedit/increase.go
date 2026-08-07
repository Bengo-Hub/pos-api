package saleedit

import (
	"context"
	"fmt"
	"time"

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
	// Dry-run the amount check before any write: if there's nothing of value to add, bail out
	// exactly like the real check below, before even resolving the customer.
	var dryRunAmount float64
	for _, a := range diff.added {
		dryRunAmount += round2(a.UnitPrice * a.Quantity)
	}
	for _, inc := range diff.increased {
		dryRunAmount += round2(inc.Req.UnitPrice * inc.ByQty)
	}
	if dryRunAmount <= 0.009 {
		return nil
	}

	// The incremental amount posts as AR (see the treasury call below) — refuse to create an
	// uncollectable, un-reconcilable debt against the shared "Walk-in Customer" ghost identity
	// (marketflow's own seeded per-tenant contact, phone "+000000000000" — NOT a legitimate AR
	// key; see orders.RequireIdentifiableCustomer's doc). Found live 2026-08-05: two true
	// walk-in orders (no name, no phone) got marked on_account via this path with a GL entry
	// treasury's PostSaleEditGL could never attribute to any customer balance — the debt was
	// posted but permanently un-collectable/un-reconcilable via Record Payment or the treasury
	// Customers page. Checked BEFORE any line/stock/GL mutation so a rejected increase never
	// partially applies.
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
	if crmContactID == "" {
		isStaffCredit := false
		var staffID uuid.UUID
		if sid, _ := order.Metadata["staff_member_id"].(string); sid != "" {
			if id, perr := uuid.Parse(sid); perr == nil {
				isStaffCredit, staffID = true, id
			}
		}
		if _, err := orders.RequireIdentifiableCustomer(customerName, customerIdentifier, isStaffCredit, staffID); err != nil {
			return fmt.Errorf("cannot add value to this sale: %w", err)
		}
	}

	var incrementalAmount, incrementalTax, incrementalCost float64
	consumptionItems := make([]inventory.ConsumptionItem, 0, len(diff.added)+len(diff.increased))

	// Resolve brand-new lines' tax the SAME way CreateOrder/AddOrderLines do (catalog tax code →
	// till-provided rate → the outlet's flat fallback VAT) instead of the narrower, client-rate-
	// only lineTaxAmount this used to call — that helper only ever saw a.TaxRate (whatever pos-ui
	// happened to send, nil for any item with no catalog tax_rate), so a fallback-VAT tenant's
	// added line was posted with ZERO tax from the moment it was created. See
	// orders.ResolveLineTaxes's own doc comment for the matching CreateOrder/AddOrderLines bug
	// this mirrors (confirmed live 2026-08-07).
	addedInputs := make([]orders.OrderLineInput, len(diff.added))
	for i, a := range diff.added {
		addedInputs[i] = orders.OrderLineInput{
			CatalogItemID: a.CatalogItemID, SKU: a.SKU, Name: a.Name,
			Quantity: a.Quantity, UnitPrice: a.UnitPrice,
			TaxCodeID: a.TaxCodeID, PriceIncludesTax: a.PriceIncludesTax, TaxRate: a.TaxRate,
		}
	}
	var addedTaxes []resolvedAddedLineTax
	if s.orderSvc != nil {
		// orders.ResolveLineTaxes returns an unexported element type (can't be named here), so
		// convert inline via field access — every field this copies is exported.
		raw := s.orderSvc.ResolveLineTaxes(ctx, tenantID, req.TenantSlug, addedInputs,
			s.orderSvc.OutletFallbackTaxRate(ctx, order.OutletID))
		addedTaxes = make([]resolvedAddedLineTax, len(raw))
		for i, t := range raw {
			addedTaxes[i] = resolvedAddedLineTax{
				codeID: t.CodeID, kraCode: t.KRACode, rate: t.Rate, amount: t.Amount,
				inclusive: t.Inclusive, hasInfo: t.HasInfo,
			}
		}
	}

	for i, a := range diff.added {
		total := round2(a.UnitPrice * a.Quantity)
		var lt resolvedAddedLineTax
		if i < len(addedTaxes) {
			lt = addedTaxes[i]
		} else {
			// s.orderSvc unwired (defensive only — always wired in production, see
			// orchestrator.go's SetOrderService): fall back to whatever rate the client sent
			// rather than fail the whole edit, matching pre-fix behavior exactly.
			if amt := lineTaxAmount(total, a.PriceIncludesTax, a.TaxRate); amt != nil {
				lt = resolvedAddedLineTax{rate: *a.TaxRate, amount: *amt, inclusive: a.PriceIncludesTax, hasInfo: true}
			}
		}
		inclusive := lt.inclusive || a.PriceIncludesTax
		create := s.client.POSOrderLine.Create().
			SetOrderID(order.ID).
			SetCatalogItemID(a.CatalogItemID).
			SetSku(a.SKU).
			SetName(a.Name).
			SetQuantity(a.Quantity).
			SetUnitPrice(a.UnitPrice).
			SetTotalPrice(total).
			SetPriceIncludesTax(inclusive).
			SetMetadata(map[string]any{"sale_edit_id": editID.String(), "edit_added": true})
		if lt.codeID != "" {
			create = create.SetTaxCodeID(lt.codeID)
		} else if a.TaxCodeID != "" {
			create = create.SetTaxCodeID(a.TaxCodeID)
		}
		if lt.kraCode != "" {
			create = create.SetTaxKraCode(lt.kraCode)
		}
		if lt.rate > 0 {
			create = create.SetTaxRate(lt.rate)
		} else if a.TaxRate != nil {
			create = create.SetTaxRate(*a.TaxRate)
		}
		if lt.amount > 0 {
			create = create.SetTaxAmount(lt.amount)
		}
		if _, err := create.Save(ctx); err != nil {
			return fmt.Errorf("create line: %w", err)
		}
		incrementalAmount += total
		if lt.hasInfo && !inclusive && lt.amount > 0 {
			incrementalTax += lt.amount
		}
		if a.SKU != "" {
			consumptionItems = append(consumptionItems, inventory.ConsumptionItem{SKU: a.SKU, Quantity: a.Quantity})
		}
	}

	for _, inc := range diff.increased {
		deltaTotal := round2(inc.Req.UnitPrice * inc.ByQty)
		newQty := round2(inc.Line.Quantity + inc.ByQty)
		newTotal := round2(inc.Line.TotalPrice + deltaTotal)
		upd := inc.Line.Update().SetQuantity(newQty).SetTotalPrice(newTotal)
		if inc.Line.TaxAmount != nil && !inc.Line.PriceIncludesTax && inc.Line.Quantity > 0 {
			// Prorate the line's own existing tax rate onto the incremental quantity, and — so a
			// LATER edit's own totals-recompute-from-lines still sees the right figure — persist
			// the bumped line's own TaxAmount too, not just this edit's incrementalTax.
			perUnitTax := *inc.Line.TaxAmount / inc.Line.Quantity
			deltaTax := round2(perUnitTax * inc.ByQty)
			incrementalTax += deltaTax
			upd = upd.SetTaxAmount(round2(*inc.Line.TaxAmount + deltaTax))
		}
		if _, err := upd.Save(ctx); err != nil {
			return fmt.Errorf("bump line %s: %w", inc.Line.ID, err)
		}
		incrementalAmount += deltaTotal
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

	// Both calls below are already best-effort by their own error handling (logged-and-continue,
	// documented as "non-fatal; can be reconciled") — the order's own totals/on_account metadata
	// are already committed above, so the Edit-Sale save response the cashier sees doesn't depend
	// on either succeeding. Dispatched OFF the request path (same detached-context/panic-recovery
	// idiom as payments.dispatchPostFinalize) so a slow/unreachable inventory-api or treasury-api
	// never delays the save — mirrors the 2026-08-07 latency fix already applied to the equally
	// best-effort ApplyCustomerCreditToDebt call on the main payment-confirm path.
	if s.inventoryClient != nil && len(consumptionItems) > 0 {
		s.dispatchEditIncreaseConsumption(tenantID, order.ID, editID, consumptionItems)
	}

	if s.treasuryClient != nil {
		// crmContactID/customerName/customerIdentifier already resolved (with the same request
		// overrides applied) by the identifiable-customer guard above — reuse instead of
		// re-querying.
		s.dispatchEditIncreaseGL(order.ID, editID, treasury.SaleEditGLRequest{
			ReferenceID: editID.String(), OrderID: order.ID.String(), OrderNumber: order.OrderNumber,
			OutletID: order.OutletID.String(), SellingScheme: "credit",
			Amount: incrementalAmount, TaxAmount: incrementalTax, CreditAmount: incrementalAmount, CostAmount: incrementalCost,
			Currency: order.Currency, CrmContactID: crmContactID, CustomerIdentifier: customerIdentifier, CustomerName: customerName,
			UserID: req.RequestedBy.String(), Description: "Edit Sale increase — " + order.OrderNumber,
		}, req.TenantSlug)
	}

	return nil
}

// dispatchEditIncreaseConsumption records the incremental inventory consumption for an Edit-Sale
// increase off the request path — see the comment at its call site in applyInPlaceIncrease.
func (s *Service) dispatchEditIncreaseConsumption(tenantID, orderID, editID uuid.UUID, items []inventory.ConsumptionItem) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("edit-increase consumption dispatch panic recovered",
					zap.String("order_id", orderID.String()), zap.String("edit_id", editID.String()), zap.Any("panic", r))
			}
		}()
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := s.inventoryClient.RecordConsumption(dctx, tenantID.String(), inventory.ConsumptionRequest{
			OrderID: orderID.String(), Items: items,
			Reason: "sale_edit_increase", IdempotencyKey: "pos-edit-increase-" + editID.String(),
		}); err != nil {
			s.log.Warn("edit increase: inventory consumption failed (non-fatal; can be reconciled)",
				zap.String("order_id", orderID.String()), zap.String("edit_id", editID.String()), zap.Error(err))
		}
	}()
}

// dispatchEditIncreaseGL posts the incremental GL entry for an Edit-Sale increase off the
// request path — see the comment at its call site in applyInPlaceIncrease.
func (s *Service) dispatchEditIncreaseGL(orderID, editID uuid.UUID, req treasury.SaleEditGLRequest, tenantSlug string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("edit-increase GL dispatch panic recovered",
					zap.String("order_id", orderID.String()), zap.String("edit_id", editID.String()), zap.Any("panic", r))
			}
		}()
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := s.treasuryClient.PostSaleEditGL(dctx, tenantSlug, req); err != nil {
			s.log.Warn("edit increase: treasury GL post failed (non-fatal; can be reconciled)",
				zap.String("order_id", orderID.String()), zap.String("edit_id", editID.String()), zap.Error(err))
		}
	}()
}

// resolvedAddedLineTax is a package-local carrier for orders.Service.ResolveLineTaxes' per-line
// result — that function's element type is unexported (orders.resolvedLineTax), so it can only
// be consumed via field access at the call site, never named directly in this package; this type
// just gives applyInPlaceIncrease's added-lines loop something concrete to declare a slice of.
type resolvedAddedLineTax struct {
	codeID    string
	kraCode   string
	rate      float64
	amount    float64
	inclusive bool
	hasInfo   bool
}

// lineTaxAmount computes a line's tax amount from its rate, mirroring
// orders.Service.EditOrderLine's own formula (rate× total, or the inclusive-price backout).
// Used ONLY as applyInPlaceIncrease's defensive fallback for the (never-expected-in-production)
// case where orderSvc isn't wired — orders.ResolveLineTaxes (catalog code → till rate → outlet
// fallback VAT) is the real, centralized resolution every other path uses.
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
