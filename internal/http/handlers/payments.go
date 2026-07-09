package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
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
	rbac           outletmw.PermissionChecker
	// auditSvc + terminalSecret authorize the Complimentary/no-charge tender: a manager approval
	// (live PIN/card step-up token, or a one-time code) is mandatory, mirroring VoidOrder's
	// mechanism (see orders.go). Optional; when nil, complimentary is rejected outright.
	auditSvc       *audit.Service
	terminalSecret []byte
}

// SetRBAC wires the RBAC checker used for in-handler permission checks whose required permission
// depends on the request body (the credit-sale tender shares a route with cash tenders).
func (h *PaymentHandler) SetRBAC(rbac outletmw.PermissionChecker) { h.rbac = rbac }

// SetAuditService wires the audit log used to record who approved a complimentary sale.
func (h *PaymentHandler) SetAuditService(a *audit.Service) { h.auditSvc = a }

// SetTerminalSecret wires the HMAC secret used to verify step-up approval tokens
// (see step_up.go verifyApprovalToken) for the Complimentary/no-charge tender.
func (h *PaymentHandler) SetTerminalSecret(s []byte) { h.terminalSecret = s }

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
	// Credit-sale (on_account) extras captured by the credit-sale details modal:
	// explicit due date (RFC3339 or YYYY-MM-DD; wins over the customer's treasury credit
	// period, which wins over the +30-day default) and free-text notes.
	PaymentDueDate string `json:"paymentDueDate,omitempty"`
	CreditNotes    string `json:"creditNotes,omitempty"`
	// Complimentary (no-charge) extras: a mandatory reason, plus ONE of a live manager
	// PIN/card step-up token or a manager-generated one-time code — mirrors Void's dual
	// approval path (see step_up.go / order_void_code.go).
	Reason        string `json:"reason,omitempty"`
	ApprovalToken string `json:"approvalToken,omitempty"`
	ApprovalCode  string `json:"approvalCode,omitempty"`
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

	// Credit sales (sell on account) are available to CASHIERS too (product decision
	// 2026-07-07): the route's pos.payments.add gate covers it, and the real guardrails are
	// server-side regardless of role — a credit sale requires a selected customer with a
	// phone, and treasury enforces the customer's credit limit before the debt is booked.
	// The credit-sale details modal captures the due date (default +30 days) and notes.

	// Parse the optional explicit due date (RFC3339 or YYYY-MM-DD).
	var dueDate *time.Time
	if raw := strings.TrimSpace(input.PaymentDueDate); raw != "" {
		if t, perr := time.Parse(time.RFC3339, raw); perr == nil {
			dueDate = &t
		} else if t, perr := time.Parse("2006-01-02", raw); perr == nil {
			// End of the local business day, so a sale isn't "overdue" on its due date's morning.
			eod := t.Add(23*time.Hour + 59*time.Minute)
			dueDate = &eod
		} else {
			jsonError(w, "invalid paymentDueDate — use YYYY-MM-DD", http.StatusBadRequest)
			return
		}
	}

	req := payments.RecordPaymentRequest{
		TenantID:       tid,
		TenantSlug:     tenantSlug,
		OrderID:        orderID,
		TenderID:       input.TenderID,
		TenderMethod:   input.TenderMethod,
		Amount:         input.Amount,
		Currency:       input.Currency,
		ExternalRef:    input.ExternalRef,
		PublicBaseURL:  h.publicBaseURL,
		PaymentDueDate: dueDate,
		CreditNotes:    strings.TrimSpace(input.CreditNotes),
	}

	// Complimentary (no-charge): mandatory reason + mandatory manager approval (live PIN/card
	// step-up token, or a one-time code a manager generated remotely) — mirrors VoidOrder's dual
	// approval path. Enforced here, in the HTTP layer, same as VoidOrder does it in orders.go.
	if strings.EqualFold(input.TenderMethod, payments.TenderComplimentary) {
		reason := strings.TrimSpace(input.Reason)
		if reason == "" {
			jsonError(w, "a reason is required for a complimentary sale", http.StatusBadRequest)
			return
		}

		var approverID uuid.UUID
		var approved bool
		if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
			approverID, approved = verifyApprovalToken(input.ApprovalToken, "order.complimentary", h.terminalSecret)
		} else if input.ApprovalCode != "" {
			approverID, approved = redeemOrderApprovalCode(r.Context(), h.client, h.log, tid, orderID, "order.complimentary", input.ApprovalCode)
		}
		if !approved {
			jsonError(w, "manager approval is required for a complimentary sale", http.StatusForbidden)
			return
		}

		req.ComplimentaryReason = reason
		req.ApprovedByUserID = &approverID

		if h.auditSvc != nil {
			var outletID *uuid.UUID
			var amount *float64
			if order, oerr := h.client.POSOrder.Query().
				Where(posorder.ID(orderID), posorder.TenantID(tid)).Only(r.Context()); oerr == nil {
				oid := order.OutletID
				outletID = &oid
				amt := order.TotalAmount
				amount = &amt
			}
			h.auditSvc.Record(r.Context(), audit.Entry{
				TenantID:    tid,
				OutletID:    outletID,
				ActorUserID: approverID,
				ApproverID:  &approverID,
				Action:      "order.complimentary",
				EntityType:  "pos_order",
				EntityID:    orderID.String(),
				Reason:      reason,
				Amount:      amount,
			})
		}
	}

	result, err := h.paymentSvc.CreatePaymentIntent(r.Context(), req)
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
	// Quotations must be for a real customer (QA req 3: no walk-in). Phone is required — it is
	// the CRM link key that lets treasury attribute the quotation to the right customer.
	name := strings.TrimSpace(input.CustomerName)
	if name == "" || strings.EqualFold(name, "walk-in customer") || strings.EqualFold(name, "walk in customer") ||
		strings.TrimSpace(input.CustomerPhone) == "" {
		jsonError(w, "a customer with a phone number is required for quotations", http.StatusBadRequest)
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
	// Link the quotation to the SELECTED customer's CRM contact (resolved from the loyalty account by
	// phone — same canonical key credit sales use), so quotations show against the right customer in
	// treasury. Best-effort: an empty id just leaves the quotation keyed by name/phone as before.
	crmContactID := ""
	if input.CustomerPhone != "" {
		if tid, terr := parseTenantUUID(r); terr == nil {
			crmContactID = h.paymentSvc.ResolveCrmContactID(r.Context(), tid, input.CustomerPhone)
		}
	}
	now := time.Now()
	resp, err := h.treasuryClient.CreateQuotation(r.Context(), tenantSlug, treasury.CreateQuotationRequest{
		CrmContactID:  crmContactID,
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

// ListQuotationsProxy handles GET /{tenantID}/pos/quotations — proxies the treasury S2S
// quotation list so the POS "Quotation" transactions tab can page through quotations without
// the INTERNAL_SERVICE_KEY ever reaching the browser. Query params (status/from/to/limit/page)
// pass through verbatim.
func (h *PaymentHandler) ListQuotationsProxy(w http.ResponseWriter, r *http.Request) {
	tenantSlug := chi.URLParam(r, "tenantID")
	if h.treasuryClient == nil {
		jsonError(w, "treasury client not configured", http.StatusServiceUnavailable)
		return
	}
	raw, err := h.treasuryClient.ListQuotations(r.Context(), tenantSlug, r.URL.RawQuery)
	if err != nil {
		h.log.Error("list quotations proxy failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
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
	// payg (pay-as-you-go / service_charge billing): the platform earns only a per-sale
	// commission, which can ONLY be netted on platform-routed online rails. Cash/offline
	// (wallet, COD, on-account) would let the commission leak, so they are hidden for PAYG
	// tenants — they see online methods (M-Pesa, Paystack/Card) only.
	payg := false
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
		payg = claims.BillingMode == "service_charge"
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

	// Online-only default for PAYG; full set otherwise. Used both on the no-treasury
	// path and the fail-open path so PAYG restriction holds even when treasury is down.
	// "complimentary" deliberately fails CLOSED (unlike the others) — it's an opt-in tender
	// that must be explicitly enabled per tenant in treasury; it must never silently appear
	// just because treasury is unreachable.
	openDefault := map[string]any{"mpesa": true, "paystack": true, "wallet": !payg, "cod": !payg, "complimentary": false}

	if tenantSlug == "" || h.treasuryClient == nil {
		jsonOK(w, openDefault)
		return
	}

	gateways, err := h.treasuryClient.GetPublicGateways(r.Context(), tenantSlug)
	if err != nil {
		h.log.Warn("get public gateways failed — failing open", zap.String("tenant", tenantSlug), zap.Error(err))
		jsonOK(w, openDefault)
		return
	}

	if payg {
		// Strip offline / on-account rails the platform can't auto-charge.
		gateways.Wallet = false
		gateways.COD = false
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

	// Detailed rows: tender name/type, note, and a voidable flag for the View Payments
	// modal actions (raw rows only carry tender_id + a JSON blob).
	list, err := h.paymentSvc.ListOrderPaymentsDetailed(r.Context(), tid, orderID)
	if err != nil {
		h.log.Error("list payments failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": list, "total": len(list)})
}

type recordExpenseInput struct {
	CategoryID   string  `json:"category_id,omitempty"`
	ReferenceNo  string  `json:"reference_no,omitempty"` // "Reference No" → treasury expense_number (autogen when empty)
	Description  string  `json:"description"`            // "Expense note"
	Amount       float64 `json:"amount"`                 // "Total amount"
	TaxAmount    float64 `json:"tax_amount,omitempty"`   // "Applicable Tax" (computed amount)
	Currency     string  `json:"currency,omitempty"`
	ReceiptURL   string  `json:"receipt_url,omitempty"`
	ExpenseDate  string  `json:"expense_date,omitempty"` // "Date" (ISO date/datetime); defaults to today server-side
	AccountID    string  `json:"account_id,omitempty"`   // "Payment Account" (chart-of-accounts UUID)
	VendorID     string  `json:"vendor_id,omitempty"`    // "Expense for" (when a vendor is chosen)
	CostCenterID string  `json:"cost_center_id,omitempty"`
	// PaymentMethod/PaidOn/PaymentNote/ExpenseFor are the GoDigital payment block + label fields.
	// Treasury has no dedicated columns for them, so they are forwarded into the expense metadata.
	PaymentMethod string  `json:"payment_method,omitempty"`
	PaidOn        string  `json:"paid_on,omitempty"`
	PaymentNote   string  `json:"payment_note,omitempty"`
	PaymentAmount float64 `json:"payment_amount,omitempty"` // amount paid now (payment block); informational, stored in metadata
	ExpenseFor    string  `json:"expense_for,omitempty"`    // free-text label when no vendor selected
	TaxRate       float64 `json:"tax_rate,omitempty"`       // selected tax rate %, informational
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

	// Collect the GoDigital payment-block + label fields that have no dedicated treasury column
	// into metadata, so nothing the cashier enters is lost.
	metadata := map[string]any{}
	if in.PaymentMethod != "" {
		metadata["payment_method"] = in.PaymentMethod
	}
	if in.PaidOn != "" {
		metadata["paid_on"] = in.PaidOn
	}
	if in.PaymentNote != "" {
		metadata["payment_note"] = in.PaymentNote
	}
	if in.PaymentAmount > 0 {
		metadata["payment_amount"] = in.PaymentAmount
	}
	if in.ExpenseFor != "" {
		metadata["expense_for"] = in.ExpenseFor
	}
	if in.TaxRate > 0 {
		metadata["tax_rate"] = in.TaxRate
	}
	if len(metadata) == 0 {
		metadata = nil
	}

	// "Date" — use the cashier-supplied date when present (accept either a plain date or a full
	// timestamp); treasury defaults to today when omitted.
	expenseDate := in.ExpenseDate
	if expenseDate != "" {
		if t, perr := time.Parse("2006-01-02", expenseDate); perr == nil {
			expenseDate = t.UTC().Format(time.RFC3339)
		}
	} else {
		expenseDate = time.Now().UTC().Format(time.RFC3339)
	}

	// Treasury S2S resolves {tenant} as a UUID — pass the URL tenant UUID, not the slug.
	resp, err := h.treasuryClient.RecordExpense(r.Context(), chi.URLParam(r, "tenantID"), treasury.ExpenseRequest{
		ExpenseNumber: in.ReferenceNo,
		CategoryID:    in.CategoryID,
		Description:   in.Description,
		Amount:        in.Amount,
		TaxAmount:     in.TaxAmount,
		Currency:      in.Currency,
		ReceiptURL:    in.ReceiptURL,
		ExpenseDate:   expenseDate,
		AccountID:     in.AccountID,
		VendorID:      in.VendorID,
		CostCenterID:  in.CostCenterID,
		OutletID:      httpware.GetOutletID(r.Context()),
		SubmittedBy:   submittedBy,
		SourceService: "pos",
		Metadata:      metadata,
	})
	if err != nil {
		h.log.Error("record expense failed", zap.Error(err))
		jsonError(w, "failed to record expense", http.StatusBadGateway)
		return
	}
	jsonOK(w, resp)
}

// ListExpenseCategories handles GET /{tenant}/pos/expenses/categories — proxies treasury's expense
// category list so the Add-Expense form can populate the "Expense Category" dropdown.
func (h *PaymentHandler) ListExpenseCategories(w http.ResponseWriter, r *http.Request) {
	// Treasury's S2S /{tenant}/... routes resolve the tenant strictly as a UUID (no slug→UUID
	// middleware on the s2s group) — passing the slug 400s. Pass the tenant UUID.
	tid, terr := parseTenantUUID(r)
	if terr != nil || h.treasuryClient == nil {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}
	resp, err := h.treasuryClient.ListExpenseCategories(r.Context(), tid.String())
	if err != nil {
		h.log.Error("list expense categories failed", zap.Error(err))
		jsonError(w, "failed to load expense categories", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// ListExpenseAccounts handles GET /{tenant}/pos/expenses/accounts — proxies treasury's chart of
// accounts so the Add-Expense form can populate the "Payment Account" dropdown.
func (h *PaymentHandler) ListExpenseAccounts(w http.ResponseWriter, r *http.Request) {
	tid, terr := parseTenantUUID(r)
	if terr != nil || h.treasuryClient == nil {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}
	resp, err := h.treasuryClient.ListExpenseAccounts(r.Context(), tid.String())
	if err != nil {
		h.log.Error("list expense accounts failed", zap.Error(err))
		jsonError(w, "failed to load payment accounts", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// ListTaxCodes handles GET /{tenant}/pos/tax-codes — proxies treasury's tax-code list so the POS
// Settings → Tax tab can show the tenant's treasury-sourced tax codes/rates (the actual source of
// truth for per-item tax). Treasury, not POS, owns these rates; the POS terminal applies each item's
// own enriched tax_rate/tax_inclusive at checkout.
func (h *PaymentHandler) ListTaxCodes(w http.ResponseWriter, r *http.Request) {
	tid, terr := parseTenantUUID(r)
	if terr != nil || h.treasuryClient == nil {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}
	codes, err := h.treasuryClient.ListTaxCodes(r.Context(), tid.String())
	if err != nil {
		h.log.Error("list tax codes failed", zap.Error(err))
		jsonError(w, "failed to load tax codes", http.StatusBadGateway)
		return
	}
	if codes == nil {
		codes = []treasury.TaxCodeResponse{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tax_codes": codes, "total": len(codes)})
}

// ListC2BCandidates handles GET /{tenant}/pos/c2b/payments — proxies the treasury C2B inbox query so
// the cashier can find an unreconciled till/paybill payment to bind to the open sale.
func (h *PaymentHandler) ListC2BCandidates(w http.ResponseWriter, r *http.Request) {
	tid, terr := parseTenantUUID(r)
	if terr != nil || h.treasuryClient == nil {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}
	resp, err := h.treasuryClient.ListC2BCandidates(r.Context(), tid.String(), r.URL.RawQuery)
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
	tid, terr := parseTenantUUID(r)
	if terr != nil || h.treasuryClient == nil {
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
	resp, err := h.treasuryClient.ClaimC2BPayment(r.Context(), tid.String(), transID, body.POSOrderID)
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

// ListBanks proxies the Paystack bank list for a country via treasury S2S so the receipt
// payment-display bank can be verified before saving (accurate "how to pay" on receipts).
func (h *PaymentHandler) ListBanks(w http.ResponseWriter, r *http.Request) {
	if h.treasuryClient == nil {
		http.Error(w, `{"error":"bank verification not configured"}`, http.StatusServiceUnavailable)
		return
	}
	tenantSlug := chi.URLParam(r, "tenantID")
	raw, err := h.treasuryClient.ListBanks(r.Context(), tenantSlug, chi.URLParam(r, "country"))
	if err != nil {
		http.Error(w, `{"error":"failed to load banks"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// ResolveBankAccount proxies Paystack account name-enquiry via treasury S2S.
func (h *PaymentHandler) ResolveBankAccount(w http.ResponseWriter, r *http.Request) {
	if h.treasuryClient == nil {
		http.Error(w, `{"error":"bank verification not configured"}`, http.StatusServiceUnavailable)
		return
	}
	tenantSlug := chi.URLParam(r, "tenantID")
	raw, err := h.treasuryClient.ResolveAccount(r.Context(), tenantSlug, r.URL.Query().Get("account_number"), r.URL.Query().Get("bank_code"))
	if err != nil {
		http.Error(w, `{"error":"failed to verify account"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// S2SRecheckOrderCompletion handles POST /api/v1/s2s/{tenant}/orders/{orderNumber}/recheck-completion.
// Internal ops recovery tool (INTERNAL_SERVICE_KEY-gated, see router.go's S2S group): re-evaluates
// whether an order's completed payments now cover its total and, if so, drives it through the
// real completion path (status transition, pos.sale.finalized publish for treasury GL/inventory
// backflush/eTIMS, receipt enqueue, commissions, table release) — the same thing a normal payment
// submission does. Needed when an order's total was corrected out-of-band (e.g. a data fix for a
// stuck order) after the completion check already failed once against a wrong total, since nothing
// else re-triggers it for an order that's already at $0 outstanding.
func (h *PaymentHandler) S2SRecheckOrderCompletion(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	orderNumber := chi.URLParam(r, "orderNumber")
	order, err := h.client.POSOrder.Query().
		Where(posorder.TenantID(tid), posorder.OrderNumber(orderNumber)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	if err := h.paymentSvc.RecheckCompletion(r.Context(), tid, order.ID); err != nil {
		h.log.Error("s2s recheck-completion failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	updated, err := h.client.POSOrder.Get(r.Context(), order.ID)
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{
		"order_id":     updated.ID,
		"order_number": updated.OrderNumber,
		"status":       updated.Status,
		"paid_total":   updated.PaidTotal,
		"total_amount": updated.TotalAmount,
	})
}
