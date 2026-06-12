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
	"time"

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

// s2sHTTPClient is the shared HTTP client for service-to-service calls in this
// package. It carries a timeout so upstream calls cannot hang indefinitely.
var s2sHTTPClient = &http.Client{Timeout: 15 * time.Second}

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

// inventoryProxyVariant is the shape of an item variation surfaced by inventory-api.
type inventoryProxyVariant struct {
	ID         string            `json:"id"`
	SKU        string            `json:"sku"`
	Name       string            `json:"name"`
	Price      float64           `json:"price"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Barcode    string            `json:"barcode,omitempty"`
	IsActive   bool              `json:"is_active"`
}

// inventoryProxyItem is the shape returned by inventory-api /items list.
type inventoryProxyItem struct {
	ID                      string                  `json:"id"`
	SKU                     string                  `json:"sku"`
	Name                    string                  `json:"name"`
	Description             string                  `json:"description"`
	Type                    string                  `json:"type"`
	IsActive                bool                    `json:"is_active"`
	ImageURL                string                  `json:"image_url"`
	CategoryName            string                  `json:"category_name"`
	BrandID                 string                  `json:"brand_id"`
	BrandName               string                  `json:"brand_name"`
	BrandCode               string                  `json:"brand_code"`
	Manufacturer            string                  `json:"manufacturer"`
	Model                   string                  `json:"model"`
	HasVariants             bool                    `json:"has_variants"`
	Variants                []inventoryProxyVariant `json:"variants,omitempty"`
	Barcode                 string                  `json:"barcode"`
	RequiresAgeVerification bool                    `json:"requires_age_verification"`
	IsControlledSubstance   bool                    `json:"is_controlled_substance"`
	TrackSerialNumbers      bool                    `json:"track_serial_numbers"`
	DurationMinutes         int                     `json:"duration_minutes"`
	CostPrice               *float64                `json:"cost_price,omitempty"`
	SuggestedPrice          *float64                `json:"suggested_price,omitempty"`
	OnHand                  *float64                `json:"on_hand,omitempty"` // stock on hand from inventory balances (StockBadge)
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
		strings.Contains(cat, "wellness") || strings.Contains(cat, "conference") ||
		strings.Contains(cat, "meeting") || strings.Contains(cat, "facility") ||
		strings.Contains(cat, "amenity") || strings.Contains(cat, "salon") ||
		strings.Contains(cat, "massage") || strings.Contains(cat, "room rate") ||
		strings.Contains(cat, "room type")
	isRetailCat := strings.Contains(cat, "retail")
	// General non-food retail MERCHANDISE (detergents, cleaning, household, electronics, apparel,
	// cosmetics, stationery, etc.) — sold by retail outlets but NEVER on a restaurant/bar/services
	// terminal. This is why supermarket items like "Detergents & Cleaning" were leaking onto the
	// hospitality menu: they aren't food/pharmacy/services/component, so the old blocklist let them
	// through. They belong to retail only.
	isRetailMerchandiseCat := strings.Contains(cat, "detergent") || strings.Contains(cat, "cleaning") ||
		strings.Contains(cat, "cleaner") || strings.Contains(cat, "household") || strings.Contains(cat, "home care") ||
		strings.Contains(cat, "homeware") || strings.Contains(cat, "kitchenware") || strings.Contains(cat, "hardware") ||
		strings.Contains(cat, "electronic") || strings.Contains(cat, "appliance") || strings.Contains(cat, "apparel") ||
		strings.Contains(cat, "clothing") || strings.Contains(cat, "fashion") || strings.Contains(cat, "footwear") ||
		strings.Contains(cat, "cosmetic") || strings.Contains(cat, "toiletr") || strings.Contains(cat, "stationery") ||
		strings.Contains(cat, "personal care") || strings.Contains(cat, "furniture") || strings.Contains(cat, "grocery")
	// Component categories are building blocks (recipe inputs / modifier options), never standalone
	// sellable on a POS terminal — keep them off every sellable use case so the catalog shows only
	// finished items (menu dishes, drinks, packaged goods). NOTE: "Accompaniment" is intentionally
	// NOT here — accompaniments (e.g. Ugali, rice, fries served as sides) are real sellable RECIPE
	// menu items and must appear on the POS.
	isComponentCat := strings.Contains(cat, "raw ingredient") || strings.Contains(cat, "raw material") ||
		strings.Contains(cat, "ingredient") || strings.Contains(cat, "modifier") ||
		strings.Contains(cat, "add-on") || strings.Contains(cat, "add on")

	switch strings.ToLower(useCase) {
	case "retail":
		// Retail outlets sell general merchandise — exclude pharmacy, food/restaurant, services, components
		return !isPharmacyCat && !isFoodCat && !isServicesCat && !isComponentCat
	case "pharmacy":
		// Pharmacy outlets sell only pharmacy/health items
		return isPharmacyCat
	case "hospitality", "quick_service":
		// Restaurant/QSR outlets sell FINISHED food/drink — exclude pharmacy, retail, retail
		// merchandise (detergents/cleaning/household/etc.), services and components.
		return !isPharmacyCat && !isRetailCat && !isRetailMerchandiseCat && !isServicesCat && !isComponentCat
	case "services":
		// Beauty/wellness outlets sell services — exclude pharmacy, food, retail merchandise, components
		return !isPharmacyCat && !isFoodCat && !isRetailCat && !isRetailMerchandiseCat && !isComponentCat
	default:
		return true
	}
}

// useCaseItemTypes returns the comma-separated inventory item types valid for a given outlet use case.
// This prevents cross-contamination (e.g. pharmacy GOODS appearing in retail) AND keeps non-sellable
// component types off the POS terminal: restaurants/bars sell FINISHED items only — RECIPE (menu
// dishes), GOODS (drinks/packaged) and VOUCHER — never raw INGREDIENT stock or SERVICE items (rooms/
// conference/salon are handled by their own modules, not the food terminal).
func useCaseItemTypes(useCase string) string {
	switch strings.ToLower(useCase) {
	case "retail":
		return "GOODS,VOUCHER"
	case "hospitality", "quick_service":
		return "GOODS,RECIPE,VOUCHER"
	case "pharmacy":
		return "GOODS,VOUCHER"
	case "services":
		return "SERVICE,GOODS,VOUCHER"
	default:
		return "GOODS,RECIPE,VOUCHER"
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
	resp, err := s2sHTTPClient.Do(req)
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
	// include=variants so the catalog item carries its sellable variations.
	url := fmt.Sprintf("%s/v1/%s/inventory/items?type=%s&status=active&limit=500&include=variants", inventoryURL(), tenantSlug, types)
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
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	SKU         string  `json:"sku"`
	PackageType string  `json:"package_type"`
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

// posPricingTier is the clean shape pos-ui consumes for the price-profile selector.
type posPricingTier struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
	SortOrder int    `json:"sort_order"`
}

// GetPricingTiers handles GET /{tenantID}/pos/catalog/pricing/tiers — proxies the tenant's pricing
// tiers from inventory-api (the source of truth: Retail, Wholesale, and any custom tiers like
// "Loyal Clients") so the POS price-profile selector reflects real configured tiers instead of a
// hard-coded Retail/Wholesale toggle. Returns active tiers only, sorted; degrades to {"data":[]}.
func (h *CatalogHandler) GetPricingTiers(w http.ResponseWriter, r *http.Request) {
	tenantSlug := h.resolveTenantSlug(r)
	if tenantSlug == "" {
		jsonError(w, "could not resolve tenant", http.StatusBadRequest)
		return
	}
	url := fmt.Sprintf("%s/v1/%s/inventory/pricing-tiers", inventoryURL(), tenantSlug)
	body, err := doInventoryGET(r.Context(), url, "")
	if err != nil {
		h.log.Warn("pricing tiers: inventory proxy failed", zap.Error(err))
		jsonOK(w, map[string]any{"data": []posPricingTier{}})
		return
	}
	// inventory returns a bare array of {id,name,code,is_default,is_active,sort_order}.
	var raw []struct {
		Name      string `json:"name"`
		Code      string `json:"code"`
		IsDefault bool   `json:"is_default"`
		IsActive  bool   `json:"is_active"`
		SortOrder int    `json:"sort_order"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		// Tolerate a wrapped {"data":[...]} response too.
		var wrapper struct {
			Data []struct {
				Name      string `json:"name"`
				Code      string `json:"code"`
				IsDefault bool   `json:"is_default"`
				IsActive  bool   `json:"is_active"`
				SortOrder int    `json:"sort_order"`
			} `json:"data"`
		}
		if werr := json.Unmarshal(body, &wrapper); werr != nil {
			jsonOK(w, map[string]any{"data": []posPricingTier{}})
			return
		}
		for _, t := range wrapper.Data {
			raw = append(raw, struct {
				Name      string `json:"name"`
				Code      string `json:"code"`
				IsDefault bool   `json:"is_default"`
				IsActive  bool   `json:"is_active"`
				SortOrder int    `json:"sort_order"`
			}(t))
		}
	}
	out := make([]posPricingTier, 0, len(raw))
	for _, t := range raw {
		if !t.IsActive || strings.TrimSpace(t.Code) == "" {
			continue
		}
		out = append(out, posPricingTier{Code: t.Code, Name: t.Name, IsDefault: t.IsDefault, SortOrder: t.SortOrder})
	}
	jsonOK(w, map[string]any{"data": out})
}

