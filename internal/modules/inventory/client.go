// Package inventory provides an S2S client for inventory-api consumption backflush.
// All calls use the shared INTERNAL_SERVICE_KEY via X-API-Key header.
package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// SetItemPrice patches an inventory item's selling price by SKU — PATCH
// /v1/{tenant}/inventory/items/{sku}/price. Inventory repoints the price everywhere its
// POS price-resolve reads it (guardrail fields, RETAIL/WHOLESALE tier rows, and the
// linked recipe's selling price for RECIPE items). Used by the order-line edit's
// "also update the catalog price" option; callers treat failure as non-fatal.
func (c *Client) SetItemPrice(ctx context.Context, tenantID, sku string, price float64) error {
	if c == nil || c.baseURL == "" {
		return fmt.Errorf("inventory.Client.SetItemPrice: client not configured")
	}
	body, err := json.Marshal(map[string]float64{"price": price})
	if err != nil {
		return fmt.Errorf("inventory.Client.SetItemPrice: marshal: %w", err)
	}
	reqURL := fmt.Sprintf("%s/v1/%s/inventory/items/%s/price", c.baseURL, tenantID, url.PathEscape(sku))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("inventory.Client.SetItemPrice: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("inventory.Client.SetItemPrice: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("inventory.Client.SetItemPrice: status %d", resp.StatusCode)
	}
	return nil
}

// ConsumedLot is one InventoryLot's contribution to a SKU's consumption within a sale —
// mirrors inventory-api's stock.ConsumedLot. Populated only when the tenant's costing method
// is lot-ordered (fifo/lifo/fefo); empty for wavg-costed items.
type ConsumedLot struct {
	SKU        string     `json:"sku"`
	LotID      string     `json:"lot_id"`
	LotNumber  string     `json:"lot_number,omitempty"`
	ExpiryDate *time.Time `json:"expiry_date,omitempty"`
	Quantity   float64    `json:"quantity"`
}

// ConsumptionResult mirrors inventory-api's stock.ConsumptionResponse (the fields pos-api
// actually consumes — id/tenant/order/status/processed_at are dropped as unused here).
type ConsumptionResult struct {
	LotsConsumed []ConsumedLot `json:"lots_consumed,omitempty"`
}

// RecordConsumption calls inventory-api to backflush stock for a completed POS order.
// Non-fatal: callers should log and optionally publish a retry event on error. Returns the
// lot(s) actually drawn per SKU (Phase 2 FEFO traceability) so the caller can stamp
// POSOrderLine.lot_number/expiry_date and the controlled-substance dispense log.
func (c *Client) RecordConsumption(ctx context.Context, tenantID string, req ConsumptionRequest) (*ConsumptionResult, error) {
	if len(req.Items) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.RecordConsumption: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1/%s/inventory/consumption", c.baseURL, tenantID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.RecordConsumption: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.RecordConsumption: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("inventory.Client.RecordConsumption: status %d", resp.StatusCode)
	}
	var out ConsumptionResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// Response decode failure doesn't undo the consumption that already succeeded
		// server-side — log-equivalent: return no lots, not an error, so callers don't
		// treat a successful backflush as failed just because lot data was unreadable.
		return nil, nil
	}
	return &out, nil
}

// ReservationItem is one SKU to reserve.
type ReservationItem struct {
	SKU      string  `json:"sku"`
	Quantity float64 `json:"quantity"`
}

// CreateReservationRequest is the body for POST /v1/{tenant}/inventory/reservations.
type CreateReservationRequest struct {
	OrderID        string            `json:"order_id"`
	Items          []ReservationItem `json:"items"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

// ReservedItem mirrors inventory-api's stock.ReservedItem.
type ReservedItem struct {
	SKU             string  `json:"sku"`
	RequestedQty    float64 `json:"requested_qty"`
	ReservedQty     float64 `json:"reserved_qty"`
	AvailableQty    float64 `json:"available_qty"`
	IsFullyReserved bool    `json:"is_fully_reserved"`
}

// Reservation mirrors inventory-api's stock.ReservationResponse.
type Reservation struct {
	ID        string         `json:"id"`
	OrderID   string         `json:"order_id"`
	Status    string         `json:"status"`
	Items     []ReservedItem `json:"items"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
}

// CreateReservation reserves stock for a prescription's catalog-linked lines at
// pharmacist-approval time (Phase 3: Available→Reserved, reusing inventory-api's existing
// Reservation state machine rather than a new one).
func (c *Client) CreateReservation(ctx context.Context, tenantID string, req CreateReservationRequest) (*Reservation, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.CreateReservation: marshal: %w", err)
	}
	url := fmt.Sprintf("%s/v1/%s/inventory/reservations", c.baseURL, tenantID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.CreateReservation: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.CreateReservation: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("inventory.Client.CreateReservation: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out Reservation
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("inventory.Client.CreateReservation: decode: %w", err)
	}
	return &out, nil
}

