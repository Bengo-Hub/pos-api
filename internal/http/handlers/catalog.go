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
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	entkdsstation "github.com/bengobox/pos-service/internal/ent/kdsstation"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
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
	rbac   middleware.PermissionChecker
	// sweepGroup coalesces concurrent cachedCatalogSource cache-misses for the SAME
	// (tenant, outlet, use-case-set) key onto a single in-flight inventory-api sweep — see the
	// doc comment on cachedCatalogSource for why this matters for a large catalog.
	sweepGroup singleflight.Group
}

func NewCatalogHandler(log *zap.Logger, client *ent.Client) *CatalogHandler {
	return &CatalogHandler{log: log, client: client}
}

// SetRBAC wires the RBAC checker used to gate cost-price exposure. Cost price (supplier cost) is only
// serialized to callers holding pos.catalog.view_cost (manager/admin) — never to cashier terminals.
func (h *CatalogHandler) SetRBAC(rbac middleware.PermissionChecker) { h.rbac = rbac }

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
	ID           string `json:"id"`
	SKU          string `json:"sku"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Type         string `json:"type"`
	IsActive     bool   `json:"is_active"`
	ImageURL     string `json:"image_url"`
	CategoryName string `json:"category_name"`
	BrandID      string `json:"brand_id"`
	BrandName    string `json:"brand_name"`
	BrandCode    string `json:"brand_code"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	// Drug-master fields (pharmacy) — let the POS prescription drug picker auto-fill
	// dosage/form from the catalog item instead of requiring manual re-entry.
	DosageForm              string                  `json:"dosage_form,omitempty"`
	Strength                string                  `json:"strength,omitempty"`
	HasVariants             bool                    `json:"has_variants"`
	Variants                []inventoryProxyVariant `json:"variants,omitempty"`
	Barcode                 string                  `json:"barcode"`
	RequiresAgeVerification bool                    `json:"requires_age_verification"`
	IsControlledSubstance   bool                    `json:"is_controlled_substance"`
	TrackSerialNumbers      bool                    `json:"track_serial_numbers"`
	// NonBillable: never charged at the till even when a selling price exists (free
	// accompaniments like ugali, supplies like tissue/packaging); stock still deducts.
	NonBillable *bool `json:"non_billable,omitempty"`
	// NotForSale: inventory-only stock (ingredients, internal supplies) that must NEVER
	// appear on any POS sales surface — excluded from the terminal catalog, menus and
	// barcode lookup entirely (unlike NonBillable, which stays sellable at KES 0).
	NotForSale      bool `json:"not_for_sale"`
	DurationMinutes int  `json:"duration_minutes"`
	// Hospitality / bookable-service fields (SERVICE items: co-working, conference,
	// facility, amenity, salon). use_case drives how the terminal presents/books the item;
	// total/booked_capacity model shared-seating availability (co-working desks).
	UseCase         string   `json:"use_case,omitempty"`
	TotalCapacity   *int     `json:"total_capacity,omitempty"`
	BookedCapacity  *int     `json:"booked_capacity,omitempty"`
	CostPrice       *float64 `json:"cost_price,omitempty"`
	SuggestedPrice  *float64 `json:"suggested_price,omitempty"`
	MinSellingPrice *float64 `json:"min_selling_price,omitempty"` // hard floor enforced at sale
	MaxSellingPrice *float64 `json:"max_selling_price,omitempty"` // hard ceiling enforced at sale
	OnHand          *float64 `json:"on_hand,omitempty"`           // stock on hand from inventory balances (StockBadge)
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
	// ModifierGroups: this item's selectable modifiers (e.g. "Extra Honey" on a Dawa),
	// enriched by inventory-api. Passed straight through to the terminal — see
	// catalogModifierGroup for the exact wire shape the modal expects.
	ModifierGroups []inventoryProxyModifierGroup `json:"modifier_groups,omitempty"`
}

// inventoryProxyModifierOption mirrors inventory-api's items.ItemModifierOption.
type inventoryProxyModifierOption struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	SKU             string  `json:"sku,omitempty"`
	PriceAdjustment float64 `json:"price_adjustment"`
	IsDefault       bool    `json:"is_default"`
}

// inventoryProxyModifierGroup mirrors inventory-api's items.ItemModifierGroup.
type inventoryProxyModifierGroup struct {
	ID            string                         `json:"id"`
	Name          string                         `json:"name"`
	IsRequired    bool                           `json:"is_required"`
	MinSelections int                            `json:"min_selections"`
	MaxSelections int                            `json:"max_selections"`
	Options       []inventoryProxyModifierOption `json:"options"`
}

