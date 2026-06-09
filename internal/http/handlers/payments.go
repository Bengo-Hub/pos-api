package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/payments"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// PaymentHandler handles POS payment endpoints.
type PaymentHandler struct {
	log            *zap.Logger
	paymentSvc     *payments.Service
	treasuryClient *treasury.Client
	publicBaseURL  string
	client         *ent.Client
}

func NewPaymentHandler(log *zap.Logger, paymentSvc *payments.Service, treasuryClient *treasury.Client, publicBaseURL string, entClient *ent.Client) *PaymentHandler {
	return &PaymentHandler{
		log:            log,
		paymentSvc:     paymentSvc,
		treasuryClient: treasuryClient,
		publicBaseURL:  publicBaseURL,
		client:         entClient,
	}
}

type createIntentInput struct {
	TenderID     uuid.UUID `json:"tenderId"`
	TenderMethod string    `json:"tenderMethod"` // cash | card | mpesa | manual | room_charge
	Amount       float64   `json:"amount"`
	Currency     string    `json:"currency"`
	ExternalRef  string    `json:"externalRef,omitempty"` // cashier-entered ref for manual/paybill payments
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
		ExternalRef:   input.ExternalRef,
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

// quotationFromCartInput is the pos-ui Add Sale "Save as Quotation" body.
type quotationFromCartInput struct {
	CustomerName  string `json:"customer_name"`
	CustomerPhone string `json:"customer_phone"`
	CustomerEmail string `json:"customer_email"`
	Notes         string `json:"notes"`
	Lines         []struct {
		Name      string  `json:"name"`
		SKU       string  `json:"sku"`
		Quantity  float64 `json:"quantity"`
		UnitPrice float64 `json:"unit_price"`
	} `json:"lines"`
}

// CreateQuotationFromCart handles POST /{tenantID}/pos/quotations — forwards a pos cart to treasury
// as a quotation (treasury owns quotations; pos persists nothing). pos-ui → pos-api → treasury S2S,
// because the INTERNAL_SERVICE_KEY must never reach the browser.
func (h *PaymentHandler) CreateQuotationFromCart(w http.ResponseWriter, r *http.Request) {
	tenantSlug := chi.URLParam(r, "tenantID")

	var input quotationFromCartInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(input.Lines) == 0 {
		jsonError(w, "at least one line is required", http.StatusBadRequest)
		return
	}
	if h.treasuryClient == nil {
		jsonError(w, "treasury client not configured", http.StatusServiceUnavailable)
		return
	}

	lines := make([]treasury.QuotationLine, 0, len(input.Lines))
	for _, l := range input.Lines {
		lines = append(lines, treasury.QuotationLine{
			Description: l.Name, ItemSKU: l.SKU, Quantity: l.Quantity, UnitPrice: l.UnitPrice,
		})
	}
	now := time.Now()
	resp, err := h.treasuryClient.CreateQuotation(r.Context(), tenantSlug, treasury.CreateQuotationRequest{
		CustomerName:  input.CustomerName,
		CustomerPhone: input.CustomerPhone,
		CustomerEmail: input.CustomerEmail,
		Notes:         input.Notes,
		QuoteDate:     now.Format("2006-01-02"),
		ValidUntil:    now.AddDate(0, 0, 30).Format("2006-01-02"),
		Currency:      "KES",
		ReferenceType: "pos_cart",
		Lines:         lines,
	})
	if err != nil {
		h.log.Error("create quotation proxy failed", zap.Error(err))
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

// GetGateways handles GET /{tenantID}/pos/gateways
// Proxies the treasury public gateway availability response so the POS UI can
// conditionally show only the payment methods the tenant has enabled.
// Fails open: if treasury is unreachable all gateways are returned as enabled.
func (h *PaymentHandler) GetGateways(w http.ResponseWriter, r *http.Request) {
	// Resolve tenant slug: JWT claims → httpware context → local Tenant table (PIN JWT fallback).
	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	if tenantSlug == "" {
		tenantSlug = httpware.GetTenantSlug(r.Context())
	}
	if tenantSlug == "" && h.client != nil {
		if tid, parseErr := parseTenantUUID(r); parseErr == nil {
			if t, lookupErr := h.client.Tenant.Get(r.Context(), tid); lookupErr == nil {
				tenantSlug = t.Slug
			}
		}
	}

	if tenantSlug == "" || h.treasuryClient == nil {
		jsonOK(w, map[string]any{"mpesa": true, "paystack": true, "wallet": true, "cod": true})
		return
	}

	gateways, err := h.treasuryClient.GetPublicGateways(r.Context(), tenantSlug)
	if err != nil {
		h.log.Warn("get public gateways failed — failing open", zap.String("tenant", tenantSlug), zap.Error(err))
		jsonOK(w, map[string]any{"mpesa": true, "paystack": true, "wallet": true, "cod": true})
		return
	}

	jsonOK(w, gateways)
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

type recordExpenseInput struct {
	CategoryID  string  `json:"category_id,omitempty"`
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
	TaxAmount   float64 `json:"tax_amount,omitempty"`
	Currency    string  `json:"currency,omitempty"`
	ReceiptURL  string  `json:"receipt_url,omitempty"`
}

// RecordExpense handles POST /{tenant}/pos/expenses — records a petty-cash expense entered at the
// register straight to treasury (the "Add Expense" flow), attributed to the cashier and outlet.
// No money moves through the till; it is a finance record owned by treasury.
func (h *PaymentHandler) RecordExpense(w http.ResponseWriter, r *http.Request) {
	if h.treasuryClient == nil {
		jsonError(w, "treasury client not configured", http.StatusInternalServerError)
		return
	}
	var in recordExpenseInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if in.Description == "" || in.Amount <= 0 {
		jsonError(w, "description and a positive amount are required", http.StatusBadRequest)
		return
	}

	tenantSlug := ""
	submittedBy := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
		submittedBy = claims.Subject
	}
	if tenantSlug == "" {
		tenantSlug = httpware.GetTenantSlug(r.Context())
	}
	if tenantSlug == "" && h.client != nil {
		if tid, parseErr := parseTenantUUID(r); parseErr == nil {
			if t, lookupErr := h.client.Tenant.Get(r.Context(), tid); lookupErr == nil {
				tenantSlug = t.Slug
			}
		}
	}
	if tenantSlug == "" {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}

	resp, err := h.treasuryClient.RecordExpense(r.Context(), tenantSlug, treasury.ExpenseRequest{
		CategoryID:    in.CategoryID,
		Description:   in.Description,
		Amount:        in.Amount,
		TaxAmount:     in.TaxAmount,
		Currency:      in.Currency,
		ReceiptURL:    in.ReceiptURL,
		ExpenseDate:   time.Now().UTC().Format(time.RFC3339),
		OutletID:      httpware.GetOutletID(r.Context()),
		SubmittedBy:   submittedBy,
		SourceService: "pos",
	})
	if err != nil {
		h.log.Error("record expense failed", zap.Error(err))
		jsonError(w, "failed to record expense", http.StatusBadGateway)
		return
	}
	jsonOK(w, resp)
}

// ListC2BCandidates handles GET /{tenant}/pos/c2b/payments — proxies the treasury C2B inbox query so
// the cashier can find an unreconciled till/paybill payment to bind to the open sale.
func (h *PaymentHandler) ListC2BCandidates(w http.ResponseWriter, r *http.Request) {
	tenantSlug := h.resolveTenantSlug(r)
	if tenantSlug == "" || h.treasuryClient == nil {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}
	resp, err := h.treasuryClient.ListC2BCandidates(r.Context(), tenantSlug, r.URL.RawQuery)
	if err != nil {
		h.log.Error("list c2b candidates failed", zap.Error(err))
		jsonError(w, "failed to query c2b payments", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// ClaimC2BPayment handles POST /{tenant}/pos/c2b/payments/{transID}/claim — binds a C2B payment to a sale.
func (h *PaymentHandler) ClaimC2BPayment(w http.ResponseWriter, r *http.Request) {
	tenantSlug := h.resolveTenantSlug(r)
	if tenantSlug == "" || h.treasuryClient == nil {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}
	transID := chi.URLParam(r, "transID")
	var body struct {
		POSOrderID string  `json:"pos_order_id"`
		Amount     float64 `json:"amount"`
		TenderID   string  `json:"tender_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	resp, err := h.treasuryClient.ClaimC2BPayment(r.Context(), tenantSlug, transID, body.POSOrderID)
	if err != nil {
		h.log.Error("claim c2b payment failed", zap.Error(err))
		jsonError(w, "failed to claim c2b payment", http.StatusBadGateway)
		return
	}

	// Settle the POS order with the claimed C2B amount: record a completed payment (reference =
	// M-Pesa TransID) and close the order if it is now fully paid. Best-effort — the treasury bind
	// already succeeded, so a settle error must not fail the claim.
	if body.POSOrderID != "" && body.Amount > 0 && h.paymentSvc != nil {
		if tid, terr := parseTenantUUID(r); terr == nil {
			if orderID, oerr := uuid.Parse(body.POSOrderID); oerr == nil {
				tenderID, _ := uuid.Parse(body.TenderID) // uuid.Nil when not supplied
				if _, perr := h.paymentSvc.RecordPayment(r.Context(), payments.RecordPaymentRequest{
					TenantID:  tid,
					OrderID:   orderID,
					TenderID:  tenderID,
					Amount:    body.Amount,
					Currency:  "KES",
					Reference: transID,
				}); perr != nil {
					h.log.Error("c2b: settle order failed after claim", zap.String("trans_id", transID), zap.Error(perr))
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// resolveTenantSlug resolves the tenant slug from JWT claims, httpware context, or the local Tenant
// table (PIN JWT fallback) — the same precedence used by GetGateways/RecordExpense.
func (h *PaymentHandler) resolveTenantSlug(r *http.Request) string {
	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	if tenantSlug == "" {
		tenantSlug = httpware.GetTenantSlug(r.Context())
	}
	if tenantSlug == "" && h.client != nil {
		if tid, parseErr := parseTenantUUID(r); parseErr == nil {
			if t, lookupErr := h.client.Tenant.Get(r.Context(), tid); lookupErr == nil {
				tenantSlug = t.Slug
			}
		}
	}
	return tenantSlug
}
