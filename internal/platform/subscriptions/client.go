package subscriptions

import (
	"bytes"
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

// Entitlements is the subscription snapshot embedded into terminal (PIN) JWTs so that
// PIN sessions carry the same feature/limit gating as SSO sessions. Demo bypass and
// service-charge are surfaced so the gate can exempt them.
type Entitlements struct {
	Features     []string       `json:"features"`
	Limits       map[string]int `json:"limits"`
	Status       string         `json:"status"`
	BillingMode  string         `json:"billing_mode"`
	PlanCode     string         `json:"plan_code"`
	IsDemoBypass bool           `json:"is_demo_bypass"`
}

// GetEntitlements fetches the tenant's full subscription snapshot (features, limits,
// status, billing_mode) from the S2S endpoint. Returns nil on any error so callers can
// fall back gracefully (a PIN session then relies on slug-based demo/owner detection).
func (c *Client) GetEntitlements(ctx context.Context, tenantID string) *Entitlements {
	if c.cfg.ServiceURL == "" {
		return nil
	}
	url := fmt.Sprintf("%s/api/v1/tenants/%s/subscription", c.cfg.ServiceURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var e Entitlements
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return nil
	}
	return &e
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

// UsageDecision is the outcome of reporting a metered usage event.
type UsageDecision struct {
	// Allowed is true when the event is within limit OR soft-capped (overage opted-in).
	Allowed bool
	// Status is the raw HTTP status from subscriptions-api (402 when hard-blocked).
	Status int
	// Body carries the structured limit-reached fields when Allowed is false.
	Body map[string]any
}

// ReportUsage records a metered usage event (e.g. metric="orders", "transactions") and
// returns the limit decision. subscriptions-api atomically increments the tenant's counter
// and either allows the event (within limit or opted-in overage) or returns 402 with the
// structured limit body. Fails OPEN (Allowed=true) on any network/parse error so a
// subscriptions-api outage never blocks core POS operations. Tenant is resolved by
// subscriptions-api from the X-Tenant-ID header under API-key auth.
func (c *Client) ReportUsage(ctx context.Context, tenantID, metric, serviceName string, value float64) UsageDecision {
	if c.cfg.ServiceURL == "" || c.cfg.APIKey == "" {
		return UsageDecision{Allowed: true}
	}
	payload, _ := json.Marshal(map[string]any{
		"metric_type":  metric,
		"service_name": serviceName,
		"value":        value,
	})
	url := fmt.Sprintf("%s/api/v1/usage/report", c.cfg.ServiceURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return UsageDecision{Allowed: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.cfg.APIKey)
	req.Header.Set("X-Tenant-ID", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return UsageDecision{Allowed: true} // fail open
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return UsageDecision{Allowed: false, Status: resp.StatusCode, Body: body}
	}
	return UsageDecision{Allowed: true, Status: resp.StatusCode}
}