// inventoryBulkPrice is one entry from GET /inventory/items/pricing.
// Price/TierCode = default tier; Prices = every active tier keyed by code (RETAIL, WHOLESALE…).
type inventoryBulkPrice struct {
	ItemID   string             `json:"item_id"`
	Price    float64            `json:"price"`
	Currency string             `json:"currency"`
	TierCode string             `json:"tier_code"`
	Prices   map[string]float64 `json:"prices"`
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
// isPharmacyCategory matches any category name a tenant might plausibly have chosen for their
// drug/health catalog (the seeded default is "Pharmacy", but tenants rename categories freely) —
// shared by categoryAllowedForUseCase (use-case gating) and the /catalog/items ?category= filter
// (pharmacyCategoryAliases below), so a pharmacy-flavored search never depends on matching one
// exact, hardcoded string.
func isPharmacyCategory(categoryName string) bool {
	cat := strings.ToLower(strings.TrimSpace(categoryName))
	return strings.Contains(cat, "pharmacy") || strings.Contains(cat, "chemist") || strings.Contains(cat, "drug") || strings.Contains(cat, "medicine") || strings.Contains(cat, "pharmaceutical") || strings.Contains(cat, "medication")
}

// pharmacyCategoryAliases are ?category= values a caller might reasonably send when searching for
// drugs — matched via isPharmacyCategory (substring/alias) instead of the strict exact-match every
// other category filter value uses, since the actual seeded/tenant category name ("Pharmacy") never
// equals generic terms like "pharmaceutical" or "medication" a client might search with.
var pharmacyCategoryAliases = map[string]bool{
	"pharmacy": true, "pharmaceutical": true, "pharmaceuticals": true,
	"medication": true, "medications": true, "drug": true, "drugs": true, "chemist": true, "medicine": true,
}

func categoryAllowedForUseCase(categoryName, useCase string) bool {
	cat := strings.ToLower(strings.TrimSpace(categoryName))
	if cat == "" || useCase == "" {
		return true
	}
	isPharmacyCat := isPharmacyCategory(categoryName)
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
		strings.Contains(cat, "cold beverage") ||
		// Accompaniments/sides (ugali, greens, fries) are FOOD menu items — they belong to
		// hospitality/QSR terminals only. Without this they slipped the retail exclusion
		// (nothing above matched "accompaniment") and free ACC items leaked onto retail tills.
		isAccompanimentCategory(cat)
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
		// RECIPE included: manufactured finished products (e.g. own-brand detergents made in
		// the production facility) are RECIPE-typed but sold at retail. Food/menu RECIPEs are
		// still kept off retail tills by the category gate (categoryAllowedForUseCase).
		return "GOODS,RECIPE,VOUCHER"
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

// isBookableServiceUseCase reports whether an inventory item's use_case marks it as a
// bookable service (co-working desk, conference/meeting room, facility, amenity, salon
// service) rather than a plain ring-up product. The terminal uses this to offer an
// "assign a space / book a session" flow and to show capacity, while still allowing a
// quick day-pass sale through the normal order pipeline.
func isBookableServiceUseCase(useCase string) bool {
	switch strings.ToUpper(strings.TrimSpace(useCase)) {
	case "HOSPITALITY_FACILITY", "CONFERENCE", "SALON_SERVICE", "AMENITY", "HOSPITALITY_ROOM":
		return true
	default:
		return false
	}
}

// effectiveCatalogUseCases returns the outlet's primary use_case plus any hybrid
// use_cases enabled on OutletSetting.catalog_use_cases (deduped, lower-cased), in
// order. A hospitality cafe that also sells co-working/conference SERVICE packages
// enables "services" here so its terminal unions both allow-lists instead of being
// reclassified away from "hospitality". Empty primary + no extras returns nil,
// preserving the legacy "no use-case filtering" behavior.
func effectiveCatalogUseCases(primary string, extra []string) []string {
	seen := make(map[string]bool, 1+len(extra))
	out := make([]string, 0, 1+len(extra))
	add := func(uc string) {
		uc = strings.ToLower(strings.TrimSpace(uc))
		if uc == "" || seen[uc] {
			return
		}
		seen[uc] = true
		out = append(out, uc)
	}
	add(primary)
	for _, uc := range extra {
		add(uc)
	}
	return out
}

// useCaseItemTypesForSet unions useCaseItemTypes across every use_case in the set,
// so a hybrid outlet's inventory-api item fetch requests every type any of its
// enabled use_cases would allow (e.g. hospitality's GOODS/RECIPE/VOUCHER PLUS
// services' SERVICE). Falls back to the single-use_case default when the set is empty.
func useCaseItemTypesForSet(useCases []string) string {
	if len(useCases) == 0 {
		return useCaseItemTypes("")
	}
	seen := make(map[string]bool, 4)
	order := make([]string, 0, 4)
	for _, uc := range useCases {
		for _, t := range strings.Split(useCaseItemTypes(uc), ",") {
			t = strings.TrimSpace(t)
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			order = append(order, t)
		}
	}
	return strings.Join(order, ",")
}

// categoryAllowedForUseCaseSet allows a category through if ANY use_case in the set
// allows it — the hybrid counterpart to categoryAllowedForUseCase.
func categoryAllowedForUseCaseSet(categoryName string, useCases []string) bool {
	if len(useCases) == 0 {
		return true
	}
	for _, uc := range useCases {
		if categoryAllowedForUseCase(categoryName, uc) {
			return true
		}
	}
	return false
}

// resolveExtraCatalogUseCases loads the outlet's hybrid catalog_use_cases from
// OutletSetting, gated on the tenant holding the facility_booking subscription
// feature. This is the plan-gating hook: an outlet admin can configure
// catalog_use_cases=["services"] at any time, but it only takes effect once the
// tenant's plan includes facility_booking — so upgrading a subscription "picks up"
// the hybrid catalog automatically, with zero further config, and downgrading
// silently falls back to the primary use_case only (fail-closed, never a hard
// failure). A missing/errored settings row is likewise treated as "no extras".
func (h *CatalogHandler) resolveExtraCatalogUseCases(ctx context.Context, outletID *uuid.UUID) []string {
	if outletID == nil {
		return nil
	}
	claims, ok := authclient.ClaimsFromContext(ctx)
	if !ok || !claims.HasFeature(subscriptions.FeatureFacilityBooking) {
		return nil
	}
	s, err := h.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(*outletID)).
		Only(ctx)
	if err != nil {
		return nil
	}
	return s.CatalogUseCases
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

// inventoryPageSize matches inventory-api's shared pagination.MaxLimit. Any larger requested
// limit is silently clamped server-side, so paging must use exactly this size.
const inventoryPageSize = 100

// fetchAllInventoryItemPages pages through an inventory-api items listing until every row is
// retrieved. inventory-api clamps ANY requested limit to its shared pagination cap (100), so a
// single large-limit request silently truncates the catalog — e.g. urban-loft's 269 sellable
// items came back as the first 100 only and most of the menu vanished from the terminal.
// baseURL must NOT carry limit/page params; they are appended per page here.
// inventoryProxyFields lists exactly the top-level JSON keys inventoryProxyItem decodes (kept in
// sync with that struct's tags) — passed as inventory-api's own opt-in `?fields=` response
// projection (see inventory-api's field_projection.go) so a catalog sweep transmits only what any
// pos-api caller of fetchInventoryItems (catalog sync, clinical_records, pharmacy_checkout,
// reports_profitability) actually decodes, instead of inventory-api's full ~97-field ItemDTO
// (tags/metadata, e-commerce/SEO fields, eTIMS fields, event fields, images, etc.). Pure wire-size
// reduction: inventory-api still computes/enriches the full row server-side either way.
const inventoryProxyFields = "id,sku,name,description,type,is_active,image_url,category_name," +
	"brand_id,brand_name,brand_code,manufacturer,model,dosage_form,strength,has_variants,variants," +
	"barcode,requires_age_verification,is_controlled_substance,track_serial_numbers,non_billable," +
	"not_for_sale,duration_minutes,use_case,total_capacity,booked_capacity,cost_price," +
	"suggested_price,min_selling_price,max_selling_price,on_hand,selling_price,food_cost_pct," +
	"status,purchase_price,purchase_pack_size,purchase_unit,yield_pct,tax_code_id,tax_inclusive," +
	"tax_rate,net_price,tax_amount,modifier_groups"

// inventoryPageFetchConcurrency bounds how many pages of a single catalog sweep run in flight at
// once. A tenant with a large central-warehouse catalog (e.g. an HQ outlet holding the union of
// every branch's stock) can need ~35-40 pages at inventoryPageSize=100; fetched serially that
// reliably pushed total handler time past a minute for boi-enterprises' HQ outlet (~3,800 items)
// and the request died (502) before ever producing a response — so the per-outlet Redis cache
// (cachedCatalogSource) never got a chance to populate either, making the outlet permanently
// unable to load its catalog. Fanning pages out concurrently cuts wall-clock roughly by this
// factor while staying gentle on inventory-api.
const inventoryPageFetchConcurrency = 8

func fetchAllInventoryItemPages(ctx context.Context, baseURL, outletID string) ([]inventoryProxyItem, error) {
	sep := "?"
	if strings.Contains(baseURL, "?") {
		sep = "&"
	}
	// lean=1 skips inventory-api's image-gallery/preferred-supplier eager loads; fields= trims the
	// response to what inventoryProxyItem actually decodes. Neither is read by any pos-api caller.
	baseURL = fmt.Sprintf("%s%slean=1&fields=%s", baseURL, sep, inventoryProxyFields)
	sep = "&"

	fetchPage := func(page int) ([]inventoryProxyItem, int, bool, error) {
		url := fmt.Sprintf("%s%slimit=%d&page=%d", baseURL, sep, inventoryPageSize, page)
		body, err := doInventoryGET(ctx, url, outletID)
		if err != nil {
			return nil, 0, false, err
		}
		var wrapper struct {
			Data    []inventoryProxyItem `json:"data"`
			Total   int                  `json:"total"`
			HasMore *bool                `json:"hasMore"`
		}
		if err := json.Unmarshal(body, &wrapper); err != nil {
			return nil, 0, false, err
		}
		hasMore := len(wrapper.Data) >= inventoryPageSize
		if wrapper.HasMore != nil {
			hasMore = *wrapper.HasMore
		}
		return wrapper.Data, wrapper.Total, hasMore, nil
	}

	// Page 1 alone tells us whether there's more to fetch and (usually) the authoritative total,
	// which lets every remaining page fire in parallel instead of one-at-a-time.
	first, total, hasMore, err := fetchPage(1)
	if err != nil {
		// Never silently serve a truncated catalog: a mid-loop failure is a failure.
		return nil, err
	}
	items := make([]inventoryProxyItem, len(first))
	copy(items, first)
	if !hasMore {
		return items, nil
	}

	// Hard page ceiling as a runaway guard (5k items); real catalogs stop on total/short-page.
	maxPages := 50
	if total > 0 {
		if needed := (total + inventoryPageSize - 1) / inventoryPageSize; needed < maxPages {
			maxPages = needed
		}
	}

	type pageResult struct {
		items []inventoryProxyItem
		err   error
	}
	results := make([]pageResult, maxPages+1) // 1-indexed by page number
	sem := make(chan struct{}, inventoryPageFetchConcurrency)
	var wg sync.WaitGroup
	for page := 2; page <= maxPages; page++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// A panic in this goroutine (e.g. a downstream library bug) would otherwise be
			// unrecoverable and crash the ENTIRE pos-api process — every tenant's in-flight
			// request, not just this one. Convert it into an ordinary page error instead.
			defer func() {
				if r := recover(); r != nil {
					results[p] = pageResult{err: fmt.Errorf("panic fetching page %d: %v", p, r)}
				}
			}()
			pageItems, _, pageHasMore, pageErr := fetchPage(p)
			results[p] = pageResult{items: pageItems, err: pageErr}
			// A short/terminal page reached before the total-derived ceiling is not an error —
			// nothing to do here, the range below already stops appending once items run out.
			_ = pageHasMore
		}(page)
	}
	wg.Wait()

	for page := 2; page <= maxPages; page++ {
		res := results[page]
		if res.err != nil {
			// Never silently serve a truncated catalog: a mid-loop failure is a failure.
			return nil, res.err
		}
		if len(res.items) == 0 {
			break
		}
		items = append(items, res.items...)
	}
	return items, nil
}