// posBrand is the clean shape pos-ui consumes for the Brands tab.
type posBrand struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	LogoURL   string `json:"logo_url,omitempty"`
	SortOrder int    `json:"sort_order"`
}

// GetBrands handles GET /{tenantID}/pos/catalog/brands — proxies the tenant's item brands from
// inventory-api (the source of truth) so the POS catalog can offer a real Brands tab / filter
// instead of nothing. Returns active brands only, sorted; degrades to {"data":[]} on proxy failure.
func (h *CatalogHandler) GetBrands(w http.ResponseWriter, r *http.Request) {
	tenantSlug := h.resolveTenantSlug(r)
	if tenantSlug == "" {
		jsonError(w, "could not resolve tenant", http.StatusBadRequest)
		return
	}
	url := fmt.Sprintf("%s/v1/%s/inventory/brands", inventoryURL(), tenantSlug)
	body, err := doInventoryGET(r.Context(), url, "")
	if err != nil {
		h.log.Warn("brands: inventory proxy failed", zap.Error(err))
		jsonOK(w, map[string]any{"data": []posBrand{}})
		return
	}
	// inventory returns a wrapped {"data":[{code,name,logo_url,is_active,sort_order}]}; tolerate a bare array too.
	type rawBrand struct {
		Name      string `json:"name"`
		Code      string `json:"code"`
		LogoURL   string `json:"logo_url"`
		IsActive  bool   `json:"is_active"`
		SortOrder int    `json:"sort_order"`
	}
	var raw []rawBrand
	var wrapper struct {
		Data []rawBrand `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Data != nil {
		raw = wrapper.Data
	} else if err := json.Unmarshal(body, &raw); err != nil {
		jsonOK(w, map[string]any{"data": []posBrand{}})
		return
	}
	out := make([]posBrand, 0, len(raw))
	for _, b := range raw {
		if !b.IsActive || strings.TrimSpace(b.Code) == "" {
			continue
		}
		out = append(out, posBrand{Code: b.Code, Name: b.Name, LogoURL: b.LogoURL, SortOrder: b.SortOrder})
	}
	jsonOK(w, map[string]any{"data": out})
}

// ResolvePrice handles GET /{tenantID}/pos/catalog/pricing/resolve?item_id=&quantity=&profile=
// Resolves an item's unit/total price for a pricing profile (tier code, e.g. RETAIL or WHOLESALE)
// from inventory-api, so the register can re-price the cart when the cashier switches profile.
// Falls back to the default tier when no profile is given. Returns inventory's price DTO verbatim.
func (h *CatalogHandler) ResolvePrice(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	itemID := strings.TrimSpace(r.URL.Query().Get("item_id"))
	if itemID == "" {
		jsonError(w, "item_id is required", http.StatusBadRequest)
		return
	}
	quantity := 1
	if q, e := strconv.Atoi(r.URL.Query().Get("quantity")); e == nil && q > 0 {
		quantity = q
	}
	profile := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("profile")))

	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	if tenantSlug == "" {
		tenantSlug = httpware.GetTenantSlug(r.Context())
	}
	if tenantSlug == "" {
		if t, lookupErr := h.client.Tenant.Get(r.Context(), tid); lookupErr == nil {
			tenantSlug = t.Slug
		}
	}
	if tenantSlug == "" {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("%s/v1/%s/inventory/items/%s/price?quantity=%d", inventoryURL(), tenantSlug, itemID, quantity)
	if profile != "" {
		url += "&tier=" + profile
	}
	body, err := doInventoryGET(r.Context(), url, httpware.GetOutletID(r.Context()))
	if err != nil {
		h.log.Warn("resolve price: inventory call failed", zap.String("item_id", itemID), zap.Error(err))
		jsonError(w, "failed to resolve price", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// catalogItemDTO is the fully-assembled, override-merged catalog item used by both
// ListCatalogItems (JSON list) and the menu document renderer. It carries the
// inventory-sourced display fields (name/description/image/category) plus the
// POS-resolved selling price, availability and compliance flags.
type catalogItemDTO struct {
	ID                      string
	SKU                     string
	Name                    string
	Description             string
	CategoryName            string
	BrandName               string
	BrandCode               string
	Manufacturer            string
	Model                   string
	HasVariants             bool
	Variants                []inventoryProxyVariant
	ItemType                string
	IsActive                bool
	IsAvailable             bool
	// IsComplimentary marks a no-charge accompaniment: price 0 but explicitly enabled, shown as
	// "Free" and not charged, while its recipe/BOM stock is still deducted on sale.
	IsComplimentary         bool
	IsFeatured              bool
	DisplayOrder            int
	ImageURL                string
	Barcode                 string
	Price                   float64
	TaxStatus               string
	TaxCodeID               string
	TaxInclusive            bool
	TaxRate                 *float64
	NetPrice                *float64
	TaxAmount               *float64
	RequiresPrescription    bool
	IsReturnable            bool
	RequiresAgeVerification bool
	IsControlledSubstance   bool
	TrackSerialNumbers      bool
	MinimumAge              *int
	DurationMinutes         *int
	StockQuantity           *float64
}

// menuAssemblyFilters carries the optional list-time filters applied by ListCatalogItems.
// The menu document renderer leaves these empty (it wants the full active menu).
type menuAssemblyFilters struct {
	Category string // case-insensitive exact match on category name
	Search   string // case-insensitive substring match on item name (already lower-cased)
	ItemType string // comma-separated, case-insensitive match on item type (already upper-cased)
}

// assembleMenuItems is the single source of truth for turning inventory-api items +
// POS overrides into display-ready catalog items. It performs the inventory items +
// pricing fetch, override merge, use-case category filtering, and price resolution.
// Both ListCatalogItems and GetMenuHTML call this so the two surfaces never drift.
//
// tenantSlug/useCase/outletID are resolved by the caller (handlers differ in how they
// obtain them — auth claims vs. path-derived outlet). outletID may be nil for tenant-wide.
func (h *CatalogHandler) assembleMenuItems(
	ctx context.Context,
	tid uuid.UUID,
	tenantSlug string,
	outletID *uuid.UUID,
	useCase string,
	filters menuAssemblyFilters,
) ([]catalogItemDTO, error) {
	outletIDStr := ""
	if outletID != nil {
		outletIDStr = outletID.String()
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
		items, err := fetchInventoryItems(ctx, tenantSlug, outletIDStr, useCase)
		itemsCh <- itemsResult{items, err}
	}()
	go func() {
		prices, err := fetchInventoryPricing(ctx, tenantSlug, outletIDStr)
		priceCh <- priceResult{prices, err}
	}()

	ir := <-itemsCh
	pr := <-priceCh
	if ir.err != nil {
		return nil, ir.err
	}
	if pr.err != nil {
		h.log.Warn("inventory pricing fetch failed — prices will be 0", zap.Error(pr.err))
	}
	invPriceByID := pr.prices

	// Load all POS overrides for this tenant
	overrides, _ := h.client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid)).
		All(ctx)

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
		complimentary           bool
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
				complimentary:           metaBool(o.Metadata, "complimentary"),
			}
		} else {
			_ = prev
		}
	}

	out := make([]catalogItemDTO, 0, len(ir.items))
	for _, item := range ir.items {
		// Apply filters
		if filters.Category != "" && !strings.EqualFold(item.CategoryName, filters.Category) {
			continue
		}
		if filters.Search != "" && !strings.Contains(strings.ToLower(item.Name), filters.Search) {
			continue
		}
		if filters.ItemType != "" {
			matched := false
			for _, t := range strings.Split(filters.ItemType, ",") {
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
		isComplimentary := false
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

		// Zero-price safety gate, with a deliberate exception for COMPLIMENTARY accompaniments.
		// By default a no-price item must not appear sellable — staff must not ring up a KES 0 item by
		// accident. BUT a price-0 item that an admin has EXPLICITLY enabled via a POS catalog override
		// (o.isAvailable) is treated as a no-charge accompaniment: it stays available, is flagged so
		// the UI/receipt show "Free", and is not charged — while its recipe/BOM stock is STILL deducted
		// on sale (the inventory pos.sale.finalized consumer deducts by recipe/modifier SKU regardless
		// of price). This lets outlets offer bundled sides (ugali, greens, a free side) that consume
		// stock without billing the customer.
		if price == 0 {
			if hasOverride && o.complimentary {
				// Explicitly configured no-charge accompaniment (override metadata.complimentary=true):
				// show it, force KES 0, and flag it Free. Recipe/BOM stock is still deducted on sale.
				isComplimentary = true
				isAvailable = true
			} else {
				isAvailable = false
			}
		}

		out = append(out, catalogItemDTO{
			ID:                      item.ID,
			SKU:                     item.SKU,
			Name:                    item.Name,
			Description:             item.Description,
			CategoryName:            item.CategoryName,
			BrandName:               item.BrandName,
			BrandCode:               item.BrandCode,
			Manufacturer:            item.Manufacturer,
			Model:                   item.Model,
			HasVariants:             item.HasVariants,
			Variants:                item.Variants,
			ItemType:                item.Type,
			IsActive:                item.IsActive,
			IsAvailable:             isAvailable,
			IsComplimentary:         isComplimentary,
			IsFeatured:              isFeatured,
			DisplayOrder:            displayOrder,
			ImageURL:                item.ImageURL,
			Barcode:                 item.Barcode,
			Price:                   price,
			TaxStatus:               taxStatus,
			TaxCodeID:               item.TaxCodeID,
			TaxInclusive:            item.TaxInclusive,
			TaxRate:                 item.TaxRate,
			NetPrice:                item.NetPrice,
			TaxAmount:               item.TaxAmount,
			RequiresPrescription:    requiresPrescription,
			IsReturnable:            isReturnable,
			RequiresAgeVerification: requiresAgeVerification,
			IsControlledSubstance:   isControlledSubstance,
			TrackSerialNumbers:      item.TrackSerialNumbers,
			MinimumAge:              minimumAge,
			DurationMinutes:         durationMinutes,
			StockQuantity:           item.OnHand,
		})
	}
	return out, nil
}

// metaBool reads a boolean flag from a POSCatalogOverride.metadata map (tolerating bool or the
// string "true"). Used for opt-in per-item flags (e.g. "complimentary") that live in the existing
// metadata JSON column, so no schema migration is needed to add new toggles.
func metaBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	default:
		return false
	}
}

// catalogItemToMap converts an assembled DTO into the JSON map shape ListCatalogItems
// returns. Kept here so the wire format stays identical after the assembleMenuItems refactor.
func catalogItemToMap(item catalogItemDTO, outletID *uuid.UUID) map[string]any {
	return map[string]any{
		"id":                        item.ID,
		"sku":                       item.SKU,
		"name":                      item.Name,
		"description":               item.Description,
		"category":                  item.CategoryName,
		"brand":                     item.BrandName,
		"brand_code":                item.BrandCode,
		"manufacturer":              item.Manufacturer,
		"model":                     item.Model,
		"has_variants":              item.HasVariants,
		"variants":                  item.Variants,
		"item_type":                 item.ItemType,
		"status":                    map[bool]string{true: "active", false: "inactive"}[item.IsActive],
		"is_available":              item.IsAvailable,
		"is_complimentary":          item.IsComplimentary,
		"is_featured":               item.IsFeatured,
		"display_order":             item.DisplayOrder,
		"image_url":                 item.ImageURL,
		"barcode":                   item.Barcode,
		"price":                     item.Price,
		"tax_status":                item.TaxStatus,
		"tax_code_id":               item.TaxCodeID,
		"tax_inclusive":             item.TaxInclusive,
		"tax_rate":                  item.TaxRate,
		"net_price":                 item.NetPrice,
		"tax_amount":                item.TaxAmount,
		"requires_prescription":     item.RequiresPrescription,
		"is_returnable":             item.IsReturnable,
		"requires_age_verification": item.RequiresAgeVerification,
		"is_controlled_substance":   item.IsControlledSubstance,
		"track_serial_numbers":      item.TrackSerialNumbers,
		"minimum_age":               item.MinimumAge,
		"duration_minutes":          item.DurationMinutes,
		"stock_quantity":            item.StockQuantity,
		"outlet_id":                 outletID,
	}
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

	// outletID drives override precedence + the JSON outlet_id field (header/query derived).
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

	// Resolve use case from outlet context for item type filtering. scopeOutletID is the
	// outlet used for the inventory scope header + override precedence (context wins).
	useCase := ""
	scopeOutletID := outletID
	if oc := middleware.OutletFromContext(r.Context()); oc != nil {
		useCase = oc.UseCase
		ocID := oc.ID
		scopeOutletID = &ocID
	}

	if tenantSlug == "" {
		jsonError(w, "tenant slug required", http.StatusBadRequest)
		return
	}

	items, err := h.assembleMenuItems(r.Context(), tid, tenantSlug, scopeOutletID, useCase, menuAssemblyFilters{
		Category: catFilter,
		Search:   searchFilter,
		ItemType: itemTypeFilter,
	})
	if err != nil {
		h.log.Error("inventory items fetch failed", zap.Error(err))
		jsonError(w, "failed to fetch catalog from inventory", http.StatusBadGateway)
		return
	}

	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, catalogItemToMap(item, outletID))
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
		// Complimentary marks a no-charge accompaniment (price 0 shown as "Free", still deducts BOM
		// stock). Stored in the override metadata so no schema migration is needed. When true the
		// override is forced available so the free item is selectable on the terminal.
		Complimentary *bool `json:"complimentary,omitempty"`
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
		if input.Complimentary != nil {
			meta := existing.Metadata
			if meta == nil {
				meta = map[string]any{}
			}
			meta["complimentary"] = *input.Complimentary
			upd.SetMetadata(meta)
			if *input.Complimentary {
				upd.SetIsAvailable(true) // a no-charge item must be visible to be added free
			}
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
	if input.Complimentary != nil {
		creator.SetMetadata(map[string]any{"complimentary": *input.Complimentary})
		if *input.Complimentary {
			creator.SetIsAvailable(true)
		}
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

// inventoryCategory is the raw inventory-api category shape.
type inventoryCategory struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Code        string `json:"code"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	IsActive    bool   `json:"is_active"`
}

// posCategory is the clean, typed shape pos-ui consumes. Icon carries an emoji / icon-class name;
// ImageURL is set instead when the icon resolves to an image URL so the UI can choose <img> vs text.
type posCategory struct {
	Name     string `json:"name"`
	Icon     string `json:"icon,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// iconLooksLikeImageURL reports whether an inventory icon value should render as an <img>.
func iconLooksLikeImageURL(icon string) bool {
	s := strings.TrimSpace(strings.ToLower(icon))
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "/") {
		return true
	}
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg"} {
		if strings.HasSuffix(s, ext) {
			return true
		}
	}
	return false
}

// mapInventoryCategory splits the polymorphic inventory icon into Icon (emoji/text) vs ImageURL.
func mapInventoryCategory(c inventoryCategory) posCategory {
	out := posCategory{Name: c.Name}
	if iconLooksLikeImageURL(c.Icon) {
		out.ImageURL = strings.TrimSpace(c.Icon)
	} else {
		out.Icon = strings.TrimSpace(c.Icon)
	}
	return out
}

// resolveTenantSlug derives the tenant slug from auth claims, httpware context, or a tenant lookup.
func (h *CatalogHandler) resolveTenantSlug(r *http.Request) string {
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
	return tenantSlug
}

// fetchInventoryCategories proxies inventory-api categories and decodes them, tolerating both the
// wrapped ({"data":[...]}) and bare-array response forms.
func (h *CatalogHandler) fetchInventoryCategories(ctx context.Context, tenantSlug string) ([]inventoryCategory, error) {
	url := fmt.Sprintf("%s/v1/%s/inventory/categories", inventoryURL(), tenantSlug)
	body, err := doInventoryGET(ctx, url, "")
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Data []inventoryCategory `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Data != nil {
		return wrapper.Data, nil
	}
	var bare []inventoryCategory
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, fmt.Errorf("decode inventory categories: %w", err)
	}
	return bare, nil
}

