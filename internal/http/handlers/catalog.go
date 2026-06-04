package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/http/middleware"
)

// CatalogHandler handles catalog item endpoints.
// Item data is always sourced from inventory-api (name, description, image, category).
// Local POSCatalogOverride stores only POS-specific fields (selling_price, tax_status, availability).
type CatalogHandler struct {
	log    *zap.Logger
	client *ent.Client
	redis  *redis.Client
}

func NewCatalogHandler(log *zap.Logger, client *ent.Client) *CatalogHandler {
	return &CatalogHandler{log: log, client: client}
}

// inventoryProxyItem is the shape returned by inventory-api /items list.
type inventoryProxyItem struct {
	ID                      string   `json:"id"`
	SKU                     string   `json:"sku"`
	Name                    string   `json:"name"`
	Description             string   `json:"description"`
	Type                    string   `json:"type"`
	IsActive                bool     `json:"is_active"`
	ImageURL                string   `json:"image_url"`
	CategoryName            string   `json:"category_name"`
	Barcode                 string   `json:"barcode"`
	RequiresAgeVerification bool     `json:"requires_age_verification"`
	IsControlledSubstance   bool     `json:"is_controlled_substance"`
	TrackSerialNumbers      bool     `json:"track_serial_numbers"`
	DurationMinutes         int      `json:"duration_minutes"`
	CostPrice               *float64 `json:"cost_price,omitempty"`
	SuggestedPrice          *float64 `json:"suggested_price,omitempty"`
	// Recipe-costing fields (added 2026-06-01)
	SellingPrice   *float64 `json:"selling_price,omitempty"`
	FoodCostPct    *float64 `json:"food_cost_pct,omitempty"`
	FoodCostStatus string   `json:"status,omitempty"` // "OK - healthy" | "OK - above target FC%" | "LOSS"
	// Supplier / EP-cost fields
	PurchasePrice    *float64 `json:"purchase_price,omitempty"`
	PurchasePackSize *float64 `json:"purchase_pack_size,omitempty"`
	PurchaseUnit     string   `json:"purchase_unit,omitempty"`
	YieldPct         *float64 `json:"yield_pct,omitempty"`
	// Tax — enriched by inventory-api from treasury-api (source of truth)
	TaxCodeID    string   `json:"tax_code_id,omitempty"`
	TaxInclusive bool     `json:"tax_inclusive,omitempty"`
	TaxRate      *float64 `json:"tax_rate,omitempty"`
	NetPrice     *float64 `json:"net_price,omitempty"`
	TaxAmount    *float64 `json:"tax_amount,omitempty"`
}

// inventoryBulkPrice is one entry from GET /inventory/items/pricing.
type inventoryBulkPrice struct {
	ItemID   string  `json:"item_id"`
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
	TierCode string  `json:"tier_code"`
}

func inventoryURL() string {
	if u := os.Getenv("INVENTORY_API_URL"); u != "" {
		return u
	}
	return "http://inventory-api.inventory.svc.cluster.local:4000"
}

func serviceAPIKey() string {
	return os.Getenv("INTERNAL_SERVICE_KEY")
}