// fetchInventoryItems calls inventory-api and returns ALL active sellable items scoped by outlet
// and use-case set, paging past inventory-api's 100-row pagination cap. useCases is the outlet's
// effective set (primary use_case + any hybrid extras) — see effectiveCatalogUseCases.
func fetchInventoryItems(ctx context.Context, tenantSlug, outletID string, useCases []string) ([]inventoryProxyItem, error) {
	types := useCaseItemTypesForSet(useCases)
	// include=variants so the catalog item carries its sellable variations.
	// include_non_billable widens the type filter so free accompaniments / supplies
	// (tissue, packaging) reach the terminal regardless of item type.
	url := fmt.Sprintf("%s/v1/%s/inventory/items?type=%s&status=active&include=variants&include_non_billable=1", inventoryURL(), tenantSlug, types)
	return fetchAllInventoryItemPages(ctx, url, outletID)
}

// fetchInventoryItemByID fetches ONE inventory item by its inventory-api UUID (?id=, restricting
// ListItems to a single row while reusing its full enrichment — the same response shape as
// fetchAllInventoryItemPages). Use this instead of fetchInventoryItems whenever a caller already
// knows the specific item id(s) it needs (a handful of catalog_item_id references, e.g. a
// registration-fee item or a prescription line's linked drug) — a full paginated catalog walk to
// find one or a few known-id rows is the exact "resolveUnitCostsBySKU" mistake this fix removes
// elsewhere, just for a single-item lookup instead of a cost map. Returns nil, nil (not an error)
// when the item isn't found, so callers can treat a missing/deleted item as absent.
func fetchInventoryItemByID(ctx context.Context, tenantSlug, itemID string) (*inventoryProxyItem, error) {
	url := fmt.Sprintf("%s/v1/%s/inventory/items?id=%s&lean=1&fields=%s", inventoryURL(), tenantSlug, itemID, inventoryProxyFields)
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
	if len(wrapper.Data) == 0 {
		return nil, nil
	}
	return &wrapper.Data[0], nil
}

// fetchInventoryServiceItems lists inventory items filtered by use_case (e.g. HOSPITALITY_ROOM,
// HOSPITALITY_FACILITY, CONFERENCE, AMENITY) — used by hotel forms to pick the authoritative
// inventory master item to link to a Room/Facility/Amenity. Pages past the 100-row cap.
func fetchInventoryServiceItems(ctx context.Context, tenantSlug, useCase string) ([]inventoryProxyItem, error) {
	url := fmt.Sprintf("%s/v1/%s/inventory/items?use_case=%s&status=active", inventoryURL(), tenantSlug, useCase)
	return fetchAllInventoryItemPages(ctx, url, "")
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
// can pick an authoritative package by name instead of pasting a raw UUID. Pages past the
// shared 100-row pagination cap like the item fetches.
func fetchInventoryBundles(ctx context.Context, tenantSlug string) ([]inventoryProxyBundle, error) {
	fetchPage := func(page int) ([]inventoryProxyBundle, int, bool, error) {
		url := fmt.Sprintf("%s/v1/%s/inventory/bundles?limit=%d&page=%d", inventoryURL(), tenantSlug, inventoryPageSize, page)
		body, err := doInventoryGET(ctx, url, "")
		if err != nil {
			return nil, 0, false, err
		}
		var wrapper struct {
			Data    []inventoryProxyBundle `json:"data"`
			Total   int                    `json:"total"`
			HasMore *bool                  `json:"hasMore"`
		}
		if err := json.Unmarshal(body, &wrapper); err != nil {
			return nil, 0, false, err
		}
		hasMore := len(wrapper.Data) >= inventoryPageSize
		if wrapper.HasMore != nil {
			hasMore = *wrapper.HasMore
		}
		return wrapper.Data, wrapper.Total, hasMore, nil
	}

	// Same fix as fetchAllInventoryItemPages: page 1 tells us the total, so every remaining
	// page can fire concurrently instead of one-at-a-time (see the doc comment there for why
	// a serial walk of this shape reliably 502s a large catalog).
	first, total, hasMore, err := fetchPage(1)
	if err != nil {
		return nil, err
	}
	bundles := make([]inventoryProxyBundle, len(first))
	copy(bundles, first)
	if !hasMore {
		return bundles, nil
	}

	maxPages := 50
	if total > 0 {
		if needed := (total + inventoryPageSize - 1) / inventoryPageSize; needed < maxPages {
			maxPages = needed
		}
	}

	type pageResult struct {
		items []inventoryProxyBundle
		err   error
	}
	results := make([]pageResult, maxPages+1)
	sem := make(chan struct{}, inventoryPageFetchConcurrency)
	var wg sync.WaitGroup
	for page := 2; page <= maxPages; page++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					results[p] = pageResult{err: fmt.Errorf("panic fetching bundle page %d: %v", p, r)}
				}
			}()
			pageItems, _, _, pageErr := fetchPage(p)
			results[p] = pageResult{items: pageItems, err: pageErr}
		}(page)
	}
	wg.Wait()

	for page := 2; page <= maxPages; page++ {
		res := results[page]
		if res.err != nil {
			return nil, res.err
		}
		if len(res.items) == 0 {
			break
		}
		bundles = append(bundles, res.items...)
	}
	return bundles, nil
}

