package handlers

import (
	"net/http"
	"sort"
	"strconv"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
)

// MostProfitableItems handles GET /{tenantID}/pos/reports/most-profitable?from=&to=&limit=
//
// Ranks items by profitability over a date range, computed from FINALIZED (status="completed")
// POS sales. Lines are aggregated by sku:
//
//	units_sold = sum(quantity)
//	revenue    = sum(quantity * unit_price)
//	unit_cost  = the item's real production/goods cost from inventory-api (see resolveUnitCosts):
//	             GOODS/other stockable types use Item.cost_price (purchase cost); RECIPE items use
//	             the recipe's cost_per_portion (ingredient cost — RECIPE items have no purchase
//	             cost of their own). Falls back to 0 (and thus profit==revenue) only when
//	             inventory-api has no cost data for that sku at all.
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

	type itemAgg struct {
		SKU       string  `json:"sku"`
		Name      string  `json:"name"`
		UnitsSold float64 `json:"units_sold"`
		Revenue   float64 `json:"revenue"`
		UnitCost  float64 `json:"unit_cost"`
		Profit    float64 `json:"profit"`
		MarginPct float64 `json:"margin_pct"`
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
		}
	}

	// Resolve real per-sku costs from inventory-api in one batch (not N+1) — see
	// resolveUnitCostsBySKU for the GOODS-cost_price vs RECIPE-cost_per_portion split.
	costBySKU := resolveUnitCostsBySKU(r, h.db, h.log)
	for sku, b := range buckets {
		b.UnitCost = costBySKU[sku]
		b.Profit = b.Revenue - b.UnitCost*b.UnitsSold
		if b.Revenue != 0 {
			b.MarginPct = b.Profit / b.Revenue * 100
		}
	}

	rows := make([]*itemAgg, 0, len(buckets))
	var totalRevenue, totalProfit float64
	for _, b := range buckets {
		rows = append(rows, b)
		totalRevenue += b.Revenue
		totalProfit += b.Profit
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

	jsonOK(w, map[string]any{
		"currency":      currency,
		"from":          from.Format("2006-01-02"),
		"to":            to.Format("2006-01-02"),
		"total_revenue": totalRevenue,
		"total_profit":  totalProfit,
		"items":         rows,
	})
}

// resolveUnitCostsBySKU fetches the tenant's full inventory catalog and returns sku → real unit
// cost: Item.cost_price (falling back to purchase_price) for GOODS/other stockable types, or
// the recipe's cost_per_portion for RECIPE items — the same precedence inventory-api's own
// enrichPrices applies (items/pricing_enrich.go), and the same source pos-api's own catalog
// assembly uses for its (view_cost-gated) CostPrice column. Degrades to an empty map (all costs
// 0, profit falls back to revenue) on proxy failure rather than failing the whole report.
//
// Shared by every report that needs a true per-item cost — MostProfitableItems (JSON) and
// MostProfitablePDF/SalesByHourDoc (documents) — so the profit margin a tenant sees on screen and
// in an exported PDF always agree. Previously the PDF path (report_pdf.go's old resolveUnitCost)
// read POSCatalogOverride.metadata["cost_price"] per-sku — a field nothing in this codebase ever
// wrote, so unit_cost there was silently always 0 regardless of this fix landing in the JSON path.
func resolveUnitCostsBySKU(r *http.Request, db *ent.Client, log *zap.Logger) map[string]float64 {
	out := map[string]float64{}
	tenantSlug := resolveTenantSlug(r, db)
	if tenantSlug == "" {
		log.Warn("resolveUnitCostsBySKU: could not resolve tenant slug for cost lookup")
		return out
	}
	items, err := fetchInventoryItems(r.Context(), tenantSlug, "", "")
	if err != nil {
		log.Warn("resolveUnitCostsBySKU: inventory items fetch failed — costs will be 0", zap.Error(err))
		return out
	}
	for _, item := range items {
		if cp := firstNonNilFloat(item.CostPrice, item.PurchasePrice); cp != nil && *cp > 0 {
			out[item.SKU] = *cp
		}
	}
	return out
}