// categoryAllowedForUseCase checks whether an item's category is appropriate for the outlet use case.
// Uses case-insensitive substring matching so that minor category name variations don't break filtering.
// Items with no category are always allowed.
func categoryAllowedForUseCase(categoryName, useCase string) bool {
	cat := strings.ToLower(strings.TrimSpace(categoryName))
	if cat == "" || useCase == "" {
		return true
	}
	isPharmacyCat := strings.Contains(cat, "pharmacy") || strings.Contains(cat, "chemist") || strings.Contains(cat, "drug") || strings.Contains(cat, "medicine") || strings.Contains(cat, "pharmaceutical")
	isFoodCat := strings.Contains(cat, "breakfast") ||
		strings.Contains(cat, "beverage") ||
		strings.Contains(cat, "pastry") ||
		strings.Contains(cat, "pastries") ||
		strings.Contains(cat, "bakery") ||
		strings.Contains(cat, "pizza") ||
		strings.Contains(cat, "salad") ||
		strings.Contains(cat, "sandwich") ||
		strings.Contains(cat, "wrap") ||
		strings.Contains(cat, "main course") ||
		strings.Contains(cat, "light bite") ||
		strings.Contains(cat, "dessert") ||
		strings.Contains(cat, "hot beverage") ||
		strings.Contains(cat, "cold beverage")
	isServicesCat := strings.Contains(cat, "beauty") || strings.Contains(cat, "spa") ||
		strings.Contains(cat, "event") || strings.Contains(cat, "experience") ||
		strings.Contains(cat, "wellness")
	isRetailCat := strings.Contains(cat, "retail")

	switch strings.ToLower(useCase) {
	case "retail":
		// Retail outlets sell general merchandise — exclude pharmacy, food/restaurant, and services items
		return !isPharmacyCat && !isFoodCat && !isServicesCat
	case "pharmacy":
		// Pharmacy outlets sell only pharmacy/health items
		return isPharmacyCat
	case "hospitality", "quick_service":
		// Restaurant/QSR outlets sell food — exclude pharmacy and pure retail items
		return !isPharmacyCat && !isRetailCat && !isServicesCat
	case "services":
		// Beauty/wellness outlets sell services — exclude pharmacy, food, and retail
		return !isPharmacyCat && !isFoodCat && !isRetailCat
	default:
		return true
	}
}

// useCaseItemTypes returns the comma-separated inventory item types valid for a given outlet use case.
// This prevents cross-contamination (e.g. pharmacy GOODS appearing in retail).
func useCaseItemTypes(useCase string) string {
	switch strings.ToLower(useCase) {
	case "retail":
		return "GOODS,VOUCHER"
	case "hospitality", "quick_service":
		return "GOODS,RECIPE,SERVICE,INGREDIENT,VOUCHER"
	case "pharmacy":
		return "GOODS,VOUCHER"
	case "services":
		return "SERVICE,GOODS,VOUCHER"
	default:
		return "GOODS,RECIPE,SERVICE,VOUCHER"
	}
}

