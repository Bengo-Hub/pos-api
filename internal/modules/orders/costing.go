package orders

import (
	"context"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
)

// CatalogCacheEntry is the subset of the inventory-synced POSCatalogOverride cache
// (metadata["cost_price"/"manufacturer"/"category_name"]) that reports/COGS lookups need.
type CatalogCacheEntry struct {
	CostPrice    float64
	Manufacturer string
	CategoryName string
}

// CatalogCacheBySKU resolves the inventory-synced POSCatalogOverride cache — cost_price,
// manufacturer, category_name — for a set of SKUs in ONE query, keyed by (tenant, inventory_sku).
// cost_price is the authoritative sale-time COGS source (the SAME source the sale.finalized COGS
// posting and returns/reversals use, via CatalogCostBySKU below, so reversals stay symmetric with
// what was originally posted); manufacturer/category_name back the MostProfitableItems
// ?group_by=manufacturer|category rollup. The cache is kept fresh by the
// inventory.item.created/updated/cost_changed event consumer (catalog.InventoryEventHandler) —
// zero S2S calls here. Missing/unset fields are simply zero-valued; errors return an empty map
// (cost/metadata lookups never block money flows or reports).
func CatalogCacheBySKU(ctx context.Context, client *ent.Client, tenantID uuid.UUID, skus []string) map[string]CatalogCacheEntry {
	out := map[string]CatalogCacheEntry{}
	if client == nil || len(skus) == 0 {
		return out
	}
	overrides, err := client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tenantID), entoverride.InventorySkuIn(skus...)).
		All(ctx)
	if err != nil {
		return out
	}
	for _, ov := range overrides {
		var e CatalogCacheEntry
		if ov.Metadata != nil {
			switch v := ov.Metadata["cost_price"].(type) {
			case float64:
				e.CostPrice = v
			case int:
				e.CostPrice = float64(v)
			}
			if v, ok := ov.Metadata["manufacturer"].(string); ok {
				e.Manufacturer = v
			}
			if v, ok := ov.Metadata["category_name"].(string); ok {
				e.CategoryName = v
			}
		}
		out[ov.InventorySku] = e
	}
	return out
}

// CatalogCostBySKU is the cost-only projection of CatalogCacheBySKU — kept as its own function
// (rather than inlining CatalogCacheBySKU at every call site) for the existing, production-proven
// callers that only ever needed cost: the sale.finalized COGS posting and returns/reversals.
func CatalogCostBySKU(ctx context.Context, client *ent.Client, tenantID uuid.UUID, skus []string) map[string]float64 {
	costs := map[string]float64{}
	for sku, e := range CatalogCacheBySKU(ctx, client, tenantID, skus) {
		costs[sku] = e.CostPrice
	}
	return costs
}
