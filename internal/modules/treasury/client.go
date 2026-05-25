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
	SourceService string  `json:"source_service"` // always "pos"
	ReferenceID   string  `json:"reference_id"`   // pos_order UUID
	ReferenceType string  `json:"reference_type"` // "pos_order"
	Amount        float64 `json:"amount"`
	Currency      string  `json:"currency"`
	PaymentMethod string  `json:"payment_method"` // "cash", "pending", etc.
	Description   string  `json:"description,omitempty"`
	CustomerEmail string  `json:"customer_email,omitempty"`
	CustomerPhone string  `json:"customer_phone,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// IntentResponse is the response from POST /api/v1/{tenant}/payments/intents.
type IntentResponse struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PaymentMethod string `json:"payment_method"`
	Amount        float64 `json:"amount"`
	Currency      string `json:"currency"`
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
	Status             string `json:"status"`
	CheckoutRequestID  string `json:"checkout_request_id,omitempty"`
	AuthorizationURL   string `json:"authorization_url,omitempty"`
	Reference          string `json:"reference,omitempty"`
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
type RefundResponse struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// CreateRefund calls POST /api/v1/s2s/{tenantSlug}/refunds on treasury-api.
func (c *Client) CreateRefund(ctx context.Context, tenantSlug string, req RefundRequest) (*RefundResponse, error) {
	url := fmt.Sprintf("%s/api/v1/s2s/%s/refunds", c.baseURL, tenantSlug)
	return doRequest[RefundResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, req)
}

// CreateIntent calls POST /api/v1/{tenantSlug}/payments/intents on treasury-api.
func (c *Client) CreateIntent(ctx context.Context, tenantSlug string, req CreateIntentRequest) (*IntentResponse, error) {
	url := fmt.Sprintf("%s/api/v1/%s/payments/intents", c.baseURL, tenantSlug)
	return doRequest[IntentResponse](ctx, c.httpClient, http.MethodPost, url, c.apiKey, req)
}

// InitiateIntent calls POST /api/v1/{tenantSlug}/payments/intents/{intentID}/initiate on treasury-api.
// This is the handler behind the initiateUrl that treasury-ui invokes when the user picks a payment gateway.
func (c *Client) InitiateIntent(ctx context.Context, tenantSlug, intentID string, req InitiateRequest) (*InitiateResponse, error) {
	url := fmt.Sprintf("%s/api/v1/%s/payments/intents/%s/initiate", c.baseURL, tenantSlug, intentID)
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