// fetchInventoryPricing calls inventory-api for prices. It returns the default-tier price per
// item (byID) AND the full per-tier price map per item (tiersByID[itemID][tierCode]) so the POS
// terminal can switch pricing profiles (Retail/Wholesale/…) without a per-item round-trip.
func fetchInventoryPricing(ctx context.Context, tenantSlug, outletID string) (map[string]float64, map[string]map[string]float64, error) {
	url := fmt.Sprintf("%s/v1/%s/inventory/items/pricing", inventoryURL(), tenantSlug)
	body, err := doInventoryGET(ctx, url, outletID)
	if err != nil {
		return nil, nil, err
	}
	var prices []inventoryBulkPrice
	if err := json.Unmarshal(body, &prices); err != nil {
		return nil, nil, err
	}
	m := make(map[string]float64, len(prices))
	tiers := make(map[string]map[string]float64, len(prices))
	for _, p := range prices {
		m[p.ItemID] = p.Price
		if len(p.Prices) > 0 {
			tiers[p.ItemID] = p.Prices
		}
	}
	return m, tiers, nil
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
	quantity := parseQuantityParam(r.URL.Query().Get("quantity"))
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

	url := fmt.Sprintf("%s/v1/%s/inventory/items/%s/price?quantity=%g", inventoryURL(), tenantSlug, itemID, quantity)
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
	ID           string
	SKU          string
	Name         string
	Description  string
	CategoryName string
	BrandName    string
	BrandCode    string
	Manufacturer string
	Model        string
	// Drug-master fields (pharmacy) — surfaced for the POS prescription drug picker.
	DosageForm  string
	Strength    string
	HasVariants bool
	Variants    []inventoryProxyVariant
	ItemType    string
	IsActive    bool
	IsAvailable bool
	// IsComplimentary marks a no-charge accompaniment: price 0 but explicitly enabled, shown as
	// "Free" and not charged, while its recipe/BOM stock is still deducted on sale.
	IsComplimentary bool
	IsFeatured      bool
	DisplayOrder    int
	ImageURL        string
	Barcode         string
	Price           float64
	// Prices carries the per-pricing-profile price keyed by tier code (e.g. {"RETAIL":320,
	// "WHOLESALE":290}) so the terminal can switch profiles without re-fetching. Default-profile
	// price stays in Price (override-merged).
	Prices                  map[string]float64
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
	// Bookable-service fields — surfaced so the terminal can treat co-working/conference/
	// facility SERVICE items as bookable (assign a space + capacity) rather than a plain
	// ring-up. UseCase mirrors the inventory item's use_case; Bookable is true when the
	// item is a bookable SERVICE (see isBookableServiceUseCase). Total/BookedCapacity carry
	// shared-seating availability. All nil/false for ordinary food/retail items.
	UseCase        string
	Bookable       bool
	TotalCapacity  *int
	BookedCapacity *int
	StockQuantity  *float64
	// Selling-price guardrails sourced from inventory — the terminal enforces the band and
	// pos-api hard-blocks out-of-band sale prices (manager override required).
	MinSellingPrice *float64
	MaxSellingPrice *float64
	// CostPrice is the supplier/purchase cost from inventory. It is SENSITIVE — only serialized to
	// callers with pos.catalog.view_cost (manager/admin) so the cart can show cost + margin; never
	// exposed on a cashier terminal or the public menu document.
	CostPrice *float64
	// NonBillable mirrors inventory Item.non_billable: rung up at a forced KES 0
	// (IsComplimentary is also set) — free accompaniments and supplies.
	NonBillable bool
	// InventoryPrice and POSOverridePrice are the RAW inputs to the Price merge above
	// (nil when that source had no value for this item), exposed so the sync-monitor
	// price-reconcile tab can show inventory vs. POS-DB-override vs. the merged result
	// side by side instead of only ever seeing the final Price.
	InventoryPrice   *float64
	POSOverridePrice *float64
	// ModifierGroups: this item's selectable modifiers, proxied from inventory-api.
	ModifierGroups []inventoryProxyModifierGroup
	// Unit is the item's stock unit abbreviation (e.g. "ml", "kg", "pc"), sourced from the
	// synced POSCatalogOverride metadata cache (see overrideEntry.uom). Lets the terminal
	// allow decimal quantity entry only for continuous units (ml/l/g/kg/m/cm), never for
	// discretely-counted items.
	Unit string
	// KDSStationID mirrors POSCatalogOverride.kds_station_id (Priority 1 in resolveStationForLine)
	// — the explicit "always route this item here" pin an admin sets via the KDS item-assignment
	// panel, e.g. an ice-cream scoop living in a mixed "Kids Corner" category but explicitly
	// pinned to Bar. Empty when the item has no such override, in which case routing falls
	// through to category_filter/hot-beverage matching. Surfaced here so client-side ticket
	// print routing (kitchen-bar-print.ts) can honor it exactly like server-side routing
	// already does — previously it had no way to see this at all.
	KDSStationID string
}

// catalogModifierOption is the terminal-facing wire shape for a single modifier option.
// Deliberately camelCase (unlike every other field in this file) because pos-ui's
// ModifierModal/terminal-context consume item.modifier_groups as-is with no remapping —
// this is the one shape that must match the frontend's ModifierOption interface exactly.
type catalogModifierOption struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Price     float64 `json:"price"`
	IsDefault bool    `json:"isDefault,omitempty"`
	// SKU/InventoryModifierOptionID travel with the option so the order-line-create payload
	// can carry them straight through to POSLineModifier without a second lookup.
	SKU                       string `json:"sku,omitempty"`
	InventoryModifierOptionID string `json:"inventory_modifier_option_id,omitempty"`
}

// catalogModifierGroup is the terminal-facing wire shape for a modifier group — see
// catalogModifierOption for why this breaks from the file's usual snake_case convention.
type catalogModifierGroup struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	IsRequired    bool                    `json:"isRequired"`
	MinSelections int                     `json:"minSelections"`
	MaxSelections int                     `json:"maxSelections"`
	Options       []catalogModifierOption `json:"options"`
}

// toCatalogModifierGroups converts the inventory-proxy shape into the terminal wire shape.
func toCatalogModifierGroups(groups []inventoryProxyModifierGroup) []catalogModifierGroup {
	if len(groups) == 0 {
		return nil
	}
	out := make([]catalogModifierGroup, 0, len(groups))
	for _, g := range groups {
		opts := make([]catalogModifierOption, 0, len(g.Options))
		for _, o := range g.Options {
			opts = append(opts, catalogModifierOption{
				ID:                        o.ID,
				Name:                      o.Name,
				Price:                     o.PriceAdjustment,
				IsDefault:                 o.IsDefault,
				SKU:                       o.SKU,
				InventoryModifierOptionID: o.ID,
			})
		}
		out = append(out, catalogModifierGroup{
			ID:            g.ID,
			Name:          g.Name,
			IsRequired:    g.IsRequired,
			MinSelections: g.MinSelections,
			MaxSelections: g.MaxSelections,
			Options:       opts,
		})
	}
	return out
}

