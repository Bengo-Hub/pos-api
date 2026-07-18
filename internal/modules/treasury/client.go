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
	"net/url"
	"strconv"
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

// EtimsFiscal is treasury's fiscalisation evidence for a transmitted document — the data
// the "KRA TIMS Details" block on ETR receipts prints.
type EtimsFiscal struct {
	KraPin        string `json:"kra_pin"`
	BranchID      string `json:"branch_id"`
	DeviceSerial  string `json:"device_serial"`
	ReceiptNo     string `json:"receipt_no"`
	CuInvoiceNo   string `json:"cu_invoice_no"`
	InternalData  string `json:"internal_data"`
	Signature     string `json:"signature"`
	InvcNo        int64  `json:"invc_no"`
	QRURL         string `json:"qr_url"`
	TransmittedAt string `json:"transmitted_at"`
}

// GetEtimsFiscal fetches the fiscal evidence for a transmitted POS sale by its order id —
// the PULL/backfill path used when the etims.invoice_transmitted event was missed, so
// receipts never silently print without their fiscal identity. 404 (not fiscalised yet)
// returns nil, nil.
func (c *Client) GetEtimsFiscal(ctx context.Context, tenantSlug, orderID string) (*EtimsFiscal, error) {
	fullURL := fmt.Sprintf("%s/api/v1/s2s/%s/etims-fiscal/pos_sale/%s", c.baseURL, tenantSlug, orderID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("treasury: get etims fiscal: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("treasury: get etims fiscal: status %d", resp.StatusCode)
	}
	var out EtimsFiscal
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("treasury: decode etims fiscal: %w", err)
	}
	return &out, nil
}

// ListBanks proxies the treasury S2S Paystack bank list for a country (raw JSON passthrough).
func (c *Client) ListBanks(ctx context.Context, tenantSlug, country string) (json.RawMessage, error) {
	if country == "" {
		country = "kenya"
	}
	return c.getRaw(ctx, fmt.Sprintf("%s/api/v1/s2s/%s/gateways/banks/%s", c.baseURL, tenantSlug, country))
}

// ResolveAccount proxies the treasury S2S Paystack account name-enquiry (raw JSON passthrough).
func (c *Client) ResolveAccount(ctx context.Context, tenantSlug, accountNumber, bankCode string) (json.RawMessage, error) {
	return c.getRaw(ctx, fmt.Sprintf("%s/api/v1/s2s/%s/gateways/resolve-account?account_number=%s&bank_code=%s",
		c.baseURL, tenantSlug, url.QueryEscape(accountNumber), url.QueryEscape(bankCode)))
}

// ListQuotations proxies the treasury S2S quotation list (raw JSON passthrough) for the POS
// "Quotation" transactions tab. Supports status/from/to/limit/page query params.
func (c *Client) ListQuotations(ctx context.Context, tenantSlug, rawQuery string) (json.RawMessage, error) {
	u := fmt.Sprintf("%s/api/v1/s2s/%s/quotations", c.baseURL, tenantSlug)
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return c.getRaw(ctx, u)
}

// GetQuotation proxies the treasury S2S quotation detail (raw JSON passthrough) — pos
// surfaces render the SAME document treasury-ui manages.
func (c *Client) GetQuotation(ctx context.Context, tenantSlug, quotationID string) (json.RawMessage, error) {
	return c.getRaw(ctx, fmt.Sprintf("%s/api/v1/s2s/%s/quotations/%s", c.baseURL, tenantSlug, url.PathEscape(quotationID)))
}

// UpdateQuotation proxies a PUT/PATCH quotation update (raw body passthrough) to treasury's
// S2S UpdateQuotation — the exact handler treasury-ui edits through, so validation and
// draft-only rules stay identical.
func (c *Client) UpdateQuotation(ctx context.Context, tenantSlug, quotationID, method string, body []byte) (json.RawMessage, error) {
	if method != http.MethodPatch {
		method = http.MethodPut
	}
	return c.rawRequest(ctx, method, fmt.Sprintf("%s/api/v1/s2s/%s/quotations/%s", c.baseURL, tenantSlug, url.PathEscape(quotationID)), body)
}

