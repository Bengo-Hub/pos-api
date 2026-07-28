package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entlaborder "github.com/bengobox/pos-service/internal/ent/laborder"
	entlabline "github.com/bengobox/pos-service/internal/ent/laborderline"
	entorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entpx "github.com/bengobox/pos-service/internal/ent/prescription"
	entpxl "github.com/bengobox/pos-service/internal/ent/prescriptionline"
)

// ListBills handles GET /{tenantID}/pos/pharmacy/bills
//
// The cashier-facing queue for the "billing" pharmacy workflow mode: every prescription a
// prescriber has approved (and, if the outlet requires it, locked) but that hasn't been paid for
// yet — regardless of WHICH pharmacist wrote it. Mirrors the hospitality split where a waiter
// posts an order and any cashier settles it. In "direct" mode this queue is simply unused: the
// same person who writes the script also takes payment straight from the prescription page.
func (h *ClinicalHandler) ListBills(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.Prescription.Query().Where(
		entpx.TenantID(tid),
		entpx.StatusIn("approved", "locked", "dispensed"),
	)
	if outletID := r.URL.Query().Get("outlet_id"); outletID != "" {
		if oid, perr := uuid.Parse(outletID); perr == nil {
			q = q.Where(entpx.OutletID(oid))
		}
	}
	rows, err := q.Order(ent.Asc(entpx.FieldCreatedAt)).Limit(200).All(r.Context())
	if err != nil {
		jsonError(w, "failed to list bills", http.StatusInternalServerError)
		return
	}

	// Resolve every already-linked order in ONE query (not N+1) so we can drop the ones already
	// settled — a paid prescription is off the cashier's queue.
	orderIDs := make([]uuid.UUID, 0, len(rows))
	for _, p := range rows {
		if p.OrderID != nil {
			orderIDs = append(orderIDs, *p.OrderID)
		}
	}
	paidByOrder := map[uuid.UUID]bool{}
	amountByOrder := map[uuid.UUID]float64{}
	if len(orderIDs) > 0 {
		linked, _ := h.db.POSOrder.Query().Where(entorder.IDIn(orderIDs...)).All(r.Context())
		for _, o := range linked {
			paidByOrder[o.ID] = o.PaidTotal >= o.TotalAmount && o.TotalAmount > 0
			amountByOrder[o.ID] = o.TotalAmount
		}
	}

	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		if p.OrderID != nil && paidByOrder[*p.OrderID] {
			continue // already settled — not a pending bill
		}
		lines, _ := h.db.PrescriptionLine.Query().Where(entpxl.PrescriptionID(p.ID)).All(r.Context())
		// Estimated total from the prescribed lines; the authoritative figure is the order's
		// total once checkout has created it (tax/rounding applied there, never recomputed here).
		estimated := 0.0
		for _, l := range lines {
			qty := float64(l.QuantityDispensed)
			if qty <= 0 {
				qty = float64(l.QuantityPrescribed)
			}
			if l.UnitPrice != nil {
				up, _ := l.UnitPrice.Float64()
				estimated += up * qty
			}
		}
		entry := map[string]any{
			"prescription":    p,
			"lines":           lines,
			"line_count":      len(lines),
			"estimated_total": estimated,
		}
		if p.OrderID != nil {
			entry["order_id"] = *p.OrderID
			entry["order_total"] = amountByOrder[*p.OrderID]
		}
		out = append(out, entry)
	}
	jsonOK(w, map[string]any{"data": out})
}

// ActivateLabOrderIfPaid handles POST /{tenantID}/pos/clinical/lab-orders/{labOrderID}/activate
//
// Flips an awaiting_payment lab order to "ordered" (making it visible to the Lab module) once its
// bill is settled. Safe to call repeatedly — it re-checks the linked order's paid_total each time
// rather than trusting the caller, so a cashier hitting it early gets a clear 402 instead of
// silently activating unpaid work.
func (h *ClinicalHandler) ActivateLabOrderIfPaid(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	loID, err := uuid.Parse(chi.URLParam(r, "labOrderID"))
	if err != nil {
		jsonError(w, "invalid lab_order_id", http.StatusBadRequest)
		return
	}
	order, err := h.db.LabOrder.Query().Where(entlaborder.ID(loID), entlaborder.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "lab order not found", http.StatusNotFound)
		return
	}
	if order.Status != entlaborder.StatusAwaitingPayment {
		jsonOK(w, map[string]any{"lab_order": order, "already_active": true})
		return
	}
	if order.PaymentOrderID == nil {
		jsonError(w, "lab order has no bill to settle", http.StatusConflict)
		return
	}
	bill, err := h.db.POSOrder.Query().Where(entorder.ID(*order.PaymentOrderID)).Only(r.Context())
	if err != nil {
		jsonError(w, "lab bill not found", http.StatusNotFound)
		return
	}
	if bill.PaidTotal < bill.TotalAmount {
		jsonError(w, "lab tests must be paid for before they can be run", http.StatusPaymentRequired)
		return
	}

	updated, err := h.db.LabOrder.UpdateOneID(loID).SetStatus(entlaborder.StatusOrdered).Save(r.Context())
	if err != nil {
		h.log.Error("activate lab order failed", zap.Error(err))
		jsonError(w, "failed to activate lab order", http.StatusInternalServerError)
		return
	}
	lines, _ := h.db.LabOrderLine.Query().Where(entlabline.LabOrderID(loID)).All(r.Context())
	jsonOK(w, map[string]any{"lab_order": updated, "lines": lines})
}
