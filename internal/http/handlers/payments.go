package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/payments"
)

// PaymentHandler handles POS payment endpoints.
type PaymentHandler struct {
	log        *zap.Logger
	paymentSvc *payments.Service
}

func NewPaymentHandler(log *zap.Logger, paymentSvc *payments.Service) *PaymentHandler {
	return &PaymentHandler{log: log, paymentSvc: paymentSvc}
}

type recordPaymentInput struct {
	TenderID  uuid.UUID `json:"tenderId"`
	Amount    float64   `json:"amount"`
	Currency  string    `json:"currency"`
	Reference string    `json:"reference"`
}

// RecordPayment handles POST /{tenantID}/pos/orders/{orderID}/payments
func (h *PaymentHandler) RecordPayment(w http.ResponseWriter, r *http.Request) {
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

	var input recordPaymentInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	payment, err := h.paymentSvc.RecordPayment(r.Context(), payments.RecordPaymentRequest{
		TenantID:  tid,
		OrderID:   orderID,
		TenderID:  input.TenderID,
		Amount:    input.Amount,
		Currency:  input.Currency,
		Reference: input.Reference,
	})
	if err != nil {
		h.log.Error("record payment failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, payment)
}

// ListOrderPayments handles GET /{tenantID}/pos/orders/{orderID}/payments
func (h *PaymentHandler) ListOrderPayments(w http.ResponseWriter, r *http.Request) {
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

	list, err := h.paymentSvc.ListOrderPayments(r.Context(), tid, orderID)
	if err != nil {
		h.log.Error("list payments failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": list, "total": len(list)})
}
