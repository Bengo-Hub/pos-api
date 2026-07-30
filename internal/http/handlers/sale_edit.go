package handlers

import (
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/reversals"
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

type editReduceLineInput struct {
	LineID   uuid.UUID `json:"line_id"`
	Quantity float64   `json:"quantity,omitempty"` // 0 => whole line
}

type editReduceInput struct {
	Reason string                `json:"reason"`
	Lines  []editReduceLineInput `json:"lines"`
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

// ApplyEdit handles POST /{tenantID}/pos/orders/{orderID}/edit-reduce — the true in-place half
// of Edit Sale: removes/reduces the given lines via a partial reversal (inventory/GL/eTIMS/
// loyalty/commission all adjusted proportionally, order stays exactly as it was). Any added
// value (new item, or raising a line's qty/price) is posted separately by pos-ui as a linked
// addendum sale through the normal create-order endpoint — this route is a no-op-safe call with
// an empty `lines` array for a pure-increase edit.
func (h *SaleEditHandler) ApplyEdit(w http.ResponseWriter, r *http.Request) {
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
	var input editReduceInput
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

	reductions := make([]reversals.LineSelection, 0, len(input.Lines))
	for _, l := range input.Lines {
		reductions = append(reductions, reversals.LineSelection{LineID: l.LineID, Quantity: l.Quantity})
	}

	result, err := h.svc.ApplyEdit(r.Context(), tid, saleedit.EditRequest{
		OrderID:     orderID,
		Reason:      input.Reason,
		RequestedBy: requestedBy,
		TenantSlug:  chi.URLParam(r, "tenantID"),
		Reductions:  reductions,
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}
