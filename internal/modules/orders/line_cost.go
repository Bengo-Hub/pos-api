package orders

import (
	"context"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// Order line metadata keys for the cost a sale actually carried. payments stamps the till's
// standard cost at finalize; catalog's inventory.consumption.costed handler later replaces it with
// what inventory really charged and marks the source.
const (
	LineUnitCostAtSaleKey = "unit_cost_at_sale"
	LineCostSourceKey     = "cost_source"
	// LineCostSourceInventory marks a unit cost that came from inventory's own consumption costing.
	LineCostSourceInventory = "inventory"
)

// LineUnitCostAtSale returns the per-unit cost this line was sold at, or nil when the line carries
// none (sold before the snapshot existed, or cost was unknown at sale time). A zero is only trusted
// when inventory reported it, because the till never stamps a zero and a bare zero from anywhere
// else would hide a real cost from the catalog fallback.
func LineUnitCostAtSale(l *ent.POSOrderLine) *float64 {
	if l == nil || l.Metadata == nil {
		return nil
	}
	var c float64
	switch v := l.Metadata[LineUnitCostAtSaleKey].(type) {
	case float64:
		c = v
	case int:
		c = float64(v)
	default:
		return nil
	}
	if c < 0 {
		return nil
	}
	if c == 0 && l.Metadata[LineCostSourceKey] != LineCostSourceInventory {
		return nil
	}
	return &c
}

// LineUnitCosts resolves the per-unit cost of each line, keyed by line ID. A line's own sale-time
// cost wins so a reversal or return backs out exactly the COGS the sale booked, even after the
// item's catalog cost has moved. Only lines without one fall back to today's catalog cache, in a
// single query for just those SKUs.
func LineUnitCosts(ctx context.Context, client *ent.Client, tenantID uuid.UUID, lines []*ent.POSOrderLine) map[uuid.UUID]float64 {
	var missing []string
	seen := map[string]bool{}
	for _, l := range lines {
		if l == nil || l.Sku == "" || LineUnitCostAtSale(l) != nil || seen[l.Sku] {
			continue
		}
		seen[l.Sku] = true
		missing = append(missing, l.Sku)
	}
	var fallback map[string]float64
	if len(missing) > 0 {
		fallback = CatalogCostBySKU(ctx, client, tenantID, missing)
	}
	return resolveLineUnitCosts(lines, fallback)
}

// resolveLineUnitCosts is the pure half of LineUnitCosts, split out so the precedence rule is
// testable without a database.
func resolveLineUnitCosts(lines []*ent.POSOrderLine, fallbackBySKU map[string]float64) map[uuid.UUID]float64 {
	out := make(map[uuid.UUID]float64, len(lines))
	for _, l := range lines {
		if l == nil {
			continue
		}
		if c := LineUnitCostAtSale(l); c != nil {
			out[l.ID] = *c
			continue
		}
		out[l.ID] = fallbackBySKU[l.Sku]
	}
	return out
}

// BlendedUnitCost is the per-unit cost of a line after extra units are added to it at a new cost,
// so the line's snapshot keeps matching the total COGS booked for it across the sale and the edit.
func BlendedUnitCost(oldUnitCost, oldQty, addedUnitCost, addedQty float64) float64 {
	total := oldQty + addedQty
	if total <= 0 {
		return addedUnitCost
	}
	return (oldUnitCost*oldQty + addedUnitCost*addedQty) / total
}
