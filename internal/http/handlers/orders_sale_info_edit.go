package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// updateSaleInfoInput is the body for PATCH /{tenantID}/pos/orders/{orderID}/sale-info.
// Pointers distinguish "field not sent" from "clear to empty string".
type updateSaleInfoInput struct {
	ServedByUserID *string `json:"served_by_user_id"`
	CustomerName   *string `json:"customer_name"`
	CustomerPhone  *string `json:"customer_phone"`
	Reason         string  `json:"reason"`
}

// UpdateSaleInfo handles PATCH /{tenantID}/pos/orders/{orderID}/sale-info — an admin/manager
// (route-gated to pos.orders.manage) correction tool for WHO served a sale and the customer on
// file, on a draft, an open bill, OR a completed sale (never on a voided/cancelled/refunded one —
// see orders.Service.UpdateSaleInfo). Deliberately narrow: never touches totals, line items,
// discounts, tax, or payments — those stay immutable once completed (EditOrderLine/
// SetOrderDiscount's status guards), since they're keyed to eTIMS fiscal signing + GL postings.
func (h *POSOrderHandler) UpdateSaleInfo(w http.ResponseWriter, r *http.Request) {
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
	var input updateSaleInfoInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.Reason) == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	svcInput := orders.UpdateSaleInfoInput{Reason: input.Reason}
	if input.ServedByUserID != nil {
		if id, perr := uuid.Parse(strings.TrimSpace(*input.ServedByUserID)); perr == nil {
			svcInput.ServedByUserID = &id
		} else {
			jsonError(w, "served_by_user_id must be a valid user id", http.StatusBadRequest)
			return
		}
	}
	svcInput.CustomerName = input.CustomerName
	svcInput.CustomerPhone = input.CustomerPhone

	result, err := h.orderSvc.UpdateSaleInfo(r.Context(), tid, orderID, svcInput)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if h.auditSvc != nil {
		oid := result.Order.OutletID
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: callerID,
			Action:      "order.sale_info_updated",
			EntityType:  "pos_order",
			EntityID:    orderID.String(),
			Reason:      input.Reason,
			Before:      result.Before,
			After:       result.After,
		})
	}

	jsonOK(w, map[string]any{"order": result.Order})
}