// GetCatalogCategories handles GET /{tenantID}/pos/catalog/categories — returns a clean typed list
// {"data":[{name, icon, image_url?}]} of categories that actually have ≥1 sellable item for THIS
// outlet's use case. Previously it proxied every active inventory category unfiltered, so categories
// with zero items for the outlet (e.g. pharmacy categories on a retail outlet, or empty bulk-import
// categories) showed up and returned no items when tapped. We now intersect inventory categories
// with the use-case-filtered item set so categories are always tied to items. Degrades to {"data":[]}
// on proxy failure so the UI stays graceful.
func (h *CatalogHandler) GetCatalogCategories(w http.ResponseWriter, r *http.Request) {
	tenantSlug := h.resolveTenantSlug(r)
	if tenantSlug == "" {
		jsonError(w, "could not resolve tenant", http.StatusBadRequest)
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	// Resolve outlet scope + use case the same way ListCatalogItems does (context wins, query fallback).
	var scopeOutletID *uuid.UUID
	useCase := ""
	if oc := middleware.OutletFromContext(r.Context()); oc != nil {
		ocID := oc.ID
		scopeOutletID = &ocID
		useCase = oc.UseCase
	}
	if scopeOutletID == nil {
		if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
			if oid, perr := uuid.Parse(oidStr); perr == nil {
				scopeOutletID = &oid
			}
		}
	}
	if scopeOutletID == nil {
		if oidStr := r.URL.Query().Get("outlet_id"); oidStr != "" {
			if oid, perr := uuid.Parse(oidStr); perr == nil {
				scopeOutletID = &oid
			}
		}
	}

	// Build the set of category names that have at least one sellable item for this use case.
	items, err := h.assembleMenuItems(r.Context(), tid, tenantSlug, scopeOutletID, useCase, menuAssemblyFilters{})
	if err != nil {
		h.log.Warn("catalog categories: item assembly failed", zap.Error(err))
		jsonOK(w, map[string]any{"data": []posCategory{}})
		return
	}
	withItems := make(map[string]bool, len(items))
	for _, it := range items {
		name := strings.TrimSpace(it.CategoryName)
		if name != "" {
			withItems[strings.ToLower(name)] = true
		}
	}
	if len(withItems) == 0 {
		jsonOK(w, map[string]any{"data": []posCategory{}})
		return
	}

	// Fetch inventory categories for their icon/image metadata, then keep only those that have items.
	cats, err := h.fetchInventoryCategories(r.Context(), tenantSlug)
	if err != nil {
		// Inventory category metadata unavailable — still return the category names we derived from
		// items (without icons) so the UI filters work.
		h.log.Warn("catalog categories: inventory proxy failed; deriving names from items", zap.Error(err))
		out := make([]posCategory, 0, len(withItems))
		seen := make(map[string]bool, len(items))
		for _, it := range items {
			name := strings.TrimSpace(it.CategoryName)
			if name == "" || seen[strings.ToLower(name)] {
				continue
			}
			seen[strings.ToLower(name)] = true
			out = append(out, posCategory{Name: name})
		}
		jsonOK(w, map[string]any{"data": out})
		return
	}

	out := make([]posCategory, 0, len(cats))
	emitted := make(map[string]bool, len(cats))
	for _, c := range cats {
		if !c.IsActive {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(c.Name))
		if !withItems[key] || emitted[key] {
			continue
		}
		emitted[key] = true
		out = append(out, mapInventoryCategory(c))
	}
	// Cover categories present on items but missing from the inventory category list (defensive).
	for _, it := range items {
		name := strings.TrimSpace(it.CategoryName)
		key := strings.ToLower(name)
		if name == "" || emitted[key] {
			continue
		}
		emitted[key] = true
		out = append(out, posCategory{Name: name})
	}
	jsonOK(w, map[string]any{"data": out})
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
