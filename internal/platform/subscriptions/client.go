package subscriptions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"go.uber.org/zap"
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

// Client interacts with the subscriptions service, built on the shared
// github.com/Bengo-Hub/shared-service-client transport (circuit breaker + bounded retry +
// tracing) instead of a bare http.Client. The retry budget is kept close to RequestTimeout (a
// single quick retry, not the shared-service-client default 30s budget) since this client sits
// on hot paths (per-order usage reporting, PIN-session entitlement lookups) that must fail open
// fast on a subscriptions-api outage; the circuit breaker still protects a sustained outage from
// making every subsequent call pay a fresh timeout.
type Client struct {
	cfg Config
	sc  *serviceclient.Client
	// httpc is used only by ReportUsage: subscriptions-api's usage-report endpoint returns a
	// meaningful 429 as part of its usage-DECISION contract (distinct from a generic upstream
	// rate-limit), but shared-service-client's transport always treats HTTP 429 as a retryable
	// transport error and never surfaces it as a normal Response — which would silently turn a
	// real "usage limit exceeded" decision into a fail-open after burning the retry budget. A
	// direct client here preserves that one caller-visible status code exactly, mirroring the
	// same dual-transport pattern already used in erp-api's platform/treasury client.go.
	httpc *http.Client
}

// NewClient creates a new subscriptions service client.
func NewClient(cfg Config, log *zap.Logger) *Client {
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	scCfg := serviceclient.DefaultConfig(strings.TrimRight(cfg.ServiceURL, "/"), "subscriptions-api", log)
	scCfg.Timeout = timeout
	scCfg.InitialInterval = 100 * time.Millisecond
	scCfg.MaxInterval = timeout
	scCfg.MaxElapsedTime = timeout
	return &Client{cfg: cfg, sc: serviceclient.New(scCfg), httpc: &http.Client{Timeout: timeout}}
}

func (c *Client) authHeaders(tenantID, bearerToken string) map[string]string {
	headers := map[string]string{"X-Tenant-ID": tenantID}
	if c.cfg.APIKey != "" {
		headers["X-API-Key"] = c.cfg.APIKey
	} else if bearerToken != "" {
		headers["Authorization"] = "Bearer " + bearerToken
	}
	return headers
}

// IsSubscriptionActive returns true if the tenant has an active subscription.
// Uses the S2S tenant-scoped endpoint so callers don't need to pass a user JWT.
// Fails open (returns true) on network errors to avoid blocking service on subscriptions-api downtime.
func (c *Client) IsSubscriptionActive(ctx context.Context, tenantID, tenantSlug, bearerToken string) bool {
	// Use the S2S tenant-scoped path — subscriptions-api resolves tenant from URL param,
	// not from JWT claims, so API-key auth works correctly without a user JWT in context.
	resp, err := c.sc.Get(ctx, fmt.Sprintf("/api/v1/tenants/%s/subscription", tenantID), c.authHeaders(tenantID, bearerToken))
	if err != nil {
		return true // fail open
	}
	if resp.StatusCode == 404 {
		return false
	}
	if resp.StatusCode != 200 {
		return true // fail open
	}
	var sub SubscriptionStatus
	if err := resp.DecodeJSON(&sub); err != nil {
		return true // fail open
	}
	return sub.IsActive()
}

// planLimitsResponse is the partial shape of GET /api/v1/tenants/{id}/subscription
// used to read the tenant's plan limits (keyed by max_* keys, e.g. "max_rooms").
type planLimitsResponse struct {
	Limits map[string]int `json:"limits"`
}

// Entitlements is the canonical tenant subscription snapshot returned by subscriptions-api's
// GET /api/v1/tenants/{id}/subscription (mirrors treasury-api's platform/subscriptions.
// Entitlements — the two should converge on one shared shape). Embedded into terminal (PIN)
// JWTs so that PIN sessions carry the same feature/limit gating as SSO sessions. Demo bypass and
// service-charge are surfaced so the gate can exempt them.
type Entitlements struct {
	Features     []string `json:"features"`
	Status       string   `json:"status"`
	BillingMode  string   `json:"billing_mode"`
	IsDemoBypass bool     `json:"is_demo_bypass"`
	// ActiveProducts mirrors treasury-api's Entitlements field of the same name — the
	// per-product self-activation list. pos-api doesn't currently gate on it, but decoding it
	// keeps this struct the full canonical shape rather than a partial subset.
	ActiveProducts    []string       `json:"active_products"`
	Limits            map[string]int `json:"limits"`
	PlanCode          string         `json:"plan_code"`
	// TierOrder/AllowOverage/CurrentPeriodEnd/IsPerpetual/Exempt mirror the fields auth-api's
	// EnrichTokenWithSubscription maps onto an SSO JWT (sub_tier/sub_allow_overage/sub_expires/
	// sub_exempt). Without these a terminal (PIN) session can't be told apart from a lower-tier
	// or non-exempt one for tier-rank gates, overage soft-cap, grace-period, or an explicit
	// platform exemption — it would silently fall back to "no tier / no overage / no grace".
	TierOrder        int    `json:"tier_order"`
	AllowOverage     bool   `json:"allow_overage"`
	CurrentPeriodEnd string `json:"current_period_end"`
	IsPerpetual      bool   `json:"is_perpetual"`
	Exempt           bool   `json:"exempt"`
}

// GetEntitlements fetches the tenant's full subscription snapshot (features, limits,
// status, billing_mode) from the S2S endpoint. Returns nil on any error so callers can
// fall back gracefully (a PIN session then relies on slug-based demo/owner detection).
func (c *Client) GetEntitlements(ctx context.Context, tenantID string) *Entitlements {
	if c.cfg.ServiceURL == "" {
		return nil
	}
	resp, err := c.sc.Get(ctx, fmt.Sprintf("/api/v1/tenants/%s/subscription", tenantID), c.authHeaders(tenantID, ""))
	if err != nil || resp.StatusCode != 200 {
		return nil
	}
	var e Entitlements
	if err := resp.DecodeJSON(&e); err != nil {
		return nil
	}
	return &e
}

// GetLimit returns the numeric plan limit for the given limit key (e.g. "max_rooms",
// "max_conference_events"). ok is false when the limit is unset, unlimited (<= 0),
// or subscriptions-api is unreachable — callers MUST fail open (allow the action) in
// that case so a subscriptions-api outage never blocks core operations.
func (c *Client) GetLimit(ctx context.Context, tenantID, limitKey string) (limit int, ok bool) {
	resp, err := c.sc.Get(ctx, fmt.Sprintf("/api/v1/tenants/%s/subscription", tenantID), c.authHeaders(tenantID, ""))
	if err != nil || resp.StatusCode != 200 {
		return 0, false
	}
	var body planLimitsResponse
	if err := resp.DecodeJSON(&body); err != nil {
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
	url := fmt.Sprintf("%s/api/v1/usage/report", strings.TrimRight(c.cfg.ServiceURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return UsageDecision{Allowed: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.cfg.APIKey)
	req.Header.Set("X-Tenant-ID", tenantID)

	resp, err := c.httpc.Do(req)
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
