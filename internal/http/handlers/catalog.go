package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/catalogitem"
)

// CatalogHandler handles catalog item CRUD endpoints.
type CatalogHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewCatalogHandler(log *zap.Logger, client *ent.Client) *CatalogHandler {
	return &CatalogHandler{log: log, client: client}
}

// inventoryProxyItem is the shape returned by inventory-api /items list.
type inventoryProxyItem struct {
	ID           string `json:"id"`
	SKU          string `json:"sku"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Type         string `json:"type"`
	IsActive     bool   `json:"is_active"`
	ImageURL     string `json:"image_url"`
	CategoryName string `json:"category_name"`
	Barcode      string `json:"barcode"`
}

// inventoryBulkPrice is one entry from GET /inventory/items/pricing.
type inventoryBulkPrice struct {
	ItemID   string  `json:"item_id"`
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
	TierCode string  `json:"tier_code"`
}

// proxyFromInventory calls inventory-api and returns items in the same DTO format
// as ListCatalogItems. Fetches items and bulk pricing concurrently.
func (h *CatalogHandler) proxyFromInventory(ctx context.Context, tenantSlug string) ([]map[string]any, error) {
	inventoryURL := os.Getenv("INVENTORY_API_URL")
	if inventoryURL == "" {
		inventoryURL = "http://inventory-api.inventory.svc.cluster.local:4000"
	}
	apiKey := os.Getenv("INTERNAL_SERVICE_KEY")

	doGet := func(url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if apiKey != "" {
			req.Header.Set("X-API-Key", apiKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("inventory-api %s returned %d", url, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}

	type itemsResult struct {
		items []inventoryProxyItem
		err   error
	}
	type priceResult struct {
		prices []inventoryBulkPrice
		err    error
	}

	itemsCh := make(chan itemsResult, 1)
	priceCh := make(chan priceResult, 1)

	go func() {
		body, err := doGet(fmt.Sprintf("%s/v1/%s/inventory/items?type=GOODS,RECIPE&status=active&limit=500", inventoryURL, tenantSlug))
		if err != nil {
			itemsCh <- itemsResult{err: err}
			return
		}
		var wrapper struct {
			Data []inventoryProxyItem `json:"data"`
		}
		if err := json.Unmarshal(body, &wrapper); err != nil {
			itemsCh <- itemsResult{err: err}
			return
		}
		itemsCh <- itemsResult{items: wrapper.Data}
	}()

	go func() {
		body, err := doGet(fmt.Sprintf("%s/v1/%s/inventory/items/pricing", inventoryURL, tenantSlug))
		if err != nil {
			// Pricing is best-effort — log and continue with price=0
			h.log.Warn("failed to fetch bulk pricing from inventory-api", zap.Error(err))
			priceCh <- priceResult{}
			return
		}
		var prices []inventoryBulkPrice
		if err := json.Unmarshal(body, &prices); err != nil {
			h.log.Warn("failed to decode bulk pricing", zap.Error(err))
			priceCh <- priceResult{}
			return
		}
		priceCh <- priceResult{prices: prices}
	}()

	ir := <-itemsCh
	pr := <-priceCh
	if ir.err != nil {
		return nil, ir.err
	}

	// Build item_id → price map
	priceByID := make(map[string]float64, len(pr.prices))
	for _, p := range pr.prices {
		priceByID[p.ItemID] = p.Price
	}

	out := make([]map[string]any, 0, len(ir.items))
	for _, item := range ir.items {
		status := "active"
		if !item.IsActive {
			status = "inactive"
		}
		out = append(out, map[string]any{
			"id":          item.ID,
			"sku":         item.SKU,
			"name":        item.Name,
			"description": item.Description,
			"category":    item.CategoryName,
			"item_type":   item.Type,
			"status":      status,
			"image_url":   item.ImageURL,
			"barcode":     item.Barcode,
			"price":       priceByID[item.ID],
		})
	}
	return out, nil
}

// ListCatalogItems handles GET /{tenantID}/pos/catalog/items
// Primary source: inventory-api (always proxied — ensures fresh data even when local sync is stale).
// Local CatalogItem table is used as an override layer (status, tax_status per SKU).
// Fallback: local-only query when inventory-api is unreachable.
func (h *CatalogHandler) ListCatalogItems(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * limit

	catFilter := r.URL.Query().Get("category")
	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		statusFilter = "active"
	}
	searchFilter := strings.ToLower(r.URL.Query().Get("search"))

	// Resolve tenant slug from JWT claims or httpware context
	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	if tenantSlug == "" {
		tenantSlug = httpware.GetTenantSlug(r.Context())
	}

	// ── Primary path: inventory-api proxy + local overrides ──────────────────
	if tenantSlug != "" {
		proxyItems, proxyErr := h.proxyFromInventory(r.Context(), tenantSlug)
		if proxyErr == nil && len(proxyItems) > 0 {
			// Fetch local overrides (status/tax_status set via POS catalog management)
			localItems, _ := h.client.CatalogItem.Query().
				Where(catalogitem.TenantID(tid)).
				All(r.Context())
			overrides := make(map[string]*ent.CatalogItem, len(localItems))
			for _, li := range localItems {
				overrides[li.Sku] = li
			}

			// Merge inventory items with local overrides, then apply filters
			merged := make([]map[string]any, 0, len(proxyItems))
			for _, item := range proxyItems {
				sku, _ := item["sku"].(string)
				if local, ok := overrides[sku]; ok {
					item["status"] = local.Status
					item["tax_status"] = local.TaxStatus
				}
				// Apply filters on merged result
				if catFilter != "" {
					cat, _ := item["category"].(string)
					if !strings.EqualFold(cat, catFilter) {
						continue
					}
				}
				if statusFilter != "" {
					st, _ := item["status"].(string)
					if st != statusFilter {
						continue
					}
				}
				if searchFilter != "" {
					name := strings.ToLower(fmt.Sprintf("%v", item["name"]))
					if !strings.Contains(name, searchFilter) {
						continue
					}
				}
				merged = append(merged, item)
			}

			total := len(merged)
			start := offset
			if start > total {
				start = total
			}
			end := start + limit
			if end > total {
				end = total
			}
			jsonOK(w, map[string]any{"data": merged[start:end], "total": total, "limit": limit, "page": page})
			return
		}
		if proxyErr != nil {
			h.log.Warn("inventory proxy failed, falling back to local catalog", zap.Error(proxyErr))
		}
	}

	// ── Fallback: local catalog only ─────────────────────────────────────────
	query := h.client.CatalogItem.Query().Where(
		catalogitem.TenantID(tid),
		catalogitem.ItemTypeNotIn("INGREDIENT", "EQUIPMENT"),
	)

	if catFilter != "" {
		query = query.Where(catalogitem.Category(catFilter))
	}
	if statusFilter != "" {
		query = query.Where(catalogitem.Status(statusFilter))
	}
	if searchFilter != "" {
		query = query.Where(catalogitem.NameContainsFold(searchFilter))
	}

	total, _ := query.Clone().Count(r.Context())
	items, err := query.
		Offset(offset).
		Limit(limit).
		Order(ent.Asc(catalogitem.FieldName)).
		All(r.Context())
	if err != nil {
		h.log.Error("list catalog items failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": items, "total": total, "limit": limit, "page": page})
}

type createCatalogItemInput struct {
	SKU       string `json:"sku"`
	Name      string `json:"name"`
	Category  string `json:"category"`
	TaxStatus string `json:"taxStatus"`
	Status    string `json:"status"`
	Barcode   string `json:"barcode,omitempty"`
}

// CreateCatalogItem handles POST /{tenantID}/pos/catalog/items
func (h *CatalogHandler) CreateCatalogItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createCatalogItemInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Name == "" || input.SKU == "" {
		jsonError(w, "name and sku are required", http.StatusBadRequest)
		return
	}
	if input.TaxStatus == "" {
		input.TaxStatus = "taxable"
	}
	if input.Status == "" {
		input.Status = "active"
	}

	builder := h.client.CatalogItem.Create().
		SetTenantID(tid).
		SetSku(input.SKU).
		SetName(input.Name).
		SetCategory(input.Category).
		SetTaxStatus(input.TaxStatus).
		SetStatus(input.Status)

	item, err := builder.Save(r.Context())
	if err != nil {
		h.log.Error("create catalog item failed", zap.Error(err))
		jsonError(w, "failed to create item: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, item)
}

// GetCatalogItem handles GET /{tenantID}/pos/catalog/items/{id}
func (h *CatalogHandler) GetCatalogItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid item id", http.StatusBadRequest)
		return
	}

	item, err := h.client.CatalogItem.Query().
		Where(catalogitem.ID(itemID), catalogitem.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "item not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, item)
}

// UpdateCatalogItem handles PUT /{tenantID}/pos/catalog/items/{id}
func (h *CatalogHandler) UpdateCatalogItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid item id", http.StatusBadRequest)
		return
	}

	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	item, err := h.client.CatalogItem.Query().
		Where(catalogitem.ID(itemID), catalogitem.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "item not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	updater := item.Update()
	if v, ok := input["name"].(string); ok {
		updater.SetName(v)
	}
	if v, ok := input["category"].(string); ok {
		updater.SetCategory(v)
	}
	if v, ok := input["status"].(string); ok {
		updater.SetStatus(v)
	}
	if v, ok := input["taxStatus"].(string); ok {
		updater.SetTaxStatus(v)
	}

	updated, err := updater.Save(r.Context())
	if err != nil {
		jsonError(w, "update failed", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// DeleteCatalogItem handles DELETE /{tenantID}/pos/catalog/items/{id} (soft delete)
func (h *CatalogHandler) DeleteCatalogItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid item id", http.StatusBadRequest)
		return
	}

	_, err = h.client.CatalogItem.Update().
		Where(catalogitem.ID(itemID), catalogitem.TenantID(tid)).
		SetStatus("inactive").
		Save(r.Context())
	if err != nil {
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// parseTenantUUID extracts and parses tenant UUID from httpware context.
// Platform owners can override via ?tenantId= query param for cross-tenant access.
func parseTenantUUID(r *http.Request) (uuid.UUID, error) {
	ctx := r.Context()

	// Platform owner query-param override
	if httpware.IsPlatformOwner(ctx) {
		if q := r.URL.Query().Get("tenantId"); q != "" {
			return uuid.Parse(q)
		}
	}

	// Standard: httpware context (from TenantV2 middleware)
	tenantIDStr := httpware.GetTenantID(ctx)
	if tenantIDStr != "" {
		if httpware.IsPlatformOwner(ctx) {
			claims, ok := authclient.ClaimsFromContext(ctx)
			if ok && claims.TenantID == tenantIDStr {
				// Platform owner's own tenant — return Nil to indicate "all"
				return uuid.Nil, nil
			}
		}
		return uuid.Parse(tenantIDStr)
	}

	return uuid.Nil, fmt.Errorf("tenant context required")
}