// QuotationAction proxies a quotation lifecycle action (send | accept | decline | cancel)
// to treasury's S2S routes — same handlers treasury-ui's action menu calls.
func (c *Client) QuotationAction(ctx context.Context, tenantSlug, quotationID, action string) (json.RawMessage, error) {
	return c.rawRequest(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/s2s/%s/quotations/%s/%s", c.baseURL, tenantSlug, url.PathEscape(quotationID), url.PathEscape(action)), nil)
}

// rawRequest performs an arbitrary-method S2S call with an optional raw JSON body and
// returns the raw response (passthrough proxying — pos never reshapes treasury documents).
func (c *Client) rawRequest(ctx context.Context, method, fullURL string, body []byte) (json.RawMessage, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("treasury: %s %s status %d: %s", method, fullURL, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

func (c *Client) getRaw(ctx context.Context, fullURL string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("treasury: %s status %d", fullURL, resp.StatusCode)
	}
	return json.RawMessage(body), nil
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

// RefundRequest is the body for POST /api/v1/s2s/{tenant}/refunds.
// Treasury reverses revenue+VAT, credits the chosen channel account, reverses COGS, and is
// idempotent on the return id (reference_id). The tax_amount/cost fields let treasury reverse the
// exact VAT and Cost-of-Goods-Sold posted at sale time (cost triggers the restock/COGS reversal).
type RefundRequest struct {
	SourceService string `json:"source_service"` // "pos"
	ReferenceID   string `json:"reference_id"`   // pos_return UUID
	ReferenceType string `json:"reference_type"` // "pos_return"
	// Reference is the human return number (RET-…) treasury shows on customer statements
	// instead of the raw return UUID.
	Reference          string  `json:"reference,omitempty"`
	OriginalIntentID   string  `json:"original_intent_id,omitempty"` // original payment intent if known
	Amount             float64 `json:"amount"`
	TaxAmount          float64 `json:"tax_amount,omitempty"` // VAT portion of the refunded lines (reversed)
	Cost               float64 `json:"cost,omitempty"`       // COGS of returned goods → triggers restock/COGS reversal
	Currency           string  `json:"currency"`
	Reason             string  `json:"reason"`
	RefundChannel      string  `json:"refund_channel,omitempty"`      // cash|mpesa|bank|cheque|store_credit|offset_invoice
	CrmContactID       string  `json:"crm_contact_id,omitempty"`      // CRM contact of the original buyer, when known
	CustomerIdentifier string  `json:"customer_identifier,omitempty"` // phone fallback so store-credit nets a phone-keyed AR row
	CustomerName       string  `json:"customer_name,omitempty"`
	CustomerEmail      string  `json:"customer_email,omitempty"`
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
// idempotencyKey (the pos_return id) is sent as the Idempotency-Key header so a network retry
// can't double-refund; treasury is also idempotent on reference_id. Pass "" to skip the header.
func (c *Client) CreateRefund(ctx context.Context, tenantSlug, idempotencyKey string, req RefundRequest) (*RefundResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/refunds", c.baseURL, tenantSlug)
	extraHeaders := map[string]string{}
	if idempotencyKey != "" {
		extraHeaders["Idempotency-Key"] = idempotencyKey
	}
	return doRequestWithHeaders[RefundResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, extraHeaders, req)
}

// CreditSaleRequest is the body for POST /api/v1/s2s/{tenant}/ar/credit-sale.
type CreditSaleRequest struct {
	// CrmContactID is the canonical AR key (the marketflow CRM contact of the selected customer).
	// customer_identifier (phone) is a fallback so treasury can resolve/backfill a legacy phone-keyed
	// row; sending both makes the credit sale, its returns and its opening balance net on ONE row.
	CrmContactID       string `json:"crm_contact_id,omitempty"`
	CustomerIdentifier string `json:"customer_identifier,omitempty"`
	CustomerName       string `json:"customer_name,omitempty"`
	POSOrderID         string `json:"pos_order_id,omitempty"`
	// Reference is the human invoice number (POS-…) treasury shows on customer statements
	// instead of the raw order UUID.
	Reference string  `json:"reference,omitempty"`
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	// UserID is the cashier/manager (global auth-service user id) who rang the sale — treasury
	// records them as the journal entry's creator ("Recorded By").
	UserID string `json:"user_id,omitempty"`
}

// CreditSaleResponse is the treasury customer-balance row returned after posting a credit sale.
type CreditSaleResponse struct {
	ID         string `json:"id"`
	BalanceDue string `json:"balance_due"`
	Currency   string `json:"currency"`
	// CreditPeriodDays is the customer's configured payment period — pos-api uses it to stamp
	// the order's payment_due_date so the All-Sales "Overdue" filter can find late credit sales.
	CreditPeriodDays *int `json:"credit_period_days,omitempty"`
}

// RecordCreditSale posts a POS on-account ("credit sale") charge to the customer's AR balance in
// treasury. Treasury enforces the customer's credit limit and returns an error if it would be exceeded.
func (c *Client) RecordCreditSale(ctx context.Context, tenantSlug string, req CreditSaleRequest) (*CreditSaleResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/ar/credit-sale", c.baseURL, tenantSlug)
	return doRequest[CreditSaleResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, req)
}

// CreditTermsResponse is a treasury customer-balance row scoped to what the POS credit card
// needs: balance due + configured credit limit / payment period. Decimal fields arrive as
// quoted strings (treasury serializes decimal.Decimal that way).
type CreditTermsResponse struct {
	CrmContactID     string `json:"crm_contact_id,omitempty"`
	CustomerName     string `json:"customer_name,omitempty"`
	BalanceDue       string `json:"balance_due"`
	CreditLimit      string `json:"credit_limit,omitempty"`
	CreditPeriodDays *int   `json:"credit_period_days,omitempty"`
	Currency         string `json:"currency"`
}

// GetCreditTerms fetches a customer's AR balance + credit terms from treasury over S2S.
// contactIDOrIdentifier is the CRM contact UUID (preferred) or the phone identifier.
func (c *Client) GetCreditTerms(ctx context.Context, tenantSlug, contactIDOrIdentifier string) (*CreditTermsResponse, error) {
	u := fmt.Sprintf("%s/api/v1/s2s/%s/ar/customers/%s/credit-terms", c.baseURL, tenantSlug, url.PathEscape(contactIDOrIdentifier))
	return doRequest[CreditTermsResponse](ctx, c.httpClient, http.MethodGet, u, c.apiKey, nil)
}

// NOTE: credit terms are WRITTEN only from the treasury Customers page (treasury-ui →
// treasury-api PATCH /ar/customers/{id}/credit-terms). The old S2S SetCreditTerms proxy
// method was removed with the duplicate POS credit-terms editor.

// ARPaymentRequest is the body for POST /api/v1/s2s/{tenant}/ar/customers/{key}/payment —
// settling (part of) a customer's on-account debt collected at the till.
type ARPaymentRequest struct {
	Amount        float64 `json:"amount"`
	PaymentMethod string  `json:"payment_method,omitempty"`
	Reference     string  `json:"reference,omitempty"`
}

// ARPaymentResponse is the updated treasury customer-balance row.
type ARPaymentResponse struct {
	ID         string `json:"id"`
	BalanceDue string `json:"balance_due"`
	Currency   string `json:"currency"`
}

// RecordARPayment posts a customer AR repayment to treasury (decrements balance_due and posts
// the Dr Cash / Cr AR journal there — pos-api must NOT post any GL for a credit settlement,
// or the receipt would double-post). contactIDOrIdentifier follows the credit-terms convention:
// CRM contact UUID preferred, phone identifier fallback.
func (c *Client) RecordARPayment(ctx context.Context, tenantSlug, contactIDOrIdentifier string, req ARPaymentRequest) (*ARPaymentResponse, error) {
	u := fmt.Sprintf("%s/api/v1/s2s/%s/ar/customers/%s/payment", c.baseURL, tenantSlug, url.PathEscape(contactIDOrIdentifier))
	return doRequest[ARPaymentResponse](ctx, c.httpClient, http.MethodPost, u, c.apiKey, req)
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
	CrmContactID  string          `json:"crm_customer_id,omitempty"` // treasury field: link the quotation to the selected CRM customer
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

// flexFloat decodes a JSON number OR a numeric string. Treasury serializes decimal fields (e.g. a
// tax rate) as strings ("16"/"16.00"), which broke the previous float64 field with
// "cannot unmarshal string into ... float64". It re-marshals as a plain JSON number.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(string(b), `"`))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

// TaxCodeResponse is the response from GET /api/v1/s2s/{tenant}/taxes/{code}.
type TaxCodeResponse struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	Rate      flexFloat `json:"rate"`
	TaxType   string    `json:"tax_type"`
	KRACode   string    `json:"kra_code"`
	IsDefault bool      `json:"is_default"`
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

// TaxProfileResponse is the VAT-active switch from GET /api/v1/s2s/{tenant}/tax-profile.
type TaxProfileResponse struct {
	VATActive      bool   `json:"vat_active"`
	VATRegistered  bool   `json:"vat_registered"`
	EtimsActivated bool   `json:"etims_activated"`
	KraPin         string `json:"kra_pin"`
}

// GetTaxProfile fetches the tenant's VAT-active switch from treasury-api S2S.
func (c *Client) GetTaxProfile(ctx context.Context, tenantSlug string) (*TaxProfileResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/tax-profile", c.baseURL, tenantSlug)
	return doRequest[TaxProfileResponse](ctx, c.httpClient, http.MethodGet, url, c.apiKey, nil)
}

// PublicGatewaysResponse is the flat {mpesa,paystack,wallet,cod,complimentary} shape the POS UI consumes.
type PublicGatewaysResponse struct {
	MPesa         bool `json:"mpesa"`
	Paystack      bool `json:"paystack"`
	Wallet        bool `json:"wallet"`
	COD           bool `json:"cod"`
	Complimentary bool `json:"complimentary"`
}

// treasuryGatewaysWire is treasury's ACTUAL response shape for GET /api/v1/pay/{tenant}/gateways:
// the active gateways come back as a STRING ARRAY ({"gateways":["paystack","mpesa","cod"]}). The flat
// boolean fields are kept too so we still work if treasury ever switches to returning booleans.
type treasuryGatewaysWire struct {
	Gateways      []string `json:"gateways"`
	MPesa         bool     `json:"mpesa"`
	Paystack      bool     `json:"paystack"`
	Wallet        bool     `json:"wallet"`
	COD           bool     `json:"cod"`
	Complimentary bool     `json:"complimentary"`
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
	out := &PublicGatewaysResponse{MPesa: wire.MPesa, Paystack: wire.Paystack, Wallet: wire.Wallet, COD: wire.COD, Complimentary: wire.Complimentary}
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
		case "complimentary":
			out.Complimentary = true
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
	ExpenseNumber string         `json:"expense_number,omitempty"` // "Reference No" (treasury autogenerates when empty)
	CategoryID    string         `json:"category_id,omitempty"`
	Description   string         `json:"description"`
	Amount        float64        `json:"amount"`
	TaxAmount     float64        `json:"tax_amount,omitempty"`
	Currency      string         `json:"currency,omitempty"`
	ExpenseDate   string         `json:"expense_date,omitempty"`
	ReceiptURL    string         `json:"receipt_url,omitempty"`
	VendorID      string         `json:"vendor_id,omitempty"`      // "Expense for", when a vendor is selected
	AccountID     string         `json:"account_id,omitempty"`     // Payment Account (chart-of-accounts UUID)
	CostCenterID  string         `json:"cost_center_id,omitempty"` // optional cost-center dimension
	OutletID      string         `json:"outlet_id,omitempty"`
	SubmittedBy   string         `json:"submitted_by,omitempty"`
	SourceService string         `json:"source_service,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"` // payment_method, paid_on, payment_note, expense_for, tax_rate
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

// ListExpenseCategories fetches the tenant's expense categories from treasury over S2S, to populate
// the "Expense Category" dropdown on the POS Add-Expense form. Returns the raw treasury envelope
// ({"categories":[...],"total":n}) for passthrough.
func (c *Client) ListExpenseCategories(ctx context.Context, tenantSlug string) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/expense-categories", c.baseURL, tenantSlug)
	resp, err := doRequest[json.RawMessage](ctx, c.httpClient, http.MethodGet, url, c.apiKey, nil)
	if err != nil {
		return nil, err
	}
	return *resp, nil
}

// ListExpenseAccounts fetches the tenant's chart of accounts from treasury over S2S, to populate
// the "Payment Account" dropdown on the POS Add-Expense form. Returns the raw treasury envelope
// ({"accounts":[...],"total":n}) for passthrough.
func (c *Client) ListExpenseAccounts(ctx context.Context, tenantSlug string) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/accounts", c.baseURL, tenantSlug)
	resp, err := doRequest[json.RawMessage](ctx, c.httpClient, http.MethodGet, url, c.apiKey, nil)
	if err != nil {
		return nil, err
	}
	return *resp, nil
}

// PreviewNextExpenseNumber fetches a live "next number" preview (e.g. "EXP-260710-000123") from
// treasury's document-sequence service, to show as a placeholder on the POS Add-Expense form's
// "Reference No" field — the same server-authoritative sequence treasury-ui's own document forms
// preview. Display-only: never sent as the actual number (pos-api leaves reference_no empty when
// the cashier doesn't override it, letting treasury assign the real number atomically on create).
func (c *Client) PreviewNextExpenseNumber(ctx context.Context, tenantSlug string) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/document-sequences/expense/preview", c.baseURL, tenantSlug)
	resp, err := doRequest[json.RawMessage](ctx, c.httpClient, http.MethodGet, url, c.apiKey, nil)
	if err != nil {
		return nil, err
	}
	return *resp, nil
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
