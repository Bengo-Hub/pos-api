package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
)

// dateMoveAdminRoles gates MoveOrderDate to the tenant's own admin/owner tier — deliberately
// narrower than adminOverrideRoles/hasOverrideRole (both of which also grant "manager"/
// "store_manager"), because moving a sale's reporting date is a financial-record-integrity
// action the requester explicitly scoped to "admins/platform owner admins", one notch above
// the manager-level authority pos.orders.manage already grants for line-price edits/voids.
var dateMoveAdminRoles = map[string]bool{
	"admin": true, "owner": true, "pos_admin": true, "super_admin": true, "superuser": true,
}

// canMoveOrderDate reports whether the caller is a platform owner or holds one of
// dateMoveAdminRoles — checked against both the pos_role request context (SSO/PIN
// session role) and the JWT role claims, mirroring canManageRBAC's fallback so SSO
// admins without a local pos_role are still recognized.
func canMoveOrderDate(r *http.Request) bool {
	if dateMoveAdminRoles[strings.ToLower(requesterRole(r))] {
		return true
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil {
		return false
	}
	if claims.IsPlatformOwner {
		return true
	}
	for _, role := range claims.Roles {
		if dateMoveAdminRoles[strings.ToLower(role)] {
			return true
		}
	}
	return false
}

type moveOrderDateInput struct {
	NewDate string `json:"new_date"` // "2026-07-09" (YYYY-MM-DD)
	Reason  string `json:"reason"`
}

// MoveOrderDate handles PATCH /{tenantID}/pos/orders/{orderID}/date — admin/platform-owner
// tool to correct which calendar day a settled sale counts toward in reports (e.g. a sale
// rung up and synced a day late because a missing recipe blocked checkout at the time, then
// added and settled the next day). Sets business_date without touching the immutable
// created_at audit timestamp, amounts, payments, or stock. See orders.Service.MoveOrderDate.
func (h *POSOrderHandler) MoveOrderDate(w http.ResponseWriter, r *http.Request) {
	if !canMoveOrderDate(r) {
		jsonError(w, "only admins/platform owners may move a sale's date", http.StatusForbidden)
		return
	}
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
	var input moveOrderDateInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Reason == "" || input.NewDate == "" {
		jsonError(w, "new_date and reason are required", http.StatusBadRequest)
		return
	}
	newDate, err := time.Parse("2006-01-02", input.NewDate)
	if err != nil {
		jsonError(w, "new_date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	result, err := h.orderSvc.MoveOrderDate(r.Context(), tid, orderID, callerID, newDate, input.Reason)
	if err != nil {
		h.log.Warn("move order date failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if h.auditSvc != nil {
		oid := result.Order.OutletID
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: callerID,
			Action:      "order.date_moved",
			EntityType:  "pos_order",
			EntityID:    orderID.String(),
			Reason:      input.Reason,
			Before:      map[string]any{"effective_date": result.BeforeDate.Format("2006-01-02")},
			After:       map[string]any{"effective_date": result.AfterDate.Format("2006-01-02")},
		})
	}

	jsonOK(w, map[string]any{"order": result.Order})
}
