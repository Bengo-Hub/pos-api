package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
)

// PaymentHandler handles POS payment endpoints.
type PaymentHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewPaymentHandler(log *zap.Logger, client *ent.Client) *PaymentHandler {
	return &PaymentHandler{log: log, client: client}
}

type recordPaymentInput struct {
	TenderID uuid.UUID `json:"tenderId"`
	Amount   float64   `json:"amount"`
	Currency string    `json:"currency"`
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
	if input.Currency == "" {
		input.Currency = "KES"
	}

	// Verify order exists and belongs to tenant
	_, err = h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	payment, err := h.client.POSPayment.Create().
		SetOrderID(orderID).
		SetTenderID(input.TenderID).
		SetAmount(input.Amount).
		SetCurrency(input.Currency).
		SetStatus("completed").
		Save(r.Context())
	if err != nil {
		h.log.Error("record payment failed", zap.Error(err))
		jsonError(w, "failed to record payment", http.StatusInternalServerError)
		return
	}

	// Update order status to completed
	_, _ = h.client.POSOrder.Update().
		Where(posorder.ID(orderID)).
		SetStatus("completed").
		Save(r.Context())

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, payment)
}

// ListOrderPayments handles GET /{tenantID}/pos/orders/{orderID}/payments
func (h *PaymentHandler) ListOrderPayments(w http.ResponseWriter, r *http.Request) {
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	payments, err := h.client.POSPayment.Query().
		Where(pospayment.OrderID(orderID)).
		All(r.Context())
	if err != nil {
		h.log.Error("list payments failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": payments, "total": len(payments)})
}
