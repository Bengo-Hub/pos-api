package handlers

import (
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/saledelete"
)

// SaleDeleteHandler exposes the tenant-admin Delete-Sale ("shred") tool. Unlike the
// platform-owner-only /reversals routes, this is gated purely by the pos.orders.delete
// permission — admin by default, tenant-configurable via the Roles & Permissions matrix.
type SaleDeleteHandler struct {
	log *zap.Logger
	svc *saledelete.Service
}

func NewSaleDeleteHandler(log *zap.Logger, svc *saledelete.Service) *SaleDeleteHandler {
	return &SaleDeleteHandler{log: log, svc: svc}
}

type deleteSaleInput struct {
	Reason string `json:"reason"`
}

// DeleteSale handles DELETE /{tenantID}/pos/orders/{orderID} — see saledelete.Service.Delete for
// the fiscalised-vs-hard-delete branching.
func (h *SaleDeleteHandler) DeleteSale(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order ID", http.StatusBadRequest)
		return
	}
	var input deleteSaleInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil && err.Error() != "EOF" {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	requestedBy, _ := uuid.Parse(claims.Subject)

	result, err := h.svc.Delete(r.Context(), tid, saledelete.Request{
		OrderID:     orderID,
		Reason:      input.Reason,
		RequestedBy: requestedBy,
		TenantSlug:  chi.URLParam(r, "tenantID"),
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}
