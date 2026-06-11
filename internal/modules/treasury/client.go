// Package treasury provides an S2S client for the treasury-api.
// All calls use the shared INTERNAL_SERVICE_KEY via X-API-Key header (never per-service keys).
package treasury

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a thin S2S client for treasury-api payment operations.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a treasury S2S client.
func NewClient(serviceURL, internalServiceKey string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: serviceURL,
		apiKey:  internalServiceKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// CreateIntentRequest is the body for POST /api/v1/{tenant}/payments/intents.
type CreateIntentRequest struct {
	SourceService string         `json:"source_service"` // always "pos"
	ReferenceID   string         `json:"reference_id"`   // pos_order UUID
	ReferenceType string         `json:"reference_type"` // "pos_order"
	Amount        float64        `json:"amount"`
	Currency      string         `json:"currency"`
	PaymentMethod string         `json:"payment_method"` // "cash", "pending", etc.
	Description   string         `json:"description,omitempty"`
	CustomerEmail string         `json:"customer_email,omitempty"`
	CustomerPhone string         `json:"customer_phone,omitempty"`
	OutletID      string         `json:"outlet_id,omitempty"` // outlet context for per-outlet gateway config resolution
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// IntentResponse is the response from POST /api/v1/s2s/{tenant}/payments/intents.
// Amount is string because treasury serializes decimal.Decimal as a quoted JSON string.
// IntentID is the primary field; ID is kept as fallback for non-S2S endpoints that return "id".
type IntentResponse struct {
	IntentID      string `json:"intent_id"` // primary: S2S create endpoint returns this
	ID            string `json:"id"`        // fallback: other treasury endpoints return this
	Status        string `json:"status"`
	PaymentMethod string `json:"payment_method"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	InitiateURL   string `json:"initiate_url,omitempty"` // treasury-built public URL for payment initiation
}

// ResolvedID returns IntentID if non-empty, falling back to ID.
// The S2S create endpoint returns "intent_id"; other endpoints return "id".
func (r *IntentResponse) ResolvedID() string {
	if r.IntentID != "" {
		return r.IntentID
	}
	return r.ID
}

// InitiateRequest is the body for POST /api/v1/{tenant}/payments/intents/{id}/initiate.
type InitiateRequest struct {
	PaymentMethod string         `json:"payment_method"`
	Phone         string         `json:"phone,omitempty"`
	ReturnURL     string         `json:"return_url,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// InitiateResponse is the response from POST /api/v1/{tenant}/payments/intents/{id}/initiate.
type InitiateResponse struct {
	Status            string `json:"status"`
	CheckoutRequestID string `json:"checkout_request_id,omitempty"`
	AuthorizationURL  string `json:"authorization_url,omitempty"`
	Reference         string `json:"reference,omitempty"`
}

// RefundRequest is the body for POST /api/v1/s2s/{tenant}/refunds
type RefundRequest struct {
	SourceService    string  `json:"source_service"`               // "pos"
	ReferenceID      string  `json:"reference_id"`                 // pos_return UUID
	ReferenceType    string  `json:"reference_type"`               // "pos_return"
	OriginalIntentID string  `json:"original_intent_id,omitempty"` // original payment intent if known
	Amount           float64 `json:"amount"`
	Currency         string  `json:"currency"`
	Reason           string  `json:"reason"`
	CustomerEmail    string  `json:"customer_email,omitempty"`
}

// RefundResponse is the response from POST /api/v1/s2s/{tenant}/refunds
// Amount is string because treasury serializes decimal.Decimal as a quoted JSON string.
type RefundResponse struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// CreateRefund calls POST /api/v1/s2s/{tenantSlug}/refunds on treasury-api.
func (c *Client) CreateRefund(ctx context.Context, tenantSlug string, req RefundRequest) (*RefundResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/refunds", c.baseURL, tenantSlug)
	return doRequest[RefundResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, req)
}

// CreditSaleRequest is the body for POST /api/v1/s2s/{tenant}/ar/credit-sale.
type CreditSaleRequest struct {
	CustomerIdentifier string  `json:"customer_identifier,omitempty"`
	CustomerName       string  `json:"customer_name,omitempty"`
	POSOrderID         string  `json:"pos_order_id,omitempty"`
	Amount             float64 `json:"amount"`
	Currency           string  `json:"currency"`
}

// CreditSaleResponse is the treasury customer-balance row returned after posting a credit sale.
type CreditSaleResponse struct {
	ID         string `json:"id"`
	BalanceDue string `json:"balance_due"`
	Currency   string `json:"currency"`
}

// RecordCreditSale posts a POS on-account ("credit sale") charge to the customer's AR balance in
// treasury. Treasury enforces the customer's credit limit and returns an error if it would be exceeded.
func (c *Client) RecordCreditSale(ctx context.Context, tenantSlug string, req CreditSaleRequest) (*CreditSaleResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/ar/credit-sale", c.baseURL, tenantSlug)
	return doRequest[CreditSaleResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, req)
}

// QuotationLine is one line on an S2S quotation create. Quantity/UnitPrice go as JSON numbers;
// treasury's decimal.Decimal fields parse them.
type QuotationLine struct {
	Description string  `json:"description"`
	ItemSKU     string  `json:"item_sku,omitempty"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
}

// CreateQuotationRequest is the body for POST /api/v1/s2s/{tenant}/quotations. Dates are ISO strings
// (treasury parses them via its flexible date decoder).
type CreateQuotationRequest struct {
	CustomerName  string          `json:"customer_name,omitempty"`
	CustomerPhone string          `json:"customer_phone,omitempty"`
	CustomerEmail string          `json:"customer_email,omitempty"`
	QuoteDate     string          `json:"quote_date"`
	ValidUntil    string          `json:"valid_until"`
	Currency      string          `json:"currency,omitempty"`
	Notes         string          `json:"notes,omitempty"`
	ReferenceType string          `json:"reference_type,omitempty"`
	Lines         []QuotationLine `json:"lines"`
}

// QuotationResponse is the treasury quotation returned after creation.
type QuotationResponse struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	TotalAmount string `json:"total_amount"`
}

// CreateQuotation creates a treasury quotation from a pos cart over S2S. Treasury owns quotations;
// pos persists nothing — it keeps only the returned id as a reference.
func (c *Client) CreateQuotation(ctx context.Context, tenantSlug string, req CreateQuotationRequest) (*QuotationResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/quotations", c.baseURL, tenantSlug)
	return doRequest[QuotationResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, req)
}

// InvoiceRef is the minimal treasury invoice returned by GetInvoiceByReference.
type InvoiceRef struct {
	ID            string `json:"id"`
	InvoiceNumber string `json:"invoice_number"`
	Status        string `json:"status"`
}

// GetInvoiceByReference finds a treasury invoice by its (reference_type, reference_id) tuple — used
// to locate the original sale's tax invoice when a return needs a VAT-reversal credit note.
func (c *Client) GetInvoiceByReference(ctx context.Context, tenantSlug, refType, refID string) (*InvoiceRef, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/invoices/by-reference?reference_type=%s&reference_id=%s", c.baseURL, tenantSlug, refType, refID)
	return doRequest[InvoiceRef](ctx, c.httpClient, http.MethodGet, url, c.apiKey, nil)
}

// CreditNoteResponse is the treasury credit note returned after creation.
type CreditNoteResponse struct {
	ID     string `json:"id"`
	Number string `json:"invoice_number"`
	Status string `json:"status"`
}

// CreateCreditNote issues a VAT-reversal sales credit note for an invoice (no body — treasury copies
// the invoice lines and reverses VAT, CN- series). Used when a tax-invoiced sale is returned.
func (c *Client) CreateCreditNote(ctx context.Context, tenantSlug, invoiceID string) (*CreditNoteResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/invoices/%s/create-credit-note", c.baseURL, tenantSlug, invoiceID)
	return doRequest[CreditNoteResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, nil)
}

// CreateIntent calls POST /api/v1/s2s/{tenantSlug}/payments/intents on treasury-api.
// The S2S path requires only X-API-Key (INTERNAL_SERVICE_KEY) — no JWT needed.
// idempotencyKey (e.g. order UUID) is sent as Idempotency-Key header to prevent duplicate intents
// on network retries; pass empty string to skip.
func (c *Client) CreateIntent(ctx context.Context, tenantSlug, idempotencyKey string, req CreateIntentRequest) (*IntentResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/payments/intents", c.baseURL, tenantSlug)
	extraHeaders := map[string]string{}
	if idempotencyKey != "" {
		extraHeaders["Idempotency-Key"] = idempotencyKey
	}
	return doRequestWithHeaders[IntentResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, extraHeaders, req)
}

// InitiateIntent calls POST /api/v1/s2s/{tenantSlug}/payments/intents/{intentID}/initiate on treasury-api.
// Used by the pos-api proxy endpoint — treasury's returned initiate_url should be preferred when available.
func (c *Client) InitiateIntent(ctx context.Context, tenantSlug, intentID string, req InitiateRequest) (*InitiateResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/payments/intents/%s/initiate", c.baseURL, tenantSlug, intentID)
	return doRequest[InitiateResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, req)
}

// TaxCodeResponse is the response from GET /api/v1/s2s/{tenant}/taxes/{code}.
type TaxCodeResponse struct {
	ID        string  `json:"id"`
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	Rate      float64 `json:"rate"`
	TaxType   string  `json:"tax_type"`
	KRACode   string  `json:"kra_code"`
	IsDefault bool    `json:"is_default"`
}

// ListTaxCodesResponse wraps the list response from GET /api/v1/s2s/{tenant}/taxes.
type ListTaxCodesResponse struct {
	TaxCodes []TaxCodeResponse `json:"tax_codes"`
	Total    int               `json:"total"`
}

// GetTaxCode fetches a single TaxCode by code string from treasury-api S2S.
// Returns nil, nil when not found (404).
func (c *Client) GetTaxCode(ctx context.Context, tenantSlug, code string) (*TaxCodeResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/taxes/%s", c.baseURL, tenantSlug, code)
	resp, err := doRequest[TaxCodeResponse](ctx, c.httpClient, http.MethodGet, url, c.apiKey, nil)
	if err != nil {
		// Treat 404 as "not found" — return nil without error so callers can fall back
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return resp, nil
}

// ListTaxCodes fetches all active tax codes for a tenant from treasury-api S2S.
func (c *Client) ListTaxCodes(ctx context.Context, tenantSlug string) ([]TaxCodeResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/taxes", c.baseURL, tenantSlug)
	resp, err := doRequest[ListTaxCodesResponse](ctx, c.httpClient, http.MethodGet, url, c.apiKey, nil)
	if err != nil {
		return nil, err
	}
	return resp.TaxCodes, nil
}

// PublicGatewaysResponse is the flat {mpesa,paystack,wallet,cod} shape the POS UI consumes.
type PublicGatewaysResponse struct {
	MPesa    bool `json:"mpesa"`
	Paystack bool `json:"paystack"`
	Wallet   bool `json:"wallet"`
	COD      bool `json:"cod"`
}

// treasuryGatewaysWire is treasury's ACTUAL response shape for GET /api/v1/pay/{tenant}/gateways:
// the active gateways come back as a STRING ARRAY ({"gateways":["paystack","mpesa","cod"]}). The flat
// boolean fields are kept too so we still work if treasury ever switches to returning booleans.
type treasuryGatewaysWire struct {
	Gateways []string `json:"gateways"`
	MPesa    bool     `json:"mpesa"`
	Paystack bool     `json:"paystack"`
	Wallet   bool     `json:"wallet"`
	COD      bool     `json:"cod"`
}

// GetPublicGateways fetches the active payment gateways for a tenant from the treasury public endpoint
// (no auth on treasury side) and maps them to the flat booleans the POS UI consumes.
//
// BUGFIX: treasury returns {"gateways":["paystack","mpesa","cod"]} (an array), NOT flat booleans.
// Decoding that straight into PublicGatewaysResponse yielded all-false, which silently hid EVERY online
// gateway (M-Pesa, Paystack/Card, Wallet) in the POS payment modal even when the tenant had enabled
// them. We now decode the array (and tolerate the flat form) and derive the flags from it.
func (c *Client) GetPublicGateways(ctx context.Context, tenantSlug string) (*PublicGatewaysResponse, error) {
	url := fmt.Sprintf("%s/api/v1/pay/%s/gateways", c.baseURL, tenantSlug)
	wire, err := doRequest[treasuryGatewaysWire](ctx, c.httpClient, http.MethodGet, url, c.apiKey, nil)
	if err != nil {
		return nil, err
	}
	out := &PublicGatewaysResponse{MPesa: wire.MPesa, Paystack: wire.Paystack, Wallet: wire.Wallet, COD: wire.COD}
	for _, g := range wire.Gateways {
		switch strings.ToLower(strings.TrimSpace(g)) {
		case "mpesa", "mpesa_paybill", "mpesa_till":
			out.MPesa = true
		case "paystack", "card":
			out.Paystack = true
		case "wallet":
			out.Wallet = true
		case "cod":
			out.COD = true
		}
	}
	return out, nil
}

// PayoutRequest is the body for POST /api/v1/{tenant}/payouts/disburse.
type PayoutRequest struct {
	EntityType   string  `json:"entity_type"` // "staff"
	EntityID     string  `json:"entity_id"`   // staff member user_id or UUID
	Amount       float64 `json:"amount"`
	Currency     string  `json:"currency"`
	Reference    string  `json:"reference"`
	Reason       string  `json:"reason"`
	PayoutMethod string  `json:"payout_method"` // "mpesa_b2c" | "paystack_bank" | "cash"
	Recipient    struct {
		Name          string `json:"name"`
		Phone         string `json:"phone,omitempty"`
		AccountNumber string `json:"account_number,omitempty"`
		AccountName   string `json:"account_name,omitempty"`
	} `json:"recipient"`
}

// PayoutResponse is the response from POST /api/v1/{tenant}/payouts/disburse.
type PayoutResponse struct {
	PayoutID  string `json:"payout_id"`
	Reference string `json:"reference"`
	Status    string `json:"status"`
}

// DisbursePayout calls POST /api/v1/{tenantSlug}/payouts/disburse on treasury-api.
func (c *Client) DisbursePayout(ctx context.Context, tenantSlug string, req PayoutRequest) (*PayoutResponse, error) {
	url := fmt.Sprintf("%s/api/v1/%s/payouts/disburse", c.baseURL, tenantSlug)
	return doRequest[PayoutResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, req)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return len(err.Error()) > 0 && (contains(err.Error(), "404") || contains(err.Error(), "not found"))
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// doRequest performs an authenticated JSON request and decodes the response.
func doRequest[T any](ctx context.Context, client *http.Client, method, url, apiKey string, body any) (*T, error) {
	return doRequestWithHeaders[T](ctx, client, method, url, apiKey, nil, body)
}

// doRequestWithHeaders is like doRequest but accepts additional headers.
func doRequestWithHeaders[T any](ctx context.Context, client *http.Client, method, url, apiKey string, extraHeaders map[string]string, body any) (*T, error) {
	var req *http.Request
	var err error
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			return nil, fmt.Errorf("treasury: marshal request: %w", merr)
		}
		req, err = http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, http.NoBody)
	}
	if err != nil {
		return nil, fmt.Errorf("treasury: build request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("treasury: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("treasury: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("treasury: upstream error %d: %s", resp.StatusCode, string(respBody))
	}

	var result T
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("treasury: decode response: %w", err)
	}
	return &result, nil
}

// ExpenseRequest is the body for POST /api/v1/s2s/{tenant}/expenses (register "Add Expense").
type ExpenseRequest struct {
	CategoryID    string         `json:"category_id,omitempty"`
	Description   string         `json:"description"`
	Amount        float64        `json:"amount"`
	TaxAmount     float64        `json:"tax_amount,omitempty"`
	Currency      string         `json:"currency,omitempty"`
	ExpenseDate   string         `json:"expense_date,omitempty"`
	ReceiptURL    string         `json:"receipt_url,omitempty"`
	OutletID      string         `json:"outlet_id,omitempty"`
	SubmittedBy   string         `json:"submitted_by,omitempty"`
	SourceService string         `json:"source_service,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// ExpenseResponse is the created expense returned by treasury (subset).
type ExpenseResponse struct {
	ID            string `json:"id"`
	ExpenseNumber string `json:"expense_number"`
	Status        string `json:"status"`
}

// RecordExpense posts a register expense to treasury over S2S (X-API-Key).
func (c *Client) RecordExpense(ctx context.Context, tenantSlug string, req ExpenseRequest) (*ExpenseResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/expenses", c.baseURL, tenantSlug)
	return doRequest[ExpenseResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, req)
}

// ListC2BCandidates queries unreconciled M-Pesa C2B inbox payments from treasury (raw passthrough of
// the cashier's query params: shortCode, amount, billRef, since, status).
func (c *Client) ListC2BCandidates(ctx context.Context, tenantSlug, rawQuery string) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/c2b/payments", c.baseURL, tenantSlug)
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	resp, err := doRequest[json.RawMessage](ctx, c.httpClient, http.MethodGet, url, c.apiKey, nil)
	if err != nil {
		return nil, err
	}
	return *resp, nil
}

// ClaimC2BPayment atomically binds a C2B payment to a POS order in the treasury inbox.
func (c *Client) ClaimC2BPayment(ctx context.Context, tenantSlug, transID, posOrderID string) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/c2b/payments/%s/claim", c.baseURL, tenantSlug, transID)
	resp, err := doRequest[json.RawMessage](ctx, c.httpClient, http.MethodPost, url, c.apiKey, map[string]string{"pos_order_id": posOrderID})
	if err != nil {
		return nil, err
	}
	return *resp, nil
}
