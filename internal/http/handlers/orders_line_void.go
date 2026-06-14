package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
)

type voidLineInput struct {
	Qty           float64 `json:"qty"`
	Reason        string  `json:"reason"`
	ApprovalToken string  `json:"approval_token"`
}

// VoidOrderLine handles POST /{tenantID}/pos/orders/{orderID}/lines/{lineID}/void.
// Removing a line from an already-sent/persisted order soft-voids it (kept for
// audit, never hard-deleted) and reduces the order total. This is the
// anti-sweethearting control: pre-send cart edits are client-only, but once a
// line exists server-side, removing it is recorded — optionally gated by a
// manager-PIN step-up approval token.
func (h *POSOrderHandler) VoidOrderLine(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order id", http.StatusBadRequest)
		return
	}
	lineID, err := uuid.Parse(chi.URLParam(r, "lineID"))
	if err != nil {
		jsonError(w, "invalid line id", http.StatusBadRequest)
		return
	}
	var input voidLineInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Reason == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	var approverID *uuid.UUID
	if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
		if aid, valid := verifyApprovalToken(input.ApprovalToken, "order.line_remove", h.terminalSecret); valid {
			approverID = &aid
		} else {
			jsonError(w, "invalid or expired approval", http.StatusForbidden)
			return
		}
	}

	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	if order.Status == "voided" || order.Status == "completed" {
		jsonError(w, "cannot modify a "+order.Status+" order", http.StatusBadRequest)
		return
	}

	line, err := h.client.POSOrderLine.Query().
		Where(posorderline.ID(lineID), posorderline.OrderID(orderID)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "line not found", http.StatusNotFound)
		return
	}
	if line.VoidedQty != nil {
		jsonError(w, "line already voided", http.StatusBadRequest)
		return
	}

	// Default to voiding the whole line; cap a partial qty at the line quantity.
	voidQty := input.Qty
	if voidQty <= 0 || voidQty > line.Quantity {
		voidQty = line.Quantity
	}
	unit := 0.0
	if line.Quantity > 0 {
		unit = line.TotalPrice / line.Quantity
	}
	voidedValue := unit * voidQty

	now := time.Now()
	if _, err := line.Update().
		SetVoidedQty(voidQty).
		SetVoidedReason(input.Reason).
		SetVoidedBy(callerID).
		SetVoidedAt(now).
		Save(r.Context()); err != nil {
		h.log.Error("void line failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Reduce the order headline totals by the voided value (floor at zero).
	newSubtotal := order.Subtotal - voidedValue
	if newSubtotal < 0 {
		newSubtotal = 0
	}
	newTotal := order.TotalAmount - voidedValue
	if newTotal < 0 {
		newTotal = 0
	}
	updatedOrder, err := order.Update().
		SetSubtotal(newSubtotal).
		SetTotalAmount(newTotal).
		Save(r.Context())
	if err != nil {
		h.log.Error("adjust order totals after line void failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	action := "order.line_remove"
	if voidQty < line.Quantity {
		action = "order.line_qty_decrease"
	}
	if h.auditSvc != nil {
		oid := order.OutletID
		amt := voidedValue
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: callerID,
			ApproverID:  approverID,
			Action:      action,
			EntityType:  "pos_order_line",
			EntityID:    lineID.String(),
			Reason:      input.Reason,
			Amount:      &amt,
			Before:      map[string]any{"sku": line.Sku, "name": line.Name, "quantity": line.Quantity},
			After:       map[string]any{"voided_qty": voidQty},
		})
	}

	jsonOK(w, map[string]any{"order": updatedOrder, "voided_qty": voidQty})
}