func doInventoryGET(ctx context.Context, path string, outletID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if k := serviceAPIKey(); k != "" {
		req.Header.Set("X-API-Key", k)
	}
	if outletID != "" {
		req.Header.Set("X-Outlet-ID", outletID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("inventory-api %s returned %d", path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// fetchInventoryItems calls inventory-api and returns active sellable items scoped by outlet and use case.
func fetchInventoryItems(ctx context.Context, tenantSlug, outletID, useCase string) ([]inventoryProxyItem, error) {
	types := useCaseItemTypes(useCase)
	url := fmt.Sprintf("%s/v1/%s/inventory/items?type=%s&status=active&limit=500", inventoryURL(), tenantSlug, types)
	body, err := doInventoryGET(ctx, url, outletID)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Data []inventoryProxyItem `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Data, nil
}

// fetchInventoryServiceItems lists inventory items filtered by use_case (e.g. HOSPITALITY_ROOM,
// HOSPITALITY_FACILITY, CONFERENCE, AMENITY) — used by hotel forms to pick the authoritative
// inventory master item to link to a Room/Facility/Amenity.
func fetchInventoryServiceItems(ctx context.Context, tenantSlug, useCase string) ([]inventoryProxyItem, error) {
	url := fmt.Sprintf("%s/v1/%s/inventory/items?use_case=%s&status=active&limit=500", inventoryURL(), tenantSlug, useCase)
	body, err := doInventoryGET(ctx, url, "")
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Data []inventoryProxyItem `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Data, nil
}

// inventoryProxyBundle is the subset of an inventory-api Bundle the hotel forms need
// to render a searchable picker (conference package selection).
type inventoryProxyBundle struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	SKU         string `json:"sku"`
	PackageType string `json:"package_type"`
	Price       float64 `json:"price"`
}

// fetchInventoryBundles lists inventory-api Bundles (packages) so hotel/conference forms
// can pick an authoritative package by name instead of pasting a raw UUID.
func fetchInventoryBundles(ctx context.Context, tenantSlug string) ([]inventoryProxyBundle, error) {
	url := fmt.Sprintf("%s/v1/%s/inventory/bundles?limit=500", inventoryURL(), tenantSlug)
	body, err := doInventoryGET(ctx, url, "")
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Data []inventoryProxyBundle `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Data, nil
}

// fetchInventoryPricing calls inventory-api for default-tier prices.
func fetchInventoryPricing(ctx context.Context, tenantSlug, outletID string) (map[string]float64, error) {
	url := fmt.Sprintf("%s/v1/%s/inventory/items/pricing", inventoryURL(), tenantSlug)
	body, err := doInventoryGET(ctx, url, outletID)
	if err != nil {
		return nil, err
	}
	var prices []inventoryBulkPrice
	if err := json.Unmarshal(body, &prices); err != nil {
		return nil, err
	}
	m := make(map[string]float64, len(prices))
	for _, p := range prices {
		m[p.ItemID] = p.Price
	}
	return m, nil
}

// ListCatalogItems handles GET /{tenantID}/pos/catalog/items
// Always proxies from inventory-api; merges with local POSCatalogOverride for pricing/availability.
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
	searchFilter := strings.ToLower(r.URL.Query().Get("search"))
	itemTypeFilter := strings.ToUpper(r.URL.Query().Get("item_type"))

	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	if tenantSlug == "" {
		tenantSlug = httpware.GetTenantSlug(r.Context())
	}
	// Terminal PIN JWTs don't embed tenant_slug — look it up from the local Tenant table.
	if tenantSlug == "" {
		if t, lookupErr := h.client.Tenant.Get(r.Context(), tid); lookupErr == nil {
			tenantSlug = t.Slug
		}
	}

	var outletID *uuid.UUID
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			outletID = &oid
		}
	}
	if outletIDStr := r.URL.Query().Get("outlet_id"); outletIDStr != "" && outletID == nil {
		if oid, parseErr := uuid.Parse(outletIDStr); parseErr == nil {
			outletID = &oid
		}
	}

	// Resolve use case from outlet context for item type filtering.
	useCase := ""
	outletIDStr := ""
	if oc := middleware.OutletFromContext(r.Context()); oc != nil {
		useCase = oc.UseCase
		outletIDStr = oc.ID.String()
	} else if outletID != nil {
		outletIDStr = outletID.String()
	}

	if tenantSlug == "" {
		jsonError(w, "tenant slug required", http.StatusBadRequest)
		return
	}

	// Fetch items + pricing from inventory-api in parallel
	type itemsResult struct {
		items []inventoryProxyItem
		err   error
	}
	type priceResult struct {
		prices map[string]float64
		err    error
	}
	itemsCh := make(chan itemsResult, 1)
	priceCh := make(chan priceResult, 1)

	go func() {
		items, err := fetchInventoryItems(r.Context(), tenantSlug, outletIDStr, useCase)
		itemsCh <- itemsResult{items, err}
	}()
	go func() {
		prices, err := fetchInventoryPricing(r.Context(), tenantSlug, outletIDStr)
		priceCh <- priceResult{prices, err}
	}()

	ir := <-itemsCh
	pr := <-priceCh
	if ir.err != nil {
		h.log.Error("inventory items fetch failed", zap.Error(ir.err))
		jsonError(w, "failed to fetch catalog from inventory", http.StatusBadGateway)
		return
	}
	if pr.err != nil {
		h.log.Warn("inventory pricing fetch failed — prices will be 0", zap.Error(pr.err))
	}
	invPriceByID := pr.prices

	// Load all POS overrides for this tenant
	overrides, _ := h.client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid)).
		All(r.Context())

	// Build SKU → best override map (outlet-scoped wins over tenant-wide)
	type overrideEntry struct {
		sellingPrice            *float64
		taxStatus               string
		isAvailable             bool
		isFeatured              bool
		displayOrder            int
		requiresPrescription    bool
		isReturnable            bool
		requiresAgeVerification bool
		isControlledSubstance   bool
		minimumAge              *int
		durationMinutes         *int
	}
	overrideMap := make(map[string]overrideEntry)
	for _, o := range overrides {
		key := o.InventorySku
		prev, exists := overrideMap[key]
		// outlet-scoped overrides take precedence over tenant-wide
		if !exists || (o.OutletID != nil && outletID != nil && *o.OutletID == *outletID) {
			overrideMap[key] = overrideEntry{
				sellingPrice:            o.SellingPrice,
				taxStatus:               o.TaxStatus,
				isAvailable:             o.IsAvailable,
				isFeatured:              o.IsFeatured,
				displayOrder:            o.DisplayOrder,
				requiresPrescription:    o.RequiresPrescription,
				isReturnable:            o.IsReturnable,
				requiresAgeVerification: o.RequiresAgeVerification,
				isControlledSubstance:   o.IsControlledSubstance,
				minimumAge:              o.MinimumAge,
				durationMinutes:         o.DurationMinutes,
			}
		} else {
			_ = prev
		}
	}

	out := make([]map[string]any, 0, len(ir.items))
	for _, item := range ir.items {
		// Apply filters
		if catFilter != "" && !strings.EqualFold(item.CategoryName, catFilter) {
			continue
		}
		if searchFilter != "" && !strings.Contains(strings.ToLower(item.Name), searchFilter) {
			continue
		}
		if itemTypeFilter != "" {
			matched := false
			for _, t := range strings.Split(itemTypeFilter, ",") {
				if strings.EqualFold(strings.TrimSpace(t), item.Type) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		// Exclude items whose category doesn't belong to this outlet's use case.
		if useCase != "" && !categoryAllowedForUseCase(item.CategoryName, useCase) {
			continue
		}

		o, hasOverride := overrideMap[item.SKU]

		// Price: local selling_price > inventory tier > 0
		price := 0.0
		taxStatus := "taxable"
		isAvailable := item.IsActive
		isFeatured := false
		displayOrder := 0
		requiresPrescription := false
		isReturnable := true
		requiresAgeVerification := item.RequiresAgeVerification
		isControlledSubstance := item.IsControlledSubstance
		var minimumAge *int
		var durationMinutes *int

		if invPrice, ok := invPriceByID[item.ID]; ok {
			price = invPrice
		}

		if hasOverride {
			if o.sellingPrice != nil && *o.sellingPrice > 0 {
				price = *o.sellingPrice
			}
			if o.taxStatus != "" {
				taxStatus = o.taxStatus
			}
			isAvailable = o.isAvailable
			isFeatured = o.isFeatured
			displayOrder = o.displayOrder
			requiresPrescription = o.requiresPrescription
			isReturnable = o.isReturnable
			if o.requiresAgeVerification {
				requiresAgeVerification = true
			}
			if o.isControlledSubstance {
				isControlledSubstance = true
			}
			minimumAge = o.minimumAge
			durationMinutes = o.durationMinutes
		}

		if item.DurationMinutes > 0 && durationMinutes == nil {
			durationMinutes = &item.DurationMinutes
		}

		// Inventory-provided effective selling price (recipe price / default tier; enriched by
		// inventory-api). This is the primary source for RECIPE menu items, which carry no cost_price.
		if price == 0 && item.SellingPrice != nil && *item.SellingPrice > 0 {
			price = *item.SellingPrice
		}

		// Last resort: use cost_price from inventory when no pricing tier or override provides a price.
		if price == 0 {
			if item.SuggestedPrice != nil && *item.SuggestedPrice > 0 {
				price = *item.SuggestedPrice
			} else if item.CostPrice != nil && *item.CostPrice > 0 {
				price = *item.CostPrice
			}
		}

		// Round up to next whole number — no decimal prices on POS receipts or displays.
		if price > 0 {
			price = math.Ceil(price)
		}

		// Items with no price must not appear as available on the POS terminal —
		// a staff member cannot ring up or sell an item at KES 0 by mistake.
		// Items need a price set in inventory or a POS override before they can be sold.
		if price == 0 {
			isAvailable = false
		}

		out = append(out, map[string]any{
			"id":                        item.ID,
			"sku":                       item.SKU,
			"name":                      item.Name,
			"description":               item.Description,
			"category":                  item.CategoryName,
			"item_type":                 item.Type,
			"status":                    map[bool]string{true: "active", false: "inactive"}[item.IsActive],
			"is_available":              isAvailable,
			"is_featured":               isFeatured,
			"display_order":             displayOrder,
			"image_url":                 item.ImageURL,
			"barcode":                   item.Barcode,
			"price":                     price,
			"tax_status":                taxStatus,
			"tax_code_id":               item.TaxCodeID,
			"tax_inclusive":             item.TaxInclusive,
			"tax_rate":                  item.TaxRate,
			"requires_prescription":     requiresPrescription,
			"is_returnable":             isReturnable,
			"requires_age_verification": requiresAgeVerification,
			"is_controlled_substance":   isControlledSubstance,
			"track_serial_numbers":      item.TrackSerialNumbers,
			"minimum_age":               minimumAge,
			"duration_minutes":          durationMinutes,
			"outlet_id":                 outletID,
		})
	}

	total := len(out)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	jsonOK(w, map[string]any{
		"data":  out[start:end],
		"total": total,
		"limit": limit,
		"page":  page,
	})
}

// GetCatalogItem handles GET /{tenantID}/pos/catalog/items/{id}
// Fetches by inventory item ID — proxied from inventory-api + merged with local override.
func (h *CatalogHandler) GetCatalogItem(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "id")
	if itemID == "" {
		jsonError(w, "item id required", http.StatusBadRequest)
		return
	}

	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	if tenantSlug == "" {
		tenantSlug = httpware.GetTenantSlug(r.Context())
	}
	if tenantSlug == "" {
		jsonError(w, "tenant slug required", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("%s/v1/%s/inventory/items/%s", inventoryURL(), tenantSlug, itemID)
	body, err := doInventoryGET(r.Context(), url, "")
	if err != nil {
		jsonError(w, "item not found", http.StatusNotFound)
		return
	}

	var item map[string]any
	if err := json.Unmarshal(body, &item); err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, item)
}

// SetCatalogItemPrice handles PATCH /{tenantID}/pos/catalog/items/prices
// Upserts a POS selling_price override for an item identified by SKU.
func (h *CatalogHandler) SetCatalogItemPrice(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		SKU          string  `json:"sku"`
		SellingPrice float64 `json:"selling_price"`
		OutletID     string  `json:"outlet_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SKU == "" {
		jsonError(w, "sku and selling_price are required", http.StatusBadRequest)
		return
	}

	var outletID *uuid.UUID
	if input.OutletID != "" {
		if oid, parseErr := uuid.Parse(input.OutletID); parseErr == nil {
			outletID = &oid
		}
	}

	existing, _ := h.client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid), entoverride.InventorySku(input.SKU)).
		First(r.Context())

	if existing != nil {
		upd := existing.Update().SetSellingPrice(input.SellingPrice)
		if outletID != nil {
			upd.SetOutletID(*outletID)
		}
		updated, saveErr := upd.Save(r.Context())
		if saveErr != nil {
			jsonError(w, "update failed: "+saveErr.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, updated)
		return
	}

	creator := h.client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku(input.SKU).
		SetSellingPrice(input.SellingPrice)
	if outletID != nil {
		creator.SetOutletID(*outletID)
	}
	created, saveErr := creator.Save(r.Context())
	if saveErr != nil {
		jsonError(w, "create failed: "+saveErr.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, created)
}

// BulkSetCatalogPrices handles POST /{tenantID}/pos/catalog/items/prices/bulk
func (h *CatalogHandler) BulkSetCatalogPrices(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		OutletID string `json:"outlet_id,omitempty"`
		Prices   []struct {
			SKU          string  `json:"sku"`
			SellingPrice float64 `json:"selling_price"`
		} `json:"prices"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Prices) == 0 {
		jsonError(w, "prices array required", http.StatusBadRequest)
		return
	}

	var outletID *uuid.UUID
	if input.OutletID != "" {
		if oid, parseErr := uuid.Parse(input.OutletID); parseErr == nil {
			outletID = &oid
		}
	}

	skus := make([]string, len(input.Prices))
	for i, p := range input.Prices {
		skus[i] = p.SKU
	}
	existing, _ := h.client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid), entoverride.InventorySkuIn(skus...)).
		All(r.Context())
	existingBySKU := make(map[string]*ent.POSCatalogOverride, len(existing))
	for _, e := range existing {
		existingBySKU[e.InventorySku] = e
	}

	updated := 0
	for _, p := range input.Prices {
		if e, ok := existingBySKU[p.SKU]; ok {
			upd := e.Update().SetSellingPrice(p.SellingPrice)
			if outletID != nil {
				upd.SetOutletID(*outletID)
			}
			if _, saveErr := upd.Save(r.Context()); saveErr == nil {
				updated++
			}
		} else {
			creator := h.client.POSCatalogOverride.Create().
				SetTenantID(tid).SetInventorySku(p.SKU).
				SetSellingPrice(p.SellingPrice)
			if outletID != nil {
				creator.SetOutletID(*outletID)
			}
			if _, saveErr := creator.Save(r.Context()); saveErr == nil {
				updated++
			}
		}
	}
	jsonOK(w, map[string]any{"updated": updated})
}

// GetItemStock handles GET /{tenantID}/pos/catalog/items/{id}/stock
func (h *CatalogHandler) GetItemStock(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "id")
	if itemID == "" {
		jsonError(w, "item id required", http.StatusBadRequest)
		return
	}

	tenantSlug := httpware.GetTenantSlug(r.Context())

	url := fmt.Sprintf("%s/v1/%s/inventory/items/%s/stock", inventoryURL(), tenantSlug, itemID)
	body, err := doInventoryGET(r.Context(), url, "")
	if err != nil {
		h.log.Warn("inventory-api stock fetch failed", zap.Error(err), zap.String("item_id", itemID))
		jsonOK(w, map[string]any{"item_id": itemID, "quantity_on_hand": 0, "error": "inventory unavailable"})
		return
	}

	var stock map[string]any
	if err := json.Unmarshal(body, &stock); err != nil {
		jsonOK(w, map[string]any{"item_id": itemID, "raw": string(body)})
		return
	}
	jsonOK(w, stock)
}

// CreateCatalogItem handles POST /{tenantID}/pos/catalog/items — creates a POS price override.
func (h *CatalogHandler) CreateCatalogItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		SKU          string   `json:"sku"`
		SellingPrice *float64 `json:"selling_price,omitempty"`
		TaxStatus    string   `json:"tax_status,omitempty"`
		IsAvailable  *bool    `json:"is_available,omitempty"`
		OutletID     string   `json:"outlet_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SKU == "" {
		jsonError(w, "sku is required", http.StatusBadRequest)
		return
	}

	if input.TaxStatus == "" {
		input.TaxStatus = "taxable"
	}

	creator := h.client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku(input.SKU).
		SetTaxStatus(input.TaxStatus)
	if input.SellingPrice != nil {
		creator.SetSellingPrice(*input.SellingPrice)
	}
	if input.IsAvailable != nil {
		creator.SetIsAvailable(*input.IsAvailable)
	}
	if input.OutletID != "" {
		if oid, parseErr := uuid.Parse(input.OutletID); parseErr == nil {
			creator.SetOutletID(oid)
		}
	}
	item, err := creator.Save(r.Context())
	if err != nil {
		jsonError(w, "failed to create override: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, item)
}

// UpdateCatalogItem handles PUT /{tenantID}/pos/catalog/items/{id} — updates POS override.
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

	item, err := h.client.POSCatalogOverride.Query().
		Where(entoverride.ID(itemID), entoverride.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "override not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	updater := item.Update()
	if v, ok := input["tax_status"].(string); ok {
		updater.SetTaxStatus(v)
	}
	if v, ok := input["is_available"].(bool); ok {
		updater.SetIsAvailable(v)
	}
	if v, ok := input["selling_price"].(float64); ok {
		updater.SetSellingPrice(v)
	}
	if v, ok := input["is_featured"].(bool); ok {
		updater.SetIsFeatured(v)
	}

	updated, err := updater.Save(r.Context())
	if err != nil {
		jsonError(w, "update failed", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// DeleteCatalogItem handles DELETE /{tenantID}/pos/catalog/items/{id} — marks unavailable.
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

	_, err = h.client.POSCatalogOverride.Update().
		Where(entoverride.ID(itemID), entoverride.TenantID(tid)).
		SetIsAvailable(false).
		Save(r.Context())
	if err != nil {
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func mustParseUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}

// parseTenantUUID extracts and parses tenant UUID from httpware context.
func parseTenantUUID(r *http.Request) (uuid.UUID, error) {
	ctx := r.Context()

	if httpware.IsPlatformOwner(ctx) {
		if q := r.URL.Query().Get("tenantId"); q != "" {
			return uuid.Parse(q)
		}
	}

	tenantIDStr := httpware.GetTenantID(ctx)
	if tenantIDStr != "" {
		return uuid.Parse(tenantIDStr)
	}

	return uuid.Nil, fmt.Errorf("tenant context required")
}

// GetCatalogCategories handles GET /{tenantID}/pos/catalog/categories
// Proxies to inventory-api /inventory/categories and returns category list.
func (h *CatalogHandler) GetCatalogCategories(w http.ResponseWriter, r *http.Request) {
	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	if tenantSlug == "" {
		tenantSlug = httpware.GetTenantSlug(r.Context())
	}
	if tenantSlug == "" {
		tid, err := parseTenantUUID(r)
		if err == nil {
			if t, lookupErr := h.client.Tenant.Get(r.Context(), tid); lookupErr == nil {
				tenantSlug = t.Slug
			}
		}
	}
	if tenantSlug == "" {
		jsonError(w, "could not resolve tenant", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("%s/v1/%s/inventory/categories", inventoryURL(), tenantSlug)
	body, err := doInventoryGET(r.Context(), url, "")
	if err != nil {
		h.log.Warn("catalog categories: inventory proxy failed", zap.Error(err))
		// Return empty list rather than 404 so the UI degrades gracefully.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// parseOptionalUUID parses s as a UUID; on empty string or parse failure it falls
// back to the X-Outlet-ID request header (for outlet_id fields), then returns uuid.Nil.
func parseOptionalUUID(s string, r *http.Request) uuid.UUID {
	if id, err := uuid.Parse(s); err == nil {
		return id
	}
	if r != nil {
		if hv := r.Header.Get("X-Outlet-ID"); hv != "" {
			if id, err := uuid.Parse(hv); err == nil {
				return id
			}
		}
	}
	return uuid.Nil
}
