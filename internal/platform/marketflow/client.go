package marketflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Client is an S2S client for the MarketFlow CRM API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	log        *zap.Logger
}

// NewClient creates a new MarketFlow S2S client.
// baseURL is the MARKETFLOW_API_URL env var value (e.g. https://marketflow-api.example.com).
// apiKey is the shared INTERNAL_SERVICE_KEY.
func NewClient(baseURL, apiKey string, log *zap.Logger) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		log:        log.Named("marketflow-client"),
	}
}

// Enabled returns false if the client was not configured (no base URL).
func (c *Client) Enabled() bool {
	return c.baseURL != ""
}

type upsertContactRequest struct {
	TenantID  string `json:"tenant_id"`
	Phone     string `json:"phone"`
	Email     string `json:"email,omitempty"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type upsertContactResponse struct {
	ID string `json:"id"`
}

// UpsertContactByPhone creates or returns an existing MarketFlow contact for the given phone.
// Returns uuid.Nil on any error or if the client is disabled — callers should handle gracefully.
func (c *Client) UpsertContactByPhone(ctx context.Context, tenantID uuid.UUID, phone, fullName string) uuid.UUID {
	return c.UpsertContact(ctx, tenantID, phone, "", fullName)
}

// UpsertContact creates or returns an existing MarketFlow contact keyed by phone/email (the CRM
// dedups on either). Email is optional; MarketFlow remains the customer PII source of truth.
func (c *Client) UpsertContact(ctx context.Context, tenantID uuid.UUID, phone, email, fullName string) uuid.UUID {
	if !c.Enabled() {
		return uuid.Nil
	}

	firstName, lastName := splitName(fullName)
	payload, _ := json.Marshal(upsertContactRequest{
		TenantID:  tenantID.String(),
		Phone:     phone,
		Email:     email,
		FirstName: firstName,
		LastName:  lastName,
	})

	url := fmt.Sprintf("%s/api/v1/internal/contacts/upsert", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		c.log.Warn("marketflow: build upsert request failed", zap.Error(err))
		return uuid.Nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.log.Warn("marketflow: upsert contact request failed", zap.Error(err))
		return uuid.Nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Warn("marketflow: upsert contact unexpected status", zap.Int("status", resp.StatusCode))
		return uuid.Nil
	}

	var result upsertContactResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		c.log.Warn("marketflow: decode upsert response failed", zap.Error(err))
		return uuid.Nil
	}

	id, err := uuid.Parse(result.ID)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// splitName splits "First Last" into (firstName, lastName).
// Single words are treated as firstName with empty lastName.
func splitName(fullName string) (string, string) {
	for i, ch := range fullName {
		if ch == ' ' {
			return fullName[:i], fullName[i+1:]
		}
	}
	return fullName, ""
}

// ContactSummary is the minimal CRM contact profile returned by S2S search.
type ContactSummary struct {
	ID        string `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	CreatedAt string `json:"created_at"`
}

// SearchContacts finds the tenant's CRM contacts whose name, email or phone contains `q`
// (case-insensitive). Phone callers should pass national subscriber digits (last 9) so every
// stored format matches. Returns nil on any error or when the client is disabled — the customer
// picker degrades to loyalty-only results rather than failing.
func (c *Client) SearchContacts(ctx context.Context, tenantID uuid.UUID, q string, limit int) []ContactSummary {
	if len(q) < 2 {
		return nil
	}
	rows, _ := c.ListContacts(ctx, tenantID, q, limit, 0)
	return rows
}

// ListContacts lists/searches the tenant's CRM contact directory (q == "" lists all, newest
// first) with offset pagination, returning the page and the total match count. Returns
// (nil, 0) on any error or when the client is disabled.
func (c *Client) ListContacts(ctx context.Context, tenantID uuid.UUID, q string, limit, offset int) ([]ContactSummary, int) {
	if !c.Enabled() {
		return nil, 0
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	url := fmt.Sprintf("%s/api/v1/internal/contacts/search?tenant_id=%s&q=%s&limit=%d&offset=%d",
		c.baseURL, tenantID.String(), neturl.QueryEscape(q), limit, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.log.Warn("marketflow: contact search request failed", zap.Error(err))
		return nil, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.log.Warn("marketflow: contact search unexpected status", zap.Int("status", resp.StatusCode))
		return nil, 0
	}

	var result struct {
		Data  []ContactSummary `json:"data"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		c.log.Warn("marketflow: decode contact search response failed", zap.Error(err))
		return nil, 0
	}
	return result.Data, result.Total
}
