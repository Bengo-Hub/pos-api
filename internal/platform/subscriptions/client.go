package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Config holds configuration for the subscriptions client.
type Config struct {
	ServiceURL     string
	RequestTimeout time.Duration
	APIKey         string
}

// SubscriptionStatus represents the tenant's subscription response from subscriptions-api.
type SubscriptionStatus struct {
	Status string `json:"status"`
}

// IsActive returns true when the subscription status allows service usage.
func (s *SubscriptionStatus) IsActive() bool {
	return s.Status == "ACTIVE" || s.Status == "TRIAL"
}

// Client interacts with the subscriptions service.
type Client struct {
	cfg  Config
	http *http.Client
}

// NewClient creates a new subscriptions service client.
func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.RequestTimeout}}
}

// IsSubscriptionActive returns true if the tenant has an active subscription.
// Uses the S2S tenant-scoped endpoint so callers don't need to pass a user JWT.
// Fails open (returns true) on network errors to avoid blocking service on subscriptions-api downtime.
func (c *Client) IsSubscriptionActive(ctx context.Context, tenantID, tenantSlug, bearerToken string) bool {
	// Use the S2S tenant-scoped path — subscriptions-api resolves tenant from URL param,
	// not from JWT claims, so API-key auth works correctly without a user JWT in context.
	url := fmt.Sprintf("%s/api/v1/tenants/%s/subscription", c.cfg.ServiceURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return true // fail open
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", c.cfg.APIKey)
	} else if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return true // fail open
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false
	}
	if resp.StatusCode != http.StatusOK {
		return true // fail open
	}
	var sub SubscriptionStatus
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		return true // fail open
	}
	return sub.IsActive()
}

// planLimitsResponse is the partial shape of GET /api/v1/tenants/{id}/subscription
// used to read the tenant's plan limits (keyed by max_* keys, e.g. "max_rooms").
type planLimitsResponse struct {
	Limits map[string]int `json:"limits"`
}

// GetLimit returns the numeric plan limit for the given limit key (e.g. "max_rooms",
// "max_conference_events"). ok is false when the limit is unset, unlimited (<= 0),
// or subscriptions-api is unreachable — callers MUST fail open (allow the action) in
// that case so a subscriptions-api outage never blocks core operations.
func (c *Client) GetLimit(ctx context.Context, tenantID, limitKey string) (limit int, ok bool) {
	url := fmt.Sprintf("%s/api/v1/tenants/%s/subscription", c.cfg.ServiceURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var body planLimitsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, false
	}
	v, exists := body.Limits[limitKey]
	if !exists || v <= 0 { // -1 / 0 / absent → unlimited or not enforced
		return 0, false
	}
	return v, true
}
