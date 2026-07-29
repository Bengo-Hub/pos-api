package handlers

import (
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/saleedit"
)

// SaleEditHandler exposes the tenant-admin Edit-Sale "prepare" step — reverses a finalized sale
// so pos-ui's existing Add Sale pipeline can create the replacement. Gated by
// pos.orders.edit_finalized (admin by default, tenant-configurable).
type SaleEditHandler struct {
	log *zap.Logger
	svc *saleedit.Service
}

func NewSaleEditHandler(log *zap.Logger, svc *saleedit.Service) *SaleEditHandler {
	return &SaleEditHandler{log: log, svc: svc}
}

type prepareEditInput struct {
	Reason string `json:"reason"`
}

// PrepareEdit handles POST /{tenantID}/pos/orders/{orderID}/prepare-edit.
func (h *SaleEditHandler) PrepareEdit(w http.ResponseWriter, r *http.Request) {
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
	var input prepareEditInput
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

	result, err := h.svc.PrepareEdit(r.Context(), tid, saleedit.Request{
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
