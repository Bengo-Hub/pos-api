package handlers

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Quotation proxy endpoints — full GET/PUT/PATCH + lifecycle passthroughs to treasury's S2S
// quotation CRUD (treasury OWNS quotations; pos persists nothing and never reshapes the
// document). Together with CreateQuotationFromCart/ListQuotationsProxy (payments.go) the POS
// side gets the same document logic treasury-ui uses: get, post, put, patch and the
// send/accept/decline/cancel actions — one source of truth, INTERNAL_SERVICE_KEY never
// reaching the browser.

// quotationActions whitelists the lifecycle verbs the POS proxy may forward.
var quotationActions = map[string]bool{"send": true, "accept": true, "decline": true, "cancel": true}

// GetQuotationProxy handles GET /{tenantID}/pos/quotations/{quotationID}.
func (h *PaymentHandler) GetQuotationProxy(w http.ResponseWriter, r *http.Request) {
	if h.treasuryClient == nil {
		jsonError(w, "treasury client not configured", http.StatusServiceUnavailable)
		return
	}
	raw, err := h.treasuryClient.GetQuotation(r.Context(), chi.URLParam(r, "tenantID"), chi.URLParam(r, "quotationID"))
	if err != nil {
		h.log.Warn("get quotation proxy failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// UpdateQuotationProxy handles PUT+PATCH /{tenantID}/pos/quotations/{quotationID} — raw body
// passthrough so treasury's UpdateQuotation applies the same draft-only validation it applies
// to treasury-ui edits.
func (h *PaymentHandler) UpdateQuotationProxy(w http.ResponseWriter, r *http.Request) {
	if h.treasuryClient == nil {
		jsonError(w, "treasury client not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	raw, err := h.treasuryClient.UpdateQuotation(r.Context(), chi.URLParam(r, "tenantID"), chi.URLParam(r, "quotationID"), r.Method, body)
	if err != nil {
		h.log.Warn("update quotation proxy failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// QuotationActionProxy handles POST /{tenantID}/pos/quotations/{quotationID}/{action} for the
// whitelisted lifecycle verbs (send/accept/decline/cancel). Accept converts to an invoice in
// treasury — the same behaviour as treasury-ui's Accept action.
func (h *PaymentHandler) QuotationActionProxy(w http.ResponseWriter, r *http.Request) {
	if h.treasuryClient == nil {
		jsonError(w, "treasury client not configured", http.StatusServiceUnavailable)
		return
	}
	action := chi.URLParam(r, "action")
	if !quotationActions[action] {
		jsonError(w, "unsupported quotation action", http.StatusBadRequest)
		return
	}
	raw, err := h.treasuryClient.QuotationAction(r.Context(), chi.URLParam(r, "tenantID"), chi.URLParam(r, "quotationID"), action)
	if err != nil {
		h.log.Warn("quotation action proxy failed", zap.String("action", action), zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}
