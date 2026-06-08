package handlers

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/google/uuid"
	"go.uber.org/zap"

	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/posorder"
)

// MostProfitableItems handles GET /{tenantID}/pos/reports/most-profitable?from=&to=&limit=
//
// Ranks items by profitability over a date range, computed from FINALIZED (status="completed")
// POS sales. Lines are aggregated by sku:
//
//	units_sold = sum(quantity)
//	revenue    = sum(quantity * unit_price)
//	unit_cost  = resolved from POSCatalogOverride.metadata["cost_price"] for the sku, else 0
//	            (POSCatalogOverride has no dedicated cost column; cost lives in inventory-api.
//	             When the projection later stashes a cost in metadata it is picked up here,
//	             otherwise cost defaults to 0 and profit falls back to revenue.)
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

	from, to := parseDateRange(r)

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
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLTE(to),
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
		for _, l := range o.Edges.Lines {
			b, ok := buckets[l.Sku]
			if !ok {
				b = &itemAgg{SKU: l.Sku, Name: l.Name}
				buckets[l.Sku] = b
			}
			if b.Name == "" {
				b.Name = l.Name
			}
			b.UnitsSold += l.Quantity
			b.Revenue += l.Quantity * l.UnitPrice
		}
	}

	// Resolve unit cost per sku from the POSCatalogOverride projection.
	// POSCatalogOverride has no cost column; a cost may be stashed in metadata["cost_price"]
	// by the inventory sync. When absent, unit_cost stays 0.
	for sku, b := range buckets {
		b.UnitCost = h.resolveUnitCost(r, tid, sku)
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

// resolveUnitCost looks up a per-unit cost for a sku from the tenant's POSCatalogOverride
// projection. POSCatalogOverride carries no dedicated cost column, so the cost is read from
// metadata["cost_price"] when the inventory sync has populated it; otherwise this returns 0.
func (h *ReportsHandler) resolveUnitCost(r *http.Request, tid uuid.UUID, sku string) float64 {
	ov, err := h.db.POSCatalogOverride.Query().
		Where(
			entoverride.TenantID(tid),
			entoverride.InventorySku(sku),
		).
		First(r.Context())
	if err != nil || ov == nil || ov.Metadata == nil {
		return 0
	}
	switch v := ov.Metadata["cost_price"].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}
