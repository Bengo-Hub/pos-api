// Package inventory provides an S2S client for inventory-api consumption backflush.
// All calls use the shared INTERNAL_SERVICE_KEY via X-API-Key header.
package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client is a thin S2S client for inventory-api stock operations.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates an inventory S2S client.
func NewClient(serviceURL, internalServiceKey string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		baseURL: serviceURL,
		apiKey:  internalServiceKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// ConsumptionItem represents one line item in a consumption request.
type ConsumptionItem struct {
	SKU      string  `json:"SKU"`
	Quantity float64 `json:"Quantity"`
}

// ConsumptionRequest is the body for POST /v1/{tenant}/inventory/consumption.
type ConsumptionRequest struct {
	OrderID string            `json:"OrderID"`
	Items   []ConsumptionItem `json:"Items"`
}

// RecordConsumption calls inventory-api to backflush stock for a completed POS order.
// Non-fatal: callers should log and optionally publish a retry event on error.
func (c *Client) RecordConsumption(ctx context.Context, tenantID string, req ConsumptionRequest) error {
	if len(req.Items) == 0 {
		return nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("inventory.Client.RecordConsumption: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1/%s/inventory/consumption", c.baseURL, tenantID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("inventory.Client.RecordConsumption: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("inventory.Client.RecordConsumption: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("inventory.Client.RecordConsumption: status %d", resp.StatusCode)
	}
	return nil
}
