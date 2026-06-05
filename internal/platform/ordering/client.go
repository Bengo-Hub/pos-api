// Package ordering provides a thin S2S client for the ordering-backend.
//
// ordering-backend OWNS customer orders, delivery tracking, and the rider-assignment
// flow (see feedback_service_data_ownership). pos-api never mutates an ordering order
// directly nor calls logistics /tasks assign — it DELEGATES rider assignment to the
// canonical ordering-backend admin endpoint, which orchestrates the logistics task.
//
// All calls use the shared INTERNAL_SERVICE_KEY via the X-API-Key header
// (see feedback_s2s_service_key — single shared key, never per-service variants).
package ordering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
)

// Client is a thin S2S client for ordering-backend admin operations.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	log        *zap.Logger
}

// NewClient creates a new ordering-backend S2S client.
// baseURL is ORDERING_SERVICE_URL; apiKey is the shared INTERNAL_SERVICE_KEY.
func NewClient(baseURL, apiKey string, timeout time.Duration, log *zap.Logger) *Client {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: timeout},
		log:        log.Named("ordering-client"),
	}
}

// Enabled reports whether the client was configured with a base URL.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// AssignRider delegates rider assignment for a delivery order to the canonical
// ordering-backend endpoint:
//
//	PUT /api/v1/{tenantSlug}/admin/orders/{externalOrderID}/rider  body {"rider_id":"..."}
//
// ordering-backend owns the order + the downstream logistics task creation; pos-api
// must NOT call logistics /tasks assign directly.
func (c *Client) AssignRider(ctx context.Context, tenantSlug, externalOrderID, riderID string) error {
	if !c.Enabled() {
		return fmt.Errorf("ordering: client not configured (ORDERING_SERVICE_URL unset)")
	}

	endpoint := fmt.Sprintf("%s/api/v1/%s/admin/orders/%s/rider",
		c.baseURL, url.PathEscape(tenantSlug), url.PathEscape(externalOrderID))

	payload, err := json.Marshal(map[string]string{"rider_id": riderID})
	if err != nil {
		return fmt.Errorf("ordering: marshal assign-rider body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("ordering: build assign-rider request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ordering: assign-rider request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		c.log.Warn("ordering: assign-rider upstream error",
			zap.Int("status", resp.StatusCode),
			zap.String("external_order_id", externalOrderID),
		)
		return fmt.Errorf("ordering: upstream error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
