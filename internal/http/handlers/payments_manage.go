package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/modules/payments"
)

// View Payments modal actions: edit a payment's descriptive fields, void a payment, and
// send the customer a payment-received notification. Routes are pos.payments.manage-gated;
// the service layer additionally restricts edit/void to manually-recorded tenders.

// UpdateOrderPayment handles PATCH /{tenantID}/pos/orders/{orderID}/payments/{paymentID}.
// Editable: reference, note, occurred_at, tender (method). NEVER the amount — void and
// re-record to change money.
func (h *PaymentHandler) UpdateOrderPayment(w http.ResponseWriter, r *http.Request) {
	tid, orderID, paymentID, ok := h.paymentRouteIDs(w, r)
	if !ok {
		return
	}
	var input payments.UpdatePaymentInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	p, err := h.paymentSvc.UpdatePayment(r.Context(), tid, orderID, paymentID, input)
	if err != nil {
		h.log.Warn("update payment failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	jsonOK(w, p)
}

// VoidOrderPayment handles DELETE /{tenantID}/pos/orders/{orderID}/payments/{paymentID} —
// a soft void: row → voided, paid_total recomputed, order reopened if no longer covered,
// audit event written, and a best-effort treasury reversal keyed on the payment id.
// Body: {"reason": "..."} (optional).
func (h *PaymentHandler) VoidOrderPayment(w http.ResponseWriter, r *http.Request) {
	tid, orderID, paymentID, ok := h.paymentRouteIDs(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	voidedBy := uuid.Nil
	tenantSlug := chi.URLParam(r, "tenantID")
	if claims, cok := authclient.ClaimsFromContext(r.Context()); cok && claims != nil {
		if uid, err := uuid.Parse(claims.Subject); err == nil {
			voidedBy = uid
		}
		if slug := claims.GetTenantSlug(); slug != "" {
			tenantSlug = slug
		}
	}
	p, err := h.paymentSvc.VoidPayment(r.Context(), tid, tenantSlug, orderID, paymentID, voidedBy, body.Reason)
	if err != nil {
		h.log.Warn("void payment failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	jsonOK(w, p)
}

// NotifyOrderPayment handles POST /{tenantID}/pos/orders/{orderID}/payments/{paymentID}/notify.
func (h *PaymentHandler) NotifyOrderPayment(w http.ResponseWriter, r *http.Request) {
	tid, orderID, paymentID, ok := h.paymentRouteIDs(w, r)
	if !ok {
		return
	}
	if err := h.paymentSvc.NotifyPaymentReceived(r.Context(), tid, orderID, paymentID); err != nil {
		h.log.Warn("notify payment failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	jsonOK(w, map[string]any{"status": "queued"})
}

// paymentRouteIDs parses the (tenant, order, payment) ids shared by the payment-management routes.
func (h *PaymentHandler) paymentRouteIDs(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	paymentID, err := uuid.Parse(chi.URLParam(r, "paymentID"))
	if err != nil {
		jsonError(w, "invalid payment_id", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return tid, orderID, paymentID, true
}
