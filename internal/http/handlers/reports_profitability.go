package handlers

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	ordersmod "github.com/bengobox/pos-service/internal/modules/orders"
)

// MostProfitableItems handles GET /{tenantID}/pos/reports/most-profitable?from=&to=&limit=
//
// Ranks items by profitability over a date range, computed from FINALIZED (status="completed")
// POS sales. Lines are aggregated by sku:
//
//	units_sold = sum(quantity)
//	revenue    = sum(quantity * unit_price)
//	unit_cost  = the item's real production/goods cost, read from the local POSCatalogOverride
//	             cache (see resolveUnitCostsBySKU) — GOODS/other stockable types use
//	             Item.cost_price (purchase cost); RECIPE items use the recipe's cost_per_portion
//	             (ingredient cost — RECIPE items have no purchase cost of their own), computed
//	             server-side by inventory-api into the same cost_price field before it's synced
//	             here. Falls back to 0 (and thus profit==revenue) only when the cache has no cost
//	             data for that sku at all.
//	profit     = revenue - unit_cost * units_sold
//	margin_pct = profit / revenue   (0 when revenue is 0)
//
// Results are sorted by profit DESC; when no row carries cost data this is equivalent
// to ranking by revenue. Default window is the last 30 days, default limit 20.
// This endpoint is query-only — no schema change, no migration.
func (h *ReportsHandler) MostProfitableItems(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r, requestTenantLocation(r, h.db))

	limit := 20
	if ls := r.URL.Query().Get("limit"); ls != "" {
		if n, err2 := strconv.Atoi(ls); err2 == nil && n > 0 {
			limit = n
		}
	}
	// groupBy: "manufacturer"/"category"/"brand" roll the same per-sku profitability numbers up to
	// that item-level attribute instead of ranking individual items — same cost/profit math,
	// different aggregation key (see the block below). "outlet"/"staff"/"day"/"customer" instead
	// group by an ORDER-level attribute (see the second block, further down, which aggregates
	// directly off `orders` rather than the per-sku `buckets`). Empty (default) keeps the existing
	// per-item ranking.
	groupBy := r.URL.Query().Get("group_by")

	orders, err := h.db.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.StatusEQ("completed"),
			effectiveDateGTE(from),
			effectiveDateLTE(to),
		).
		WithLines().
		All(r.Context())
	if err != nil {
		h.log.Error("most-profitable query failed", zap.Error(err))
		jsonError(w, "failed to generate most-profitable report", http.StatusInternalServerError)
		return
	}

	buckets := make(map[string]*itemAgg)
	currency := "KES"
	for _, o := range orders {
		if o.Currency != "" {
			currency = o.Currency
		}
		// AttributeOrderLines (see report_attribution.go) fixes the same two bugs found across
		// every line-level report: a voided line no longer contributes its pre-void units, and
		// revenue is each line's prorated share of order.TotalAmount (net of discount/tax/
		// charges/round-off) rather than quantity*unit_price — so profitability now agrees with
		// Sales-by-Staff too, not just its own internal cost math.
		for i, al := range AttributeOrderLines(o) {
			l := o.Edges.Lines[i]
			b, ok := buckets[al.SKU]
			if !ok {
				b = &itemAgg{SKU: al.SKU, Name: l.Name}
				buckets[al.SKU] = b
			}
			if b.Name == "" {
				b.Name = l.Name
			}
			b.UnitsSold += al.Quantity
			b.Revenue += al.Revenue
			b.tax += al.Tax
		}
	}

	// Resolve real per-sku costs from the local POSCatalogOverride cache in one batch (not N+1, and
	// not a live inventory-api call — see resolveUnitCostsBySKU).
	skus := make([]string, 0, len(buckets))
	for sku := range buckets {
		skus = append(skus, sku)
	}
	costBySKU := resolveUnitCostsBySKU(r.Context(), h.db, tid, skus)
	var skusMissingCost int
	for sku, b := range buckets {
		cost, ok := costBySKU[sku]
		if !ok || cost == 0 {
			skusMissingCost++
		}
		b.UnitCost = cost
		// netRevenue excludes VAT — the same centralized basis LineProfit/SumLineProfits use for
		// GetSummary/SalesSummary/SalesByHour, so this ranking's Profit/MarginPct never disagrees
		// with the dashboard. Revenue itself (displayed per row) stays VAT-inclusive.
		netRevenue := b.Revenue - b.tax
		b.Profit = netRevenue - b.UnitCost*b.UnitsSold
		if netRevenue != 0 {
			b.MarginPct = b.Profit / netRevenue * 100
		}
	}

	var totalRevenue, totalProfit float64
	for _, b := range buckets {
		totalRevenue += b.Revenue
		totalProfit += b.Profit
	}

	resp := map[string]any{
		"currency":          currency,
		"from":              from.Format("2006-01-02"),
		"to":                to.Format("2006-01-02"),
		"total_revenue":     totalRevenue,
		"total_profit":      totalProfit,
		"skus_missing_cost": skusMissingCost,
	}

	if groups, recognized := computeProfitabilityGroups(r.Context(), h.db, h.log, tid, groupBy, buckets, skus, orders, costBySKU, limit); recognized {
		resp["group_by"] = groupBy
		resp["groups"] = groups
		jsonOK(w, resp)
		return
	}

	rows := make([]*itemAgg, 0, len(buckets))
	for _, b := range buckets {
		rows = append(rows, b)
	}

	// Sort by profit DESC; tie-break on revenue DESC so that with no cost data
	// (all profit == revenue) the ranking is stable and revenue-ordered.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Profit != rows[j].Profit {
			return rows[i].Profit > rows[j].Profit
		}
		return rows[i].Revenue > rows[j].Revenue
	})

	if len(rows) > limit {
		rows = rows[:limit]
	}

	resp["items"] = rows
	jsonOK(w, resp)
}