// menuAssemblyFilters carries the optional list-time filters applied by ListCatalogItems.
// The menu document renderer leaves these empty (it wants the full active menu).
type menuAssemblyFilters struct {
	Category string // case-insensitive exact match on category name, EXCEPT pharmacy aliases (see pharmacyCategoryAliases), which match via isPharmacyCategory substring
	Search   string // case-insensitive substring match on item name (already lower-cased)
	ItemType string // comma-separated, case-insensitive match on item type (already upper-cased)
}

// catalogSourceSet is the cacheable upstream slice of the catalog: the full inventory item
// sweep + resolved pricing maps. Local POSCatalogOverride data is deliberately NOT part of
// it — overrides are cheap local rows and must apply live (availability toggles, price
// edits) even while the upstream sweep is cached.
type catalogSourceSet struct {
	Items  []inventoryProxyItem          `json:"items"`
	Prices map[string]float64            `json:"prices"`
	Tiers  map[string]map[string]float64 `json:"tiers"`
}

// catalogSourceTTL — see the cache rationale in assembleMenuItems. Roughly one terminal
// version-poll interval: fresh enough for inventory edits, long enough that a paged
// full-catalog load (8–20 requests in a few seconds) hits upstream exactly once.
const catalogSourceTTL = 60 * time.Second

// cachedCatalogSource returns the (items + pricing) upstream fetch through a per-
// (tenant, outlet, use-case-set) Redis cache. Fail-open on every cache path: no Redis or
// a decode error just means a live upstream fetch. A degraded fetch (pricing failed) is
// served but NOT cached, so the next request retries pricing instead of pinning zeros.
func (h *CatalogHandler) cachedCatalogSource(ctx context.Context, tid uuid.UUID, tenantSlug, outletIDStr string, useCases []string) (*catalogSourceSet, error) {
	key := fmt.Sprintf("pos:catalogsrc:%s:%s:%s", tid, outletIDStr, strings.Join(useCases, ","))
	if h.redis != nil {
		if raw, err := h.redis.Get(ctx, key).Bytes(); err == nil {
			var cached catalogSourceSet
			if json.Unmarshal(raw, &cached) == nil {
				return &cached, nil
			}
		}
	}

	// Concurrent requests that all miss the cache for the SAME key (several terminals at a
	// large-catalog outlet loading within the same ~60s window, or the cache simply being cold
	// after a deploy) would otherwise each independently re-run the full inventory-api sweep — a
	// "cache stampede" that multiplies S2S load exactly when the system is already under the most
	// pressure. This is not hypothetical: it reproduced live during this fix's own verification
	// (multiple concurrent test requests against boi-enterprises' HQ outlet turned isolated
	// 1-2s page fetches into repeated 502s). singleflight coalesces same-key misses onto one
	// in-flight sweep per pod; every waiting caller gets the same result.
	v, err, _ := h.sweepGroup.Do(key, func() (any, error) {
		// Deliberately NOT the triggering caller's ctx: singleflight shares this single
		// in-flight call across every concurrent caller, so if it were tied to whichever
		// caller happened to trigger it, that one caller disconnecting (a client timeout, a
		// closed browser tab) would cancel the fetch for every OTHER waiter too. A bounded,
		// independent context lets the sweep run to completion (or its own timeout) regardless
		// of any individual caller's fate.
		sweepCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()

		// Fetch items + pricing from inventory-api in parallel.
		type itemsResult struct {
			items []inventoryProxyItem
			err   error
		}
		type priceResult struct {
			prices map[string]float64
			tiers  map[string]map[string]float64
			err    error
		}
		itemsCh := make(chan itemsResult, 1)
		priceCh := make(chan priceResult, 1)
		go func() {
			items, err := fetchInventoryItems(sweepCtx, tenantSlug, outletIDStr, useCases)
			itemsCh <- itemsResult{items, err}
		}()
		go func() {
			prices, tiers, err := fetchInventoryPricing(sweepCtx, tenantSlug, outletIDStr)
			priceCh <- priceResult{prices, tiers, err}
		}()
		ir := <-itemsCh
		pr := <-priceCh
		if ir.err != nil {
			return nil, ir.err
		}
		if pr.err != nil {
			h.log.Warn("inventory pricing fetch failed — prices will be 0", zap.Error(pr.err))
		}

		src := &catalogSourceSet{Items: ir.items, Prices: pr.prices, Tiers: pr.tiers}
		if h.redis != nil && pr.err == nil {
			if raw, err := json.Marshal(src); err == nil {
				_ = h.redis.Set(sweepCtx, key, raw, catalogSourceTTL).Err()
			}
		}
		return src, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*catalogSourceSet), nil
}

// overrideEntry is the POS-specific per-SKU override merged onto an inventory-api item.
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
	// uom = the item's stock unit abbreviation (e.g. "ml", "kg"), cached at catalog-sync time
	// by the inventory.item.* consumer (inventory_events.go) purely so pos.sale.finalized can
	// carry a real uom_code. Reused here to surface it to the terminal too, so the UI can allow
	// decimal-qty entry only for continuous units.
	uom string
	// kdsStationID is the explicit per-item KDS routing pin (POSCatalogOverride.kds_station_id),
	// nil when the item has no override. See catalogItemDTO.KDSStationID.
	kdsStationID *uuid.UUID
	// outletSpecific marks a row scoped to a specific outlet (vs. a tenant-wide default) — once
	// one has won the map slot for a SKU, a tenant-wide row must never replace it.
	outletSpecific bool
}

// resolveCatalogOverrides loads this tenant/outlet's POSCatalogOverride rows and merges them
// into a SKU → best-match map. Filters at the QUERY level to what this outlet may legitimately
// see (its own outlet-specific rows plus tenant-wide defaults) — an override belonging to a
// DIFFERENT, unrelated outlet must never even be fetched, let alone leak into the map by
// accident of row order (the bug this replaced: the old code fetched every override for the
// whole tenant and relied on an `!exists` merge check that let whichever outlet's row Query().
// All() happened to return FIRST win a SKU, regardless of which outlet actually asked).
func (h *CatalogHandler) resolveCatalogOverrides(ctx context.Context, tid uuid.UUID, outletID *uuid.UUID) (map[string]overrideEntry, error) {
	preds := []predicate.POSCatalogOverride{entoverride.TenantID(tid)}
	if outletID != nil {
		preds = append(preds, entoverride.Or(
			entoverride.OutletIDIsNil(),
			entoverride.OutletID(*outletID),
		))
	} else {
		preds = append(preds, entoverride.OutletIDIsNil())
	}
	overrides, err := h.client.POSCatalogOverride.Query().Where(preds...).All(ctx)
	if err != nil {
		return nil, err
	}
	return mergeCatalogOverrides(overrides), nil
}