// ReleaseReservation releases a held reservation (prescription rejected/cancelled),
// restoring reserved stock back to available.
func (c *Client) ReleaseReservation(ctx context.Context, tenantID, reservationID, reason string) error {
	body, _ := json.Marshal(map[string]string{"reason": reason})
	url := fmt.Sprintf("%s/v1/%s/inventory/reservations/%s/release", c.baseURL, tenantID, reservationID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("inventory.Client.ReleaseReservation: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("inventory.Client.ReleaseReservation: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("inventory.Client.ReleaseReservation: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ConsumeReservation converts a held reservation into an actual stock depletion at payment
// finalize — the reserved units become sold, atomically, rather than a fresh ad-hoc decrement.
func (c *Client) ConsumeReservation(ctx context.Context, tenantID, reservationID string) error {
	url := fmt.Sprintf("%s/v1/%s/inventory/reservations/%s/consume", c.baseURL, tenantID, reservationID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("inventory.Client.ConsumeReservation: build request: %w", err)
	}
	httpReq.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("inventory.Client.ConsumeReservation: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("inventory.Client.ConsumeReservation: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ReverseConsumptionItem selects one sale-line SKU to reverse: Quantity of OfQuantity
// originally sold (the ratio prorates the recorded ingredient consumption).
type ReverseConsumptionItem struct {
	SKU        string  `json:"sku"`
	Quantity   float64 `json:"quantity"`
	OfQuantity float64 `json:"of_quantity,omitempty"`
}

// ReverseConsumptionRequest is the body for POST /v1/{tenant}/inventory/consumption/reverse.
// Empty Items reverses the order's entire recorded consumption. Idempotent on IdempotencyKey.
type ReverseConsumptionRequest struct {
	OrderID        string                   `json:"order_id"`
	Items          []ReverseConsumptionItem `json:"items,omitempty"`
	Reason         string                   `json:"reason,omitempty"`
	IdempotencyKey string                   `json:"idempotency_key,omitempty"`
}

// ReversedIngredient reports one ingredient line's reversal outcome.
type ReversedIngredient struct {
	IngredientSKU    string  `json:"ingredient_sku"`
	RecipeSKU        string  `json:"recipe_sku,omitempty"`
	QuantityReversed float64 `json:"quantity_reversed"`
	StockReturned    float64 `json:"stock_returned"`
	CostReversed     float64 `json:"cost_reversed"`
}

// ReverseConsumptionResponse summarizes what inventory reversed for the order.
type ReverseConsumptionResponse struct {
	ID                string               `json:"id"`
	OrderID           string               `json:"order_id"`
	Status            string               `json:"status"`
	AlreadyProcessed  bool                 `json:"already_processed,omitempty"`
	TotalCostReversed float64              `json:"total_cost_reversed"`
	Ingredients       []ReversedIngredient `json:"ingredients"`
}

// ResolvedDrugItem is one item's pharmacy classification, resolved by inventory-api.
type ResolvedDrugItem struct {
	ItemID           string `json:"item_id,omitempty"`
	SKU              string `json:"sku"`
	DrugClass        string `json:"drug_class,omitempty"`
	ActiveIngredient string `json:"active_ingredient,omitempty"`
	GenericName      string `json:"generic_name,omitempty"`
}

// DrugInteractionFinding is one flagged interaction between two dispensed SKUs.
type DrugInteractionFinding struct {
	ClassA                 string `json:"class_a"`
	ClassB                 string `json:"class_b"`
	SKUA                   string `json:"sku_a"`
	SKUB                   string `json:"sku_b"`
	Severity               string `json:"severity"`
	Description            string `json:"description,omitempty"`
	ClinicalRecommendation string `json:"clinical_recommendation,omitempty"`
}

// AllergyMatch flags a dispensed SKU whose drug_class matched a patient-declared allergy flag.
type AllergyMatch struct {
	SKU              string `json:"sku"`
	ActiveIngredient string `json:"active_ingredient,omitempty"`
	AllergyFlag      string `json:"allergy_flag"`
}

// CheckInteractionsRequest is the body for POST /v1/{tenant}/inventory/items/check-interactions.
// PrescriptionLine only carries catalog_item_id (not SKU), so ItemIDs is the primary path;
// SKUs is supported for other callers (e.g. an OTC-item interaction check at checkout).
type CheckInteractionsRequest struct {
	SKUs         []string `json:"skus,omitempty"`
	ItemIDs      []string `json:"item_ids,omitempty"`
	AllergyFlags []string `json:"allergy_flags,omitempty"`
}

// CheckInteractionsResponse mirrors inventory-api's response — resolved drug classifications
// plus any DrugInteractionRule matches and allergy-flag hits across the given SKU set.
type CheckInteractionsResponse struct {
	Resolved       []ResolvedDrugItem       `json:"resolved"`
	Interactions   []DrugInteractionFinding `json:"interactions"`
	AllergyMatches []AllergyMatch           `json:"allergy_matches"`
}

// CheckInteractions calls inventory-api to resolve drug classes for a set of SKUs and cross-join
// them against the curated interaction-rule table + patient allergy flags. inventory-api owns
// both Item classification and DrugInteractionRule, so the match happens there, not here.
func (c *Client) CheckInteractions(ctx context.Context, tenantID string, req CheckInteractionsRequest) (*CheckInteractionsResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.CheckInteractions: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1/%s/inventory/items/check-interactions", c.baseURL, tenantID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.CheckInteractions: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.CheckInteractions: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("inventory.Client.CheckInteractions: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out CheckInteractionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("inventory.Client.CheckInteractions: decode: %w", err)
	}
	return &out, nil
}

// ReverseConsumption calls inventory-api to reverse (part of) an order's recorded BOM
// consumption — the stock side of a POS sale reversal. Idempotent server-side.
func (c *Client) ReverseConsumption(ctx context.Context, tenantID string, req ReverseConsumptionRequest) (*ReverseConsumptionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.ReverseConsumption: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1/%s/inventory/consumption/reverse", c.baseURL, tenantID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.ReverseConsumption: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("inventory.Client.ReverseConsumption: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("inventory.Client.ReverseConsumption: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out ReverseConsumptionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("inventory.Client.ReverseConsumption: decode: %w", err)
	}
	return &out, nil
}