// resolveUnitCostsBySKU resolves the tenant's real per-sku unit cost from the LOCAL
// POSCatalogOverride cache (metadata["cost_price"], kept fresh by the
// inventory.item.created/updated/cost_changed event consumer — see
// catalog.InventoryEventHandler) rather than a live S2S call to inventory-api.
//
// Previously this made a live paginated fetch of the tenant's ENTIRE catalog on every single
// report request (up to ~39 sequential HTTP calls for a 3,800-item catalog) just to build this
// map — slow, and it silently degrades to "all costs 0" (gross profit == revenue) the moment
// inventory-api's per-IP rate limiter (100 req/60s) rejects even one page of that walk, which is
// exactly what happened in production for a large-catalog tenant (boi-enterprises, 2026-08-10).
// The cache this now reads is the SAME one sale-time COGS posting and returns/reversals already
// rely on (orders.CatalogCostBySKU) — one query scoped to only the SKUs this report actually
// needs, zero network calls, and it can never silently blank out real costs just because a
// network path degraded.
//
// Shared by every report that needs a true per-item cost — MostProfitableItems, GetSummary, the
// range summary, and SalesByHour (all in reports.go) — so the profit margin a tenant sees on the
// dashboard, in most-profitable rankings, and in exported PDFs always agree.
func resolveUnitCostsBySKU(ctx context.Context, db *ent.Client, tenantID uuid.UUID, skus []string) map[string]float64 {
	return ordersmod.CatalogCostBySKU(ctx, db, tenantID, skus)
}

// resolveManufacturerCategoryBySKU resolves manufacturer/category_name/brand_name from the SAME
// local POSCatalogOverride cache resolveUnitCostsBySKU reads (one query, all fields), for the
// group_by=manufacturer|category|brand rollup on MostProfitableItems. Missing entries just come
// back zero-valued (the caller already falls back to "Unspecified").
func resolveManufacturerCategoryBySKU(ctx context.Context, db *ent.Client, tenantID uuid.UUID, skus []string) map[string]ordersmod.CatalogCacheEntry {
	return ordersmod.CatalogCacheBySKU(ctx, db, tenantID, skus)
}
