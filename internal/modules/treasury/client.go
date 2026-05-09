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

// doRequest performs an authenticated JSON request and decodes the response.
func doRequest[T any](ctx context.Context, client *http.Client, method, url, apiKey string, body any) (*T, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("treasury: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("treasury: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
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