// mergeCatalogOverrides is the pure merge step (no DB) so the precedence rule — an
// outlet-specific row always beats a tenant-wide default for the same SKU — is unit-testable
// without a database.
func mergeCatalogOverrides(overrides []*ent.POSCatalogOverride) map[string]overrideEntry {
	overrideMap := make(map[string]overrideEntry, len(overrides))
	for _, o := range overrides {
		key := o.InventorySku
		isOutletSpecific := o.OutletID != nil
		if prev, exists := overrideMap[key]; exists && prev.outletSpecific && !isOutletSpecific {
			continue
		}
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
			uom:                     metaString(o.Metadata, "uom"),
			kdsStationID:            o.KdsStationID,
			outletSpecific:          isOutletSpecific,
		}
	}
	return overrideMap
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

	// Hybrid catalog support: union the outlet's primary use_case with any extras enabled
	// on OutletSetting.catalog_use_cases (e.g. a hospitality cafe also selling co-working/
	// conference SERVICE packages via "services"). Both the inventory-api type fetch and
	// the category gate below use this set instead of the single primary use_case.
	useCases := effectiveCatalogUseCases(useCase, h.resolveExtraCatalogUseCases(ctx, outletID))

	// Fetch items + pricing from inventory-api — through a short-TTL Redis cache.
	//
	// WHY THE CACHE IS LOAD-BEARING: this sweep pages the tenant's ENTIRE inventory at
	// inventory-api's 100-row cap (a 3,800-item retail tenant = ~39 serial upstream calls,
	// plus pricing pages). The terminal's cold full-catalog load requests ~8–20 pages of
	// THIS endpoint, and without the cache EVERY page repeated the whole upstream sweep —
	// hundreds of serial S2S calls that left big-catalog terminals stuck on "Loading
	// menu…" (observed live at boi-enterprises). With it, one sweep serves the whole
	// paged load; local overrides below stay UNCACHED so availability/price edits still
	// apply on the next request. TTL 60s ≈ the terminal's 45s version poll, so inventory
	// edits surface on the same cadence they always did.
	src, err := h.cachedCatalogSource(ctx, tid, tenantSlug, outletIDStr, useCases)
	if err != nil {
		return nil, err
	}
	invPriceByID := src.Prices
	invTierPricesByID := src.Tiers // itemID → {tierCode: price}

	overrideMap, err := h.resolveCatalogOverrides(ctx, tid, outletID)
	if err != nil {
		return nil, err
	}

	out := make([]catalogItemDTO, 0, len(src.Items))
	for _, item := range src.Items {
		// NOT-FOR-SALE (inventory Item.not_for_sale): inventory-only stock (ingredients,
		// internal supplies) must never surface on ANY POS sales surface — dropped before any
		// filter/override so no local override can resurrect it. Distinct from non_billable,
		// which stays on the menu as a free (KES 0) item.
		if item.NotForSale {
			continue
		}
		// Apply filters
		if filters.Category != "" {
			if pharmacyCategoryAliases[strings.ToLower(strings.TrimSpace(filters.Category))] {
				if !isPharmacyCategory(item.CategoryName) {
					continue
				}
			} else if !strings.EqualFold(item.CategoryName, filters.Category) {
				continue
			}
		}
		// Match name, SKU or barcode (barcode/SKU are exact identifiers a scanner enters) — the
		// filter value is already lower-cased by the caller. Matching name only meant a barcode
		// search returned nothing.
		if filters.Search != "" &&
			!strings.Contains(strings.ToLower(item.Name), filters.Search) &&
			!strings.Contains(strings.ToLower(item.SKU), filters.Search) &&
			!strings.Contains(strings.ToLower(item.Barcode), filters.Search) {
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

		// Exclude items whose category doesn't belong to any of this outlet's use cases
		// (primary + hybrid extras). A hospitality cafe with "services" enabled keeps its
		// food menu AND its co-working/conference SERVICE packages.
		if len(useCases) > 0 && !categoryAllowedForUseCaseSet(item.CategoryName, useCases) {
			continue
		}

		o, hasOverride := overrideMap[item.SKU]

		kdsStationIDString := ""
		if hasOverride && o.kdsStationID != nil {
			kdsStationIDString = o.kdsStationID.String()
		}

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

		var inventoryPrice *float64
		var posOverridePrice *float64
		// RECIPE items are priced from their recipe, not from a pricing-tier row — see
		// inventory-api's own resolution order (modules/items/pricing_enrich.go
		// effectivePrice: recipe price wins over tier price), which is also what
		// item.SellingPrice already carries and what the inventory catalog UI displays.
		// invPriceByID comes from the RAW ItemPricing bulk-tier endpoint, which has no
		// concept of recipes; for a RECIPE item it can hold a stale/unrelated row (e.g.
		// left over from before the item had a recipe) that never gets updated when the
		// recipe's price changes. Checking it first — as a flat invPrice-wins-always merge
		// — silently overrides the authoritative recipe price and permanently disagrees
		// with inventory-ui, which is exactly the drift the sync-monitor price-reconcile
		// tab is meant to catch. So for RECIPE items prefer item.SellingPrice; fall back
		// to the tier price only when the recipe has none.
		if strings.EqualFold(item.Type, "RECIPE") && item.SellingPrice != nil && *item.SellingPrice > 0 {
			price = *item.SellingPrice
			sp := *item.SellingPrice
			inventoryPrice = &sp
		} else if invPrice, ok := invPriceByID[item.ID]; ok {
			price = invPrice
			ip := invPrice
			inventoryPrice = &ip
		}

		if hasOverride {
			posOverridePrice = o.sellingPrice
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

		// NON-BILLABLE (inventory Item.non_billable): never charged at the till, even when a
		// selling price exists — free accompaniments (ugali) and consumable supplies (tissue,
		// packaging). Price is FORCED to 0, the item stays available and is flagged Free, and
		// the min/max selling-price guardrails are cleared so the zero price can't trip the
		// out-of-band block. Stock still deducts on sale.
		nonBillable := item.NonBillable != nil && *item.NonBillable
		if nonBillable {
			price = 0
			isComplimentary = true
			item.MinSellingPrice = nil
			item.MaxSellingPrice = nil
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
			// A price-0 item is treated as a no-charge accompaniment (shown, forced KES 0, flagged Free,
			// BOM still deducted) when EITHER an admin explicitly enabled it via the override
			// (metadata.complimentary=true) OR it lives in an "Accompaniment"/side category — those are
			// free sides by definition (e.g. ugali), so they surface without per-item flagging. Any other
			// price-0 item stays hidden so staff can't accidentally ring up a KES 0 sale.
			if nonBillable || (hasOverride && o.complimentary) || isAccompanimentCategory(item.CategoryName) {
				isComplimentary = true
				isAvailable = true
			} else {
				isAvailable = false
			}
		}

		// Per-profile prices (rounded to whole units like the default price). The selected default
		// tier code is mapped onto the override-merged `price` so the active profile always matches
		// what's displayed; other tiers come straight from inventory.
		var tierPrices map[string]float64
		if raw := invTierPricesByID[item.ID]; len(raw) > 0 {
			tierPrices = make(map[string]float64, len(raw))
			for code, p := range raw {
				if p > 0 {
					tierPrices[code] = math.Ceil(p)
				} else {
					tierPrices[code] = 0
				}
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
			DosageForm:              item.DosageForm,
			Strength:                item.Strength,
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
			UseCase:                 item.UseCase,
			Bookable:                item.Type == "SERVICE" && isBookableServiceUseCase(item.UseCase),
			TotalCapacity:           item.TotalCapacity,
			BookedCapacity:          item.BookedCapacity,
			StockQuantity:           item.OnHand,
			MinSellingPrice:         item.MinSellingPrice,
			MaxSellingPrice:         item.MaxSellingPrice,
			// Supplier cost (for the manager-only cart cost/margin columns). Prefer the explicit
			// cost_price; fall back to purchase_price when cost isn't set — treating a stored 0
			// as "unset" too, so an item saved with cost 0 but a real purchase price still shows
			// cost/margin instead of "—".
			CostPrice:        firstPositiveFloat(item.CostPrice, item.PurchasePrice),
			NonBillable:      nonBillable,
			InventoryPrice:   inventoryPrice,
			POSOverridePrice: posOverridePrice,
			ModifierGroups:   item.ModifierGroups,
			Unit:             o.uom,
			KDSStationID:     kdsStationIDString,
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

// metaString reads a string value from a POSCatalogOverride.metadata map, returning "" when
// absent or not a string (e.g. "uom", cached at catalog-sync time — see overrideEntry.uom).
func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// readFloatMeta reads a numeric value from a metadata map, tolerating float64,
// int, json.Number, and numeric-string encodings.
func readFloatMeta(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	default:
		return 0
	}
}

// isAccompanimentCategory reports whether a catalog category is an "accompaniment"/side category.
// Items in such a category that carry no price are by definition no-charge sides (ugali, greens, a
// free side) — they are surfaced as COMPLIMENTARY (Free) automatically, without needing a per-item
// override flag, so outlets don't have to flag every free side by hand. BOM/recipe stock is still
// deducted on sale regardless of price (the inventory pos.sale.finalized consumer deducts by SKU).
func isAccompanimentCategory(cat string) bool {
	c := strings.ToLower(strings.TrimSpace(cat))
	return strings.Contains(c, "accompaniment") || strings.Contains(c, "side dish") || strings.Contains(c, "sides")
}

// firstNonNilFloat returns the first non-nil *float64 (used to pick cost_price, else purchase_price).
func firstNonNilFloat(vals ...*float64) *float64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// firstPositiveFloat returns the first pointer holding a value > 0. Used for the cost fallback
// ladder, where a stored 0 means "never captured" and must not shadow a real purchase price.
func firstPositiveFloat(vals ...*float64) *float64 {
	for _, v := range vals {
		if v != nil && *v > 0 {
			return v
		}
	}
	return nil
}

// catalogItemToMap converts an assembled DTO into the JSON map shape ListCatalogItems returns.
// showCost controls whether the SENSITIVE cost_price is emitted (only for pos.catalog.view_cost
// callers) — otherwise the key is omitted entirely so cost never reaches an unauthorized browser.
func catalogItemToMap(item catalogItemDTO, outletID *uuid.UUID, showCost bool) map[string]any {
	m := catalogItemToMapBase(item, outletID)
	if showCost && item.CostPrice != nil {
		m["cost_price"] = *item.CostPrice
	}
	return m
}

func catalogItemToMapBase(item catalogItemDTO, outletID *uuid.UUID) map[string]any {
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
		"dosage_form":               item.DosageForm,
		"strength":                  item.Strength,
		"has_variants":              item.HasVariants,
		"variants":                  item.Variants,
		"item_type":                 item.ItemType,
		"status":                    map[bool]string{true: "active", false: "inactive"}[item.IsActive],
		"is_available":              item.IsAvailable,
		"is_complimentary":          item.IsComplimentary,
		"non_billable":              item.NonBillable,
		"is_featured":               item.IsFeatured,
		"display_order":             item.DisplayOrder,
		"image_url":                 item.ImageURL,
		"barcode":                   item.Barcode,
		"price":                     item.Price,
		"prices":                    item.Prices,
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
		// Bookable-service metadata — the terminal keys its "book / assign space" flow and
		// capacity badge off these (nil/false/0 for ordinary products).
		"use_case":          item.UseCase,
		"bookable":          item.Bookable,
		"total_capacity":    item.TotalCapacity,
		"booked_capacity":   item.BookedCapacity,
		"stock_quantity":    item.StockQuantity,
		"min_selling_price": item.MinSellingPrice,
		"max_selling_price": item.MaxSellingPrice,
		// Stock unit abbreviation (e.g. "ml", "kg") — lets the terminal allow decimal
		// quantity entry only for continuous units. Empty for items with no synced unit.
		"unit": item.Unit,
		// Explicit per-item KDS routing pin (POSCatalogOverride.kds_station_id), empty when
		// unset. Lets client-side ticket print routing honor the same override server-side
		// order-line creation already does — see catalogItemDTO.KDSStationID.
		"kds_station_id": item.KDSStationID,
		// Raw inputs to the price merge above — powers the sync-monitor price-reconcile tab's
		// inventory-vs-POS-DB-override-vs-merged compare. Nil when that source had no value.
		"inventory_price":    item.InventoryPrice,
		"pos_override_price": item.POSOverridePrice,
		"outlet_id":          outletID,
		"modifier_groups":    toCatalogModifierGroups(item.ModifierGroups),
	}
}

// GetCatalogVersion handles GET /{tenantID}/pos/catalog/version — a very cheap freshness
// fingerprint the terminal polls to know when its locally-cached catalog is stale.
//
// The fingerprint is derived from the tenant's POSCatalogOverride projection (row count +
// max updated_at). The inventory.item.created/updated NATS consumer upserts an override row
// for every inventory change, so any new/edited inventory item bumps this version within
// seconds — the terminal then background-refreshes its full catalog (and IndexedDB cache)
// without the cashier refreshing or re-logging in. Two aggregates over an indexed tenant_id
// column: safe to poll frequently (also rate-limit-exempt like /payment-status).
func (h *CatalogHandler) GetCatalogVersion(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var agg []struct {
		Count int       `json:"count"`
		Max   time.Time `json:"max"`
	}
	err = h.client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid)).
		Aggregate(ent.Count(), ent.Max(entoverride.FieldUpdatedAt)).
		Scan(r.Context(), &agg)
	// Stock version: a per-tenant marker bumped by the inventory.stock.updated consumer, folded in
	// so a pure restock/adjustment/sale (which does NOT touch POSCatalogOverride) still changes the
	// version and makes terminals refresh their on_hand. "0" when Redis is unavailable or unset.
	stockVer := "0"
	if h.redis != nil {
		// Key mirrors catalog.CatalogStockVersionKey (inlined to avoid a handlers->module import).
		if v, gerr := h.redis.Get(r.Context(), "pos:catver:"+tid.String()).Result(); gerr == nil && v != "" {
			stockVer = v
		}
	}
	if err != nil || len(agg) == 0 {
		// Degrade to a constant catalog part — pollers still see the stock version change.
		jsonOK(w, map[string]any{"version": "0-0-" + stockVer, "count": 0})
		return
	}
	jsonOK(w, map[string]any{
		"version": fmt.Sprintf("%d-%d-%s", agg[0].Count, agg[0].Max.UTC().UnixMilli(), stockVer),
		"count":   agg[0].Count,
	})
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

	// Cost price is a manager-only field — expose it only to callers with pos.catalog.view_cost, or
	// the manager/admin permission pos.orders.manage (which is always seeded to those roles, so cost
	// works for managers today even before a dedicated view_cost grant exists). A cashier terminal
	// never receives supplier costs, even if the client tried to render them.
	showCost := middleware.HasServicePermission(r, h.rbac, "pos.catalog.view_cost", "pos.orders.manage")
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, catalogItemToMap(item, outletID, showCost))
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

	// not_for_sale items must never surface on a POS sales surface — treat a direct item
	// fetch like a miss, mirroring the catalog list (assembleMenuItems) and barcode lookup.
	// Handles both the bare-object and {"data": {...}} response shapes.
	if nfs, _ := item["not_for_sale"].(bool); nfs {
		jsonError(w, "item not found", http.StatusNotFound)
		return
	}
	if data, ok := item["data"].(map[string]any); ok {
		if nfs, _ := data["not_for_sale"].(bool); nfs {
			jsonError(w, "item not found", http.StatusNotFound)
			return
		}
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

	// Scope the existing-row lookup by outlet too — a tenant can have both a tenant-wide (outlet_id
	// NULL) override and per-outlet overrides for the same SKU; matching on (tenant, sku) alone let
	// this handler find-and-mutate the WRONG row for a multi-outlet tenant (e.g. overwrite the
	// tenant-wide row's price when the caller asked for one specific outlet).
	lookup := h.client.POSCatalogOverride.Query().Where(entoverride.TenantID(tid), entoverride.InventorySku(input.SKU))
	if outletID != nil {
		lookup = lookup.Where(entoverride.OutletID(*outletID))
	} else {
		lookup = lookup.Where(entoverride.OutletIDIsNil())
	}
	existing, _ := lookup.First(r.Context())

	if existing != nil {
		upd := existing.Update().SetSellingPrice(input.SellingPrice)
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

// SetCatalogItemKDSStation handles PATCH /{tenantID}/pos/catalog/items/kds-station
// Upserts an item's explicit KDS station assignment (POSCatalogOverride.kds_station_id) — the
// PRIMARY routing mechanism (resolveStationForLine priority 1 in modules/orders/service.go),
// which wins over both the hot-beverage guard and category_filter matching. This is the "managers
// assign items to stations in POS settings" escape hatch the routing code has always assumed
// exists; until this handler, nothing actually let a manager set it. StationID nil/empty clears
// the override, reverting the item to category_filter/hot-beverage routing.
func (h *CatalogHandler) SetCatalogItemKDSStation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		SKU       string `json:"sku"`
		StationID string `json:"station_id,omitempty"` // empty/omitted clears the override
		OutletID  string `json:"outlet_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SKU == "" {
		jsonError(w, "sku is required", http.StatusBadRequest)
		return
	}

	var outletID *uuid.UUID
	if input.OutletID != "" {
		if oid, parseErr := uuid.Parse(input.OutletID); parseErr == nil {
			outletID = &oid
		}
	}

	var stationID *uuid.UUID
	if input.StationID != "" {
		sid, parseErr := uuid.Parse(input.StationID)
		if parseErr != nil {
			jsonError(w, "invalid station_id", http.StatusBadRequest)
			return
		}
		// Validate the station belongs to this tenant (and outlet, when given) before linking it —
		// an override pointing at a nonexistent/foreign station would silently never match in
		// resolveStationForLine's station list lookup (scoped by tenant+outlet), routing the item
		// nowhere instead of failing loudly here.
		stationQ := h.client.KDSStation.Query().Where(entkdsstation.TenantID(tid), entkdsstation.ID(sid))
		if outletID != nil {
			stationQ = stationQ.Where(entkdsstation.OutletID(*outletID))
		}
		exists, existErr := stationQ.Exist(r.Context())
		if existErr != nil {
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !exists {
			jsonError(w, "station not found for this tenant/outlet", http.StatusNotFound)
			return
		}
		stationID = &sid
	}

	// Scope the existing-row lookup by outlet too, mirroring SetCatalogItemPrice — a tenant can
	// have both a tenant-wide (outlet_id NULL) override and per-outlet overrides for the same SKU.
	lookup := h.client.POSCatalogOverride.Query().Where(entoverride.TenantID(tid), entoverride.InventorySku(input.SKU))
	if outletID != nil {
		lookup = lookup.Where(entoverride.OutletID(*outletID))
	} else {
		lookup = lookup.Where(entoverride.OutletIDIsNil())
	}
	existing, _ := lookup.First(r.Context())

	if existing != nil {
		upd := existing.Update()
		if stationID != nil {
			upd = upd.SetKdsStationID(*stationID)
		} else {
			upd = upd.ClearKdsStationID()
		}
		updated, saveErr := upd.Save(r.Context())
		if saveErr != nil {
			jsonError(w, "update failed: "+saveErr.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, updated)
		return
	}

	if stationID == nil {
		// Clearing a station override that was never set — nothing to do, don't create an
		// otherwise-empty override row just to hold a clear.
		jsonOK(w, map[string]any{"sku": input.SKU, "kds_station_id": nil})
		return
	}

	creator := h.client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku(input.SKU).
		SetKdsStationID(*stationID)
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
	// Scoped by outlet the same way SetCatalogItemPrice is (see its comment) — matching on
	// (tenant, sku) alone can find the wrong one of a tenant-wide vs. per-outlet override pair.
	outletLookup := h.client.POSCatalogOverride.Query().Where(entoverride.TenantID(tid), entoverride.InventorySkuIn(skus...))
	if outletID != nil {
		outletLookup = outletLookup.Where(entoverride.OutletID(*outletID))
	} else {
		outletLookup = outletLookup.Where(entoverride.OutletIDIsNil())
	}
	existing, _ := outletLookup.All(r.Context())
	existingBySKU := make(map[string]*ent.POSCatalogOverride, len(existing))
	for _, e := range existing {
		existingBySKU[e.InventorySku] = e
	}

	updated := 0
	for _, p := range input.Prices {
		if e, ok := existingBySKU[p.SKU]; ok {
			if _, saveErr := e.Update().SetSellingPrice(p.SellingPrice).Save(r.Context()); saveErr == nil {
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
	return resolveTenantSlug(r, h.client)
}

// resolveTenantSlug derives the tenant slug from auth claims, httpware context, or a tenant
// lookup. Package-level (not tied to CatalogHandler) so any handler in this package that needs
// to proxy inventory-api — which is keyed by slug, not tenant UUID — can resolve it the same way.
func resolveTenantSlug(r *http.Request, client *ent.Client) string {
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
			if t, lookupErr := client.Tenant.Get(r.Context(), tid); lookupErr == nil {
				tenantSlug = t.Slug
			}
		}
	}
	return tenantSlug
}

// resolveOutletCurrency returns the outlet's configured ISO 4217 currency (OutletSetting.currency
// — the same Settings > General field the till reads via usePOSSettings), falling back to "KES"
// only when the outlet has none configured. Package-level so every handler that writes a
// money-movement record NOT going through orders.Service.CreateOrder (refunds, exchanges,
// reversals, layaway, treasury proxy calls) can resolve the tenant's real currency the same way
// orders.Service.outletCurrency does for order creation, instead of each hardcoding "KES".
func resolveOutletCurrency(ctx context.Context, client *ent.Client, outletID uuid.UUID) string {
	if outletID == uuid.Nil {
		return "KES"
	}
	if set, err := client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Only(ctx); err == nil && set != nil && set.Currency != "" {
		return set.Currency
	}
	return "KES"
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
