package orders

import (
	"context"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
)

// CatalogCostBySKU resolves the authoritative sale-time COGS source — the inventory-synced
// POSCatalogOverride.metadata["cost_price"] keyed by (tenant, inventory_sku) — for a set of
// SKUs. This is the SAME source the sale.finalized COGS posting and the returns refund use,
// so reversals stay symmetric with what was originally posted. Missing/unpriced SKUs are
// simply absent from the map; errors return an empty map (cost lookups never block money flows).
func CatalogCostBySKU(ctx context.Context, client *ent.Client, tenantID uuid.UUID, skus []string) map[string]float64 {
	costs := map[string]float64{}
	if client == nil || len(skus) == 0 {
		return costs
	}
	overrides, err := client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tenantID), entoverride.InventorySkuIn(skus...)).
		All(ctx)
	if err != nil {
		return costs
	}
	for _, ov := range overrides {
		if ov.Metadata == nil {
			continue
		}
		switch v := ov.Metadata["cost_price"].(type) {
		case float64:
			costs[ov.InventorySku] = v
		case int:
			costs[ov.InventorySku] = float64(v)
		}
	}
	return costs
}
