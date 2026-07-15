package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
)

type setOrderDiscountInput struct {
	DiscountAmount *float64 `json:"discount_amount"`
	Reason         string   `json:"reason"`
}

// SetOrderDiscount handles PATCH /{tenantID}/pos/orders/{orderID}/discount.
//
// Manager/admin corrective tool: sets an UNSETTLED order's order-level discount in place
// and recomputes the headline totals, so a resumed sale's discount is persisted on the
// SAME order before payment — no replacement order, no settling at a stale total. Writes
// a full before/after entry to the central audit trail. The route is gated on
// pos.orders.manage (router.go), mirroring EditOrderLine.
func (h *POSOrderHandler) SetOrderDiscount(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}
	var input setOrderDiscountInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Reason == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}
	if input.DiscountAmount == nil {
		jsonError(w, "discount_amount is required", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	result, err := h.orderSvc.SetOrderDiscount(r.Context(), tid, orderID, *input.DiscountAmount)
	if err != nil {
		h.log.Warn("set order discount failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if h.auditSvc != nil {
		oid := result.Order.OutletID
		amt := result.Order.TotalAmount - result.BeforeTotal
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: callerID,
			Action:      "order.discount_edit",
			EntityType:  "pos_order",
			EntityID:    orderID.String(),
			Reason:      input.Reason,
			Amount:      &amt,
			Before: map[string]any{
				"discount_total": result.BeforeDiscount, "total_amount": result.BeforeTotal,
			},
			After: map[string]any{
				"discount_total": result.Order.DiscountTotal, "total_amount": result.Order.TotalAmount,
			},
		})
	}

	jsonOK(w, map[string]any{"order": result.Order})
}
