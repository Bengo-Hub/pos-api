package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/payments"
)

// arReconcileInput is the body for POST /{tenantID}/pos/ar/reconcile — the platform-owner data-heal
// tool that settles a customer's OPEN POS credit orders down to treasury's authoritative AR balance
// (reduce-only, FIFO oldest-first). This is the manual/on-demand counterpart to the automatic
// reconcile that fires on every treasury.customer.balance_updated event (see
// payments/ar_reconcile.go + treasury_balance_subscriber.go) — for healing orders that went stale
// BEFORE that fix shipped, or for a fleet-wide dry-run sweep.
type arReconcileInput struct {
	CrmContactID       string `json:"crm_contact_id,omitempty"`
	CustomerIdentifier string `json:"customer_identifier,omitempty"`
	Phone              string `json:"phone,omitempty"` // convenience alias for customer_identifier
	DryRun             bool   `json:"dry_run"`
}

// ReconcileAR handles POST /{tenantID}/pos/ar/reconcile (platform-owner only — see router.go
// requirePlatformOwner). Fetches the customer's LIVE treasury balance as the reconcile target, then
// settles POS's own open credit orders down to it. dry_run defaults to a caller-specified bool
// (false settles for real) — callers doing a fleet sweep should always dry-run first.
func (h *PaymentHandler) ReconcileAR(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var input arReconcileInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	identifier := strings.TrimSpace(input.CustomerIdentifier)
	if identifier == "" {
		identifier = strings.TrimSpace(input.Phone)
	}
	crmContactID := strings.TrimSpace(input.CrmContactID)
	key := crmContactID
	if key == "" {
		key = identifier
	}
	if key == "" {
		jsonError(w, "crm_contact_id or customer_identifier required", http.StatusBadRequest)
		return
	}
	if h.treasuryClient == nil {
		jsonError(w, "treasury client not configured", http.StatusServiceUnavailable)
		return
	}
	tenantSlug := chi.URLParam(r, "tenantID")
	terms, err := h.treasuryClient.GetCreditTerms(r.Context(), tenantSlug, key)
	if err != nil {
		h.log.Error("ar reconcile: fetch treasury balance failed", zap.Error(err))
		jsonError(w, "failed to load treasury balance", http.StatusBadGateway)
		return
	}
	target, _ := strconv.ParseFloat(terms.BalanceDue, 64)
	if target < 0 {
		target = 0 // a negative balance_due is store credit owed TO the customer, not AR — never a POS-order target
	}

	report, err := h.paymentSvc.ReconcileCustomerOrders(r.Context(), payments.ReconcileParams{
		TenantID:           tid,
		CrmContactID:       crmContactID,
		CustomerIdentifier: identifier,
		TargetOutstanding:  target,
		Reference:          "manual-reconcile",
		DryRun:             input.DryRun,
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, report)
}
