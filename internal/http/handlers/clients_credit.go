package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/customerbalancecache"
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
		// Self-healing fallback (Phase D): the live S2S call is ALWAYS tried first — treasury
		// remains the single source of truth — but when it fails, fall back to the
		// CustomerBalanceCache kept fresh by the treasury.customer.balance_updated event
		// consumer, instead of hard-failing the terminal's credit check.
		if cached, cerr := h.creditFromCache(r.Context(), tid, key); cerr == nil && cached != nil {
			h.log.Warn("get credit terms proxy failed — serving cached balance", zap.Error(err))
			jsonOK(w, cached)
			return
		}
		h.log.Error("get credit terms proxy failed", zap.Error(err))
		jsonError(w, "failed to load credit terms", http.StatusBadGateway)
		return
	}
	jsonOK(w, terms)
}

// creditFromCache resolves a CustomerBalanceCache row by crm_contact_id (when key parses as a
// UUID) or customer_identifier, shaped as treasury.CreditTermsResponse so the response envelope
// matches the live path exactly. Returns (nil, nil) when no cached row exists yet (nothing to
// fall back to).
func (h *ClientHandler) creditFromCache(ctx context.Context, tenantID uuid.UUID, key string) (*treasury.CreditTermsResponse, error) {
	q := h.db.CustomerBalanceCache.Query().Where(customerbalancecache.TenantID(tenantID))
	if cid, err := uuid.Parse(key); err == nil {
		q = q.Where(customerbalancecache.CrmContactID(cid))
	} else {
		q = q.Where(customerbalancecache.CustomerIdentifier(key))
	}
	row, err := q.First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &treasury.CreditTermsResponse{
		CrmContactID: uuidOrEmpty(row.CrmContactID),
		CustomerName: row.CustomerName,
		BalanceDue:   row.BalanceDue,
		Currency:     row.Currency,
	}, nil
}

func uuidOrEmpty(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// NOTE: credit terms are EDITED on the central treasury Customers page (treasury-ui →
// PATCH /ar/customers/{id}/credit-terms). The old PUT /clients/{accountID}/credit editor
// proxy was removed with the duplicate POS clients pages — this file is read-only for terms.

// PayoutCredit handles POST /{tenantID}/pos/clients/{accountID}/payout-credit — pays out some/all
// of a customer's EXISTING stored credit (a negative balance_due) via a real channel, independent
// of any return/sale. Proxied over S2S for the same reason GetCredit is: the INTERNAL_SERVICE_KEY
// must never reach the browser.
func (h *ClientHandler) PayoutCredit(w http.ResponseWriter, r *http.Request) {
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
	var body treasury.PayoutCreditRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	tenantSlug := chi.URLParam(r, "tenantID")
	resp, err := h.treasury.PayoutCustomerCredit(r.Context(), tenantSlug, key, body)
	if err != nil {
		h.log.Error("payout customer credit proxy failed", zap.Error(err))
		jsonError(w, "failed to pay out credit", http.StatusBadGateway)
		return
	}
	jsonOK(w, resp)
}
