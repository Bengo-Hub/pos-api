// Package inventory provides an S2S client for inventory-api consumption backflush.
// All calls use the shared INTERNAL_SERVICE_KEY via X-API-Key header.
package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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
	SKU      string  `json:"sku"`
	Quantity float64 `json:"quantity"`
}

// ConsumptionRequest is the body for POST /v1/{tenant}/inventory/consumption.
// Tags MUST be snake_case to match inventory-api's DTO. In particular order_id:
// Go's case-insensitive JSON match rescued Items/SKU/Quantity, but "OrderID" never
// matched "order_id" (the underscore differs), so inventory saw a nil order_id and
// rejected every POS backflush with 400 MISSING_ORDER_ID — POS sales never deducted stock.
type ConsumptionRequest struct {
	OrderID string            `json:"order_id"`
	Items   []ConsumptionItem `json:"items"`
}

// ItemPrice is the response from GET /v1/{tenant}/inventory/items/{itemID}/price.
type ItemPrice struct {
	ItemID     string  `json:"item_id"`
	UnitPrice  float64 `json:"unit_price"`
	Currency   string  `json:"currency"`
	Quantity   int     `json:"quantity"`
	TotalPrice float64 `json:"total_price"`
	TierName   string  `json:"tier_name"`
}

// brandsBySKUResponse is the inventory-api reply for GET /inventory/brands/by-sku.
type brandsBySKUResponse struct {
	Data map[string]string `json:"data"`
}

// GetBrandsBySKU resolves brand names for a set of SKUs (sku → brand name). SKUs with no
// brand are omitted from the map. Used by the register-details "products sold by brand"
// section; callers treat missing SKUs as "Unbranded". Best-effort: returns an empty map
// (not an error) when inventory is unreachable so the report still renders.
func (c *Client) GetBrandsBySKU(ctx context.Context, tenantID string, skus []string) (map[string]string, error) {
	out := map[string]string{}
	if c == nil || c.baseURL == "" || len(skus) == 0 {
		return out, nil
	}
	reqURL := fmt.Sprintf("%s/v1/%s/inventory/brands/by-sku?skus=%s",
		c.baseURL, tenantID, url.QueryEscape(strings.Join(skus, ",")))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return out, fmt.Errorf("inventory.Client.GetBrandsBySKU: build request: %w", err)
	}
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return out, fmt.Errorf("inventory.Client.GetBrandsBySKU: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("inventory.Client.GetBrandsBySKU: status %d", resp.StatusCode)
	}
	var parsed brandsBySKUResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return out, fmt.Errorf("inventory.Client.GetBrandsBySKU: decode: %w", err)
	}
	if parsed.Data != nil {
		return parsed.Data, nil
	}
	return out, nil
}

// GetItemPrice fetches the authoritative unit/total price for an inventory item (default tier)
// for the given quantity. Returns ok=false when no pricing exists so callers can fall back.
func (c *Client) GetItemPrice(ctx context.Context, tenantID, itemID string, quantity int) (*ItemPrice, bool, error) {
	if quantity < 1 {
		quantity = 1
	}
	url := fmt.Sprintf("%s/v1/%s/inventory/items/%s/price?quantity=%d", c.baseURL, tenantID, itemID, quantity)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("inventory.Client.GetItemPrice: build request: %w", err)
	}
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, false, fmt.Errorf("inventory.Client.GetItemPrice: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil // no pricing configured — caller falls back to local rate
	}
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("inventory.Client.GetItemPrice: status %d", resp.StatusCode)
	}
	var price ItemPrice
	if err := json.NewDecoder(resp.Body).Decode(&price); err != nil {
		return nil, false, fmt.Errorf("inventory.Client.GetItemPrice: decode: %w", err)
	}
	return &price, true, nil
}

// BundleComponent is one component of an inventory Bundle (e.g. a MEAL_PERIOD).
type BundleComponent struct {
	ComponentItemID string `json:"component_item_id"`
	ComponentKind   string `json:"component_kind"`
	MealPeriod      string `json:"meal_period"`
	Quantity        int    `json:"quantity"`
}

// Bundle is the subset of an inventory-api Bundle needed to derive conference pricing
// and validate delegate meal periods.
type Bundle struct {
	ID           string            `json:"id"`
	ItemID       string            `json:"item_id"`
	Name         string            `json:"name"`
	PackageType  string            `json:"package_type"`
	PriceBasis   string            `json:"price_basis"`
	MinDelegates *int              `json:"min_delegates"`
	Components   []BundleComponent `json:"components"`
}

// MealPeriods returns the distinct meal_period codes defined as MEAL_PERIOD components.
func (b *Bundle) MealPeriods() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, c := range b.Components {
		if c.ComponentKind == "MEAL_PERIOD" && c.MealPeriod != "" {
			if _, ok := seen[c.MealPeriod]; !ok {
				seen[c.MealPeriod] = struct{}{}
				out = append(out, c.MealPeriod)
			}
		}
	}
	return out
}

// GetBundle fetches an inventory Bundle (package) with its components and price basis.
// Returns ok=false on 404 so callers can fall back to a manual total.
func (c *Client) GetBundle(ctx context.Context, tenantID, bundleID string) (*Bundle, bool, error) {
	url := fmt.Sprintf("%s/v1/%s/inventory/bundles/%s", c.baseURL, tenantID, bundleID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("inventory.Client.GetBundle: build request: %w", err)
	}
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, false, fmt.Errorf("inventory.Client.GetBundle: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("inventory.Client.GetBundle: status %d", resp.StatusCode)
	}
	var b Bundle
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, false, fmt.Errorf("inventory.Client.GetBundle: decode: %w", err)
	}
	return &b, true, nil
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
