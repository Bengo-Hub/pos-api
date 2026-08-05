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

// SaleEditHandler exposes the tenant-admin Edit-Sale tool. Gated by
// pos.orders.edit_finalized (admin by default, tenant-configurable).
type SaleEditHandler struct {
	log *zap.Logger
	svc *saleedit.Service
}

func NewSaleEditHandler(log *zap.Logger, svc *saleedit.Service) *SaleEditHandler {
	return &SaleEditHandler{log: log, svc: svc}
}

type editSaleLineInput struct {
	LineID           *uuid.UUID `json:"line_id,omitempty"` // nil = new line
	CatalogItemID    uuid.UUID  `json:"catalog_item_id,omitempty"`
	SKU              string     `json:"sku"`
	Name             string     `json:"name"`
	Quantity         float64    `json:"quantity"`    // requested FINAL quantity for this line
	UnitPrice        float64    `json:"unit_price"`  // requested FINAL unit price
	TaxCodeID        string     `json:"tax_code_id,omitempty"`
	PriceIncludesTax bool       `json:"price_includes_tax,omitempty"`
	TaxRate          *float64   `json:"tax_rate,omitempty"`
}

type editSaleInput struct {
	Reason             string              `json:"reason"`
	Lines              []editSaleLineInput `json:"lines"`
	CrmContactID       *uuid.UUID          `json:"crm_contact_id,omitempty"`
	CustomerIdentifier string              `json:"customer_identifier,omitempty"`
	CustomerName       string              `json:"customer_name,omitempty"`
}

// Edit handles POST /{tenantID}/pos/orders/{orderID}/edit — the single centralized entry
// point for editing a finalized sale. The caller sends the FULL desired line set; the
// orchestrator diffs it against the live order server-side (closing the class of bug where a
// stale client-side snapshot silently resubmitted a wrong/no-op edit) and routes each part of
// the diff to the correct sub-flow based on the order's actual fiscalization status.
func (h *SaleEditHandler) Edit(w http.ResponseWriter, r *http.Request) {
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
	var input editSaleInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	requestedBy, _ := uuid.Parse(claims.Subject)

	lines := make([]saleedit.EditLine, 0, len(input.Lines))
	for _, l := range input.Lines {
		lines = append(lines, saleedit.EditLine{
			LineID: l.LineID, CatalogItemID: l.CatalogItemID, SKU: l.SKU, Name: l.Name,
			Quantity: l.Quantity, UnitPrice: l.UnitPrice, TaxCodeID: l.TaxCodeID,
			PriceIncludesTax: l.PriceIncludesTax, TaxRate: l.TaxRate,
		})
	}

	result, err := h.svc.Edit(r.Context(), tid, saleedit.EditSaleRequest{
		OrderID: orderID, Reason: input.Reason, RequestedBy: requestedBy,
		TenantSlug: chi.URLParam(r, "tenantID"), Lines: lines,
		CrmContactID: input.CrmContactID, CustomerIdentifier: input.CustomerIdentifier, CustomerName: input.CustomerName,
	})
	if err != nil {
		status := http.StatusUnprocessableEntity
		if result != nil {
			// A partial failure (one sub-step succeeded, the other failed) still returns the
			// result the caller needs (e.g. a linked reversal/return id) alongside the error —
			// 207-style, but this codebase's jsonError only carries a message, so surface both.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(struct {
				Error string `json:"error"`
				saleedit.EditSaleResult
			}{Error: err.Error(), EditSaleResult: *result})
			return
		}
		jsonError(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}
