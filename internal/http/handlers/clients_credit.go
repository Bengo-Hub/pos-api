package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// Credit terms proxy (QA req 1): treasury owns the customer AR balance + credit limit /
// payment period; pos-ui manages them from the client detail page. pos-api proxies over S2S
// because the INTERNAL_SERVICE_KEY must never reach the browser. The treasury key is the
// customer's crm_contact_id (canonical AR key), falling back to the phone identifier —
// the SAME resolution credit sales use, so the terms govern exactly the row they debit.

// SetTreasuryClient wires the treasury S2S client used by the credit-terms proxy.
func (h *ClientHandler) SetTreasuryClient(tc *treasury.Client) {
	h.treasury = tc
}

// creditKeyForAccount resolves the treasury customer key for a loyalty account:
// crm_contact_id when linked, else the phone identifier.
func (h *ClientHandler) creditKeyForAccount(r *http.Request, tid uuid.UUID) (key, name string, ok bool) {
	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		return "", "", false
	}
	acc, err := h.db.LoyaltyAccount.Query().
		Where(entla.ID(accountID), entla.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		return "", "", false
	}
	if acc.CrmContactID != nil {
		return acc.CrmContactID.String(), acc.CustomerName, true
	}
	return acc.CustomerPhone, acc.CustomerName, true
}

// GetCredit handles GET /{tenantID}/pos/clients/{accountID}/credit — the customer's AR
// balance due + configured credit limit / payment period, proxied from treasury.
func (h *ClientHandler) GetCredit(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	if h.treasury == nil {
		jsonError(w, "treasury client not configured", http.StatusServiceUnavailable)
		return
	}
	key, _, ok := h.creditKeyForAccount(r, tid)
	if !ok {
		jsonError(w, "customer account not found", http.StatusNotFound)
		return
	}
	tenantSlug := chi.URLParam(r, "tenantID")
	terms, err := h.treasury.GetCreditTerms(r.Context(), tenantSlug, key)
	if err != nil {
		h.log.Error("get credit terms proxy failed", zap.Error(err))
		jsonError(w, "failed to load credit terms", http.StatusBadGateway)
		return
	}
	jsonOK(w, terms)
}

// NOTE: credit terms are EDITED on the central treasury Customers page (treasury-ui →
// PATCH /ar/customers/{id}/credit-terms). The old PUT /clients/{accountID}/credit editor
// proxy was removed with the duplicate POS clients pages — this file is read-only now.
