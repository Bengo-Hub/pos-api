package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/payments"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// PaymentHandler handles POS payment endpoints.
type PaymentHandler struct {
	log            *zap.Logger
	paymentSvc     *payments.Service
	treasuryClient *treasury.Client
	publicBaseURL  string
}

func NewPaymentHandler(log *zap.Logger, paymentSvc *payments.Service, treasuryClient *treasury.Client, publicBaseURL string) *PaymentHandler {
	return &PaymentHandler{
		log:            log,
		paymentSvc:     paymentSvc,
		treasuryClient: treasuryClient,
		publicBaseURL:  publicBaseURL,
	}
}

type createIntentInput struct {
	TenderID     uuid.UUID `json:"tenderId"`
	TenderMethod string    `json:"tenderMethod"` // cash | card | mpesa | room_charge
	Amount       float64   `json:"amount"`
	Currency     string    `json:"currency"`
}

// CreatePaymentIntent handles POST /{tenantID}/pos/orders/{orderID}/payments/intent
// Returns payment_intent_id + initiate_url for the pos-ui to open TreasuryPaymentModal.
// For cash/manual/room_charge the payment is settled immediately (IsCash=true).
func (h *PaymentHandler) CreatePaymentIntent(w http.ResponseWriter, r *http.Request) {
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

	tenantSlug := chi.URLParam(r, "tenantID")

	var input createIntentInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	result, err := h.paymentSvc.CreatePaymentIntent(r.Context(), payments.RecordPaymentRequest{
		TenantID:      tid,
		TenantSlug:    tenantSlug,
		OrderID:       orderID,
		TenderID:      input.TenderID,
		TenderMethod:  input.TenderMethod,
		Amount:        input.Amount,
		Currency:      input.Currency,
		PublicBaseURL: h.publicBaseURL,
	})
	if err != nil {
		h.log.Error("create payment intent failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{
		"payment_intent_id": result.PaymentIntentID,
		"initiate_url":      result.InitiateURL,
		"is_cash":           result.IsCash,
	})
}

type initiateInput struct {
	IntentID      string `json:"intent_id"`
	PaymentMethod string `json:"payment_method"`
	Phone         string `json:"phone,omitempty"`
	ReturnURL     string `json:"return_url,omitempty"`
}

// ProxyInitiate handles POST /{tenantID}/pos/payments/initiate
// This is the initiateUrl that TreasuryPaymentModal calls when the user selects a gateway.
// pos-api forwards the request to treasury-api and returns the response verbatim.
func (h *PaymentHandler) ProxyInitiate(w http.ResponseWriter, r *http.Request) {
	tenantSlug := chi.URLParam(r, "tenantID")

	var input initiateInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if h.treasuryClient == nil {
		jsonError(w, "treasury client not configured", http.StatusServiceUnavailable)
		return
	}

	resp, err := h.treasuryClient.InitiateIntent(r.Context(), tenantSlug, input.IntentID, treasury.InitiateRequest{
		PaymentMethod: input.PaymentMethod,
		Phone:         input.Phone,
		ReturnURL:     input.ReturnURL,
	})
	if err != nil {
		h.log.Error("initiate intent proxy failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	jsonOK(w, resp)
}

type recordPaymentInput struct {
	TenderID  uuid.UUID `json:"tenderId"`
	Amount    float64   `json:"amount"`
	Currency  string    `json:"currency"`
	Reference string    `json:"reference"`
}

// RecordPayment handles POST /{tenantID}/pos/orders/{orderID}/payments
// Legacy direct-record path for internal/offline use (no treasury round-trip).
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
