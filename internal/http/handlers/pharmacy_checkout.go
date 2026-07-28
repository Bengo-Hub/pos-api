package handlers

import (
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	entvisit "github.com/bengobox/pos-service/internal/ent/patientvisit"
	entpxl "github.com/bengobox/pos-service/internal/ent/prescriptionline"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// checkoutPrescriptionInput is the OPTIONAL body for CheckoutPrescription — a manual/coded
// discount to apply at checkout. Mirrors createOrderInput's discount fields so pharmacy sales
// carry discounts the same way every other order-creation path does (auto-apply promotions
// already apply regardless, since that evaluation lives inside CreateOrder itself).
type checkoutPrescriptionInput struct {
	DiscountAmount float64 `json:"discount_amount,omitempty"`
	DiscountReason string  `json:"discount_reason,omitempty"`
}

// CheckoutPrescription handles POST /{tenantID}/pos/pharmacy/prescriptions/{prescriptionID}/checkout
//
// Creates the payable POSOrder for a DISPENSED prescription's drug lines — the "order
// aggregation"/money side of the pharmacy workflow (previously entirely missing: Dispense only
// ever flipped clinical/stock state, with no path into a cart or payment). Checkout is a step
// AFTER Dispense (not before) so it reuses the SAME reservation-consume-on-dispense stock
// commitment Phase 3/5 already ship, instead of re-deducting stock a second time at payment —
// each resulting order line is tagged so payments/service.go's pos.sale.finalized publisher marks
// it skip_inventory, and inventory-api's consumer honors that to avoid a double deduction.
func (h *PharmacyHandler) CheckoutPrescription(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	pxID, err := uuid.Parse(chi.URLParam(r, "prescriptionID"))
	if err != nil {
		jsonError(w, "invalid prescription_id", http.StatusBadRequest)
		return
	}
	if h.orderSvc == nil {
		jsonError(w, "checkout is not available", http.StatusServiceUnavailable)
		return
	}

	px, err := h.db.Prescription.Get(r.Context(), pxID)
	if err != nil || px.TenantID != tid {
		jsonError(w, "prescription not found", http.StatusNotFound)
		return
	}
	// Two valid orders of operation, both landing on the same order+stock model:
	//   direct mode  — dispense first (stock consumed via the reservation), then take payment.
	//   billing mode — a cashier picks the approved script off the Bills queue, takes payment,
	//                  and dispenses on handover.
	// Either way stock moves exactly once, at Dispense (ConsumeReservation), and the resulting
	// order is always tagged skip_inventory so payment never deducts a second time.
	switch px.Status {
	case "approved", "locked", "dispensed":
	default:
		jsonError(w, "prescription must be approved before checkout", http.StatusConflict)
		return
	}
	if px.OrderID != nil {
		jsonError(w, "prescription already has an order", http.StatusConflict)
		return
	}

	// Pending lines are included so a pre-dispense (billing-mode) checkout bills the prescribed
	// quantity; the per-line qty fallback below already prefers dispensed-qty when it exists.
	lines, err := h.db.PrescriptionLine.Query().
		Where(entpxl.PrescriptionID(pxID), entpxl.StatusIn("pending", "dispensed")).
		All(r.Context())
	if err != nil || len(lines) == 0 {
		jsonError(w, "no drug lines to check out", http.StatusUnprocessableEntity)
		return
	}

	// Resolve each line's SKU from the tenant's inventory catalog — PrescriptionLine stores
	// catalog_item_id only (no sku column), so this reuses the SAME S2S proxy fetch every other
	// report/profitability handler already uses rather than adding a new field/migration.
	tenantSlug := resolveTenantSlug(r, h.db)
	items, _ := fetchInventoryItems(r.Context(), tenantSlug, "", nil)
	byItemID := make(map[string]inventoryProxyItem, len(items))
	for _, it := range items {
		byItemID[it.ID] = it
	}

	orderLines := make([]orders.OrderLineInput, 0, len(lines))
	for _, l := range lines {
		qty := float64(l.QuantityDispensed)
		if qty <= 0 {
			qty = float64(l.QuantityPrescribed)
		}
		unitPrice := 0.0
		if l.UnitPrice != nil {
			unitPrice, _ = l.UnitPrice.Float64()
		}
		var catalogItemID uuid.UUID
		sku := ""
		category := ""
		if l.CatalogItemID != nil {
			catalogItemID = *l.CatalogItemID
			if it, ok := byItemID[l.CatalogItemID.String()]; ok {
				sku = it.SKU
				category = it.CategoryName
			}
		}
		orderLines = append(orderLines, orders.OrderLineInput{
			CatalogItemID: catalogItemID,
			SKU:           sku,
			Name:          l.DrugName,
			Category:      category,
			Quantity:      qty,
			UnitPrice:     unitPrice,
			TotalPrice:    unitPrice * qty,
			Metadata:      map[string]any{"prescription_id": px.ID.String(), "prescription_line_id": l.ID.String()},
		})
	}

	var userID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims.Subject != "" {
		userID, _ = uuid.Parse(claims.Subject)
	}

	// Body is optional — a plain checkout (no discount) still works with no request body, as before.
	var input checkoutPrescriptionInput
	_ = json.NewDecoder(r.Body).Decode(&input)

	metadata := map[string]any{
		// Marks every line on this order as already stock-committed (Dispense's
		// ConsumeReservation call) — see payments/service.go's skip_inventory tagging.
		"prescription_id": px.ID.String(),
	}
	if input.DiscountAmount > 0 && input.DiscountReason != "" {
		metadata["discount_reason"] = input.DiscountReason
	}

	order, err := h.orderSvc.CreateOrder(r.Context(), orders.CreateOrderRequest{
		TenantID:       tid,
		TenantSlug:     tenantSlug,
		OutletID:       px.OutletID,
		UserID:         userID,
		Lines:          orderLines,
		OrderSubtype:   "retail",
		Source:         "pos_terminal",
		CustomerName:   px.PatientName,
		DiscountAmount: input.DiscountAmount,
		Metadata:       metadata,
	})
	if err != nil {
		h.log.Error("prescription checkout: create order failed", zap.Error(err))
		jsonError(w, "failed to create checkout order", http.StatusInternalServerError)
		return
	}

	if _, err := h.db.Prescription.UpdateOneID(pxID).SetOrderID(order.ID).Save(r.Context()); err != nil {
		h.log.Warn("prescription checkout: failed to link order_id", zap.Error(err))
	}

	// OPD-originated prescription: checkout is the final step of the clinical journey (payment
	// itself is tracked on the order/payment records, not re-tracked on the visit).
	if px.VisitID != nil {
		if _, err := h.db.PatientVisit.UpdateOneID(*px.VisitID).SetStatus(entvisit.StatusCompleted).Save(r.Context()); err != nil {
			h.log.Warn("failed to complete visit after checkout", zap.Error(err))
		}
	}

	jsonOK(w, map[string]any{
		"order_id":     order.ID,
		"order_number": order.OrderNumber,
		"total_amount": order.TotalAmount,
	})
}
