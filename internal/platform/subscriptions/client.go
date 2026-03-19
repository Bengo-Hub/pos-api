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
// Fails open (returns true) on network errors to avoid blocking service on subscriptions-api downtime.
func (c *Client) IsSubscriptionActive(ctx context.Context, tenantID, tenantSlug, bearerToken string) bool {
	url := fmt.Sprintf("%s/api/v1/subscription", c.cfg.ServiceURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return true // fail open
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Tenant-Slug", tenantSlug)
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
