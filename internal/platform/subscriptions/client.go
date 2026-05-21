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
