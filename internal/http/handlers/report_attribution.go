package handlers

import (
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// AttributedLine is one order line's prorated share of its order's actual net total_amount —
// the single canonical attribution EVERY line-level report (by SKU/item, by category, by KDS
// station, by order-subtype/product-mix) must use, so they can never diverge from Sales-by-Staff
// (which sums order.total_amount directly) or from each other.
//
// Diagnosed 2026-07-12 against live urban-loft data: computeKDSStationBreakdown, SalesByCategory,
// and ProductMix each independently summed line.TotalPrice/line.Quantity raw. That was wrong in
// two confirmed, reproduced ways:
//  1. A partially/fully VOIDED line (soft-void via voided_qty, kept for audit) still contributed
//     its full pre-void total_price/quantity — the exact bug already fixed in the order-recompute
//     path (orders/service.go ~L1544) but never applied to any report. Reproduced: a day with one
//     voided line read 450 KES higher than the correct (staff-report) total.
//  2. line.TotalPrice is gross (pre-tax, pre-discount, pre-charges, pre-round-off) while
//     order.TotalAmount is the net payable — so ANY discount (e.g. the happy-hour auto-discount)
//     inflated every line-level report by that discount. Reproduced: a day's gap, after removing
//     the void impact, equalled that day's discount_total to the cent.
//
// AttributeOrderLines fixes both: each line's contribution is first scaled by its void-active
// fraction, then the order's ENTIRE net total_amount is prorated across lines by their share of
// the order's active gross value. Summing Revenue across ALL lines of ALL orders in a date range
// therefore always equals the Sales-by-Staff total (sum of order.total_amount) exactly.
type AttributedLine struct {
	OrderID      uuid.UUID
	SKU          string
	Name         string
	Category     string
	KdsStationID *uuid.UUID
	// Quantity is the ACTIVE (post-void) quantity — voided units were never actually sold/served.
	Quantity float64
	// Revenue is this line's prorated share of order.TotalAmount (net of discount, incl. its
	// share of tax/charges/round-off) — NOT line.TotalPrice.
	Revenue float64
	// Tax is this line's prorated share of order.TaxTotal, using the SAME void-adjusted
	// active-gross fraction as Revenue — so a partially-voided or discounted line's tax
	// contribution shrinks exactly in step with its revenue. Used by LineProfit/SumLineProfits to
	// net VAT out of Revenue before computing profit (see those functions' doc comments for why
	// Revenue itself, which every non-profit report already relies on, is left VAT-inclusive).
	Tax float64
	// UnitCostAtSale is the per-unit cost captured at the moment this line was actually sold
	// (payments.Service.publishSaleFinalized snapshots it into
	// pos_order_lines.metadata["unit_cost_at_sale"], reusing the SAME POSCatalogOverride cache
	// lookup already used to post COGS to treasury for this exact sale — no new cost pipeline).
	// nil for lines sold before this field existed, or whose cost was unknown at sale time;
	// LineProfit/SumLineProfits fall back to the live cache for those, unchanged.
	//
	// Without this, changing an item's cost_price after the fact (correcting a bulk-import pricing
	// error, or converting a unit basis like ml->bottle) silently rewrites EVERY past period's
	// profit report, since resolveUnitCostsBySKU always reads today's cost against historical
	// revenue. Confirmed live against small-steps-cosmetics 2026-09-01: 3 SKUs migrated from
	// ml-tracked (cost 9) to bottle-tracked (cost 450) fabricated a ~7,980 KES phantom loss on
	// pre-migration ml sales once the live cache caught up to the new cost.
	UnitCostAtSale *float64
}

// lineUnitCostSnapshot reads the point-in-time cost persisted at sale time (see UnitCostAtSale
// above). Numbers decode as float64 after a JSON round-trip, but an int is accepted too in case a
// caller ever writes one directly.
func lineUnitCostSnapshot(l *ent.POSOrderLine) *float64 {
	if l.Metadata == nil {
		return nil
	}
	switch v := l.Metadata["unit_cost_at_sale"].(type) {
	case float64:
		return &v
	case int:
		f := float64(v)
		return &f
	}
	return nil
}

// AttributeOrderLines prorates one order's net total_amount across its active lines. Orders with
// zero active gross value (fully voided, or a 100%-discounted/free order) contribute lines with
// Revenue=0 rather than dividing by zero.
func AttributeOrderLines(o *ent.POSOrder) []AttributedLine {
	lines := o.Edges.Lines
	activeQty := make([]float64, len(lines))
	activeGross := make([]float64, len(lines))
	var orderActiveGross float64
	for i, l := range lines {
		fraction := 1.0
		if l.VoidedQty != nil && l.Quantity > 0 {
			fraction = (l.Quantity - *l.VoidedQty) / l.Quantity
			if fraction < 0 {
				fraction = 0
			}
		}
		activeQty[i] = l.Quantity * fraction
		activeGross[i] = l.TotalPrice * fraction
		orderActiveGross += activeGross[i]
	}
	out := make([]AttributedLine, len(lines))
	for i, l := range lines {
		var revenue, tax float64
		if orderActiveGross > 0 {
			fraction := activeGross[i] / orderActiveGross
			revenue = fraction * o.TotalAmount
			tax = fraction * o.TaxTotal
		}
		out[i] = AttributedLine{
			OrderID:        o.ID,
			SKU:            l.Sku,
			Name:           l.Name,
			Category:       l.Category,
			KdsStationID:   l.KdsStationID,
			Quantity:       activeQty[i],
			Revenue:        revenue,
			Tax:            tax,
			UnitCostAtSale: lineUnitCostSnapshot(l),
		}
	}
	return out
}

// LineProfit is the single formula every profit report (GetSummary, SalesSummary,
// MostProfitableItems, SalesByHour, DailySales, ExportDailyReport, SalesByStaffPDF,
// MostProfitablePDF, SalesByHourDoc) must use to turn an attributed line + its resolved unit cost
// into a profit figure. netRevenue nets tax out of al.Revenue first — matching what treasury
// actually books as Revenue at sale time (Cr Revenue = amount - tax_total, Cr VAT Payable =
// tax_total; see finance-service/treasury-api internal/modules/pos/subscriber.go
// handleSaleFinalized) — rather than the VAT-inclusive amount the customer paid. Every OTHER use
// of al.Revenue (total_revenue KPIs, Sales-by-Category, Sales-by-Staff, etc.) is deliberately left
// untouched and stays VAT-inclusive, matching what a receipt/invoice actually shows; only the
// profit/margin calculation itself must be net-of-tax to ever reconcile with treasury's P&L.
//
// unitCost is the per-unit cost resolved via resolveUnitCostsBySKU (0 when the local
// POSCatalogOverride cache has no cost data for the SKU — an honest, unchanged fallback, not a
// bug in itself; see TestGetSummary_GrossProfitZeroCost_WhenNoCatalogCacheRow). Callers that want
// COVERAGE visibility (whether that 0 means "genuinely free" or "cost data missing") should also
// check costBySKU's own ", ok" map lookup — see SumLineProfits.
//
// al.UnitCostAtSale, when present, OVERRIDES the passed-in unitCost — a line's own point-in-time
// cost snapshot always wins over a fresh live-cache lookup, since the latter reflects whatever the
// item's cost happens to be TODAY, not what it was when this line was actually sold (see
// UnitCostAtSale's doc comment for the concrete phantom-loss bug this fixes).
func LineProfit(al AttributedLine, unitCost float64) (netRevenue, cost, profit float64) {
	if al.UnitCostAtSale != nil {
		unitCost = *al.UnitCostAtSale
	}
	netRevenue = al.Revenue - al.Tax
	cost = unitCost * al.Quantity
	return netRevenue, cost, netRevenue - cost
}

// ProfitTotals is the aggregate result of summing LineProfit across a set of attributed lines,
// plus a cost-coverage signal: SKUsMissingCost counts distinct sold SKUs the cost cache had NO
// entry for at all (or an explicit zero), so a future sync gap (like the one that produced
// artificially-inflated margins for boi-enterprises, 2026-08-10 through 2026-08-13) is visible on
// the dashboard/report instead of silently hiding inside an inflated Gross Profit number. This is
// purely additive visibility — it does not change the profit/margin math itself, which keeps the
// existing, deliberate 0-cost fallback.
type ProfitTotals struct {
	Revenue         float64
	Cost            float64
	Profit          float64
	MarginPct       float64
	SKUsMissingCost int
}

// SumLineProfits aggregates LineProfit across every line, resolving each line's unit cost from
// costBySKU (as produced by resolveUnitCostsBySKU/orders.CatalogCacheBySKU).
func SumLineProfits(lines []AttributedLine, costBySKU map[string]float64) ProfitTotals {
	var t ProfitTotals
	missing := map[string]struct{}{}
	for _, al := range lines {
		unitCost, ok := costBySKU[al.SKU]
		if al.UnitCostAtSale != nil {
			unitCost, ok = *al.UnitCostAtSale, true
		}
		if !ok || unitCost == 0 {
			missing[al.SKU] = struct{}{}
		}
		netRevenue, cost, profit := LineProfit(al, unitCost)
		t.Revenue += netRevenue
		t.Cost += cost
		t.Profit += profit
	}
	t.SKUsMissingCost = len(missing)
	if t.Revenue != 0 {
		t.MarginPct = t.Profit / t.Revenue * 100
	}
	return t
}

// collectOrderSKUs returns the distinct set of SKUs across every line of every given order
// (orders must already have Edges.Lines loaded via WithLines()). Used to scope a
// resolveUnitCostsBySKU/resolveManufacturerCategoryBySKU cache lookup to exactly the SKUs a report
// needs, instead of resolving cost for the tenant's entire catalog. Deliberately reads raw
// o.Edges.Lines rather than AttributeOrderLines — a voided line still needs its cost resolved if
// any OTHER, non-voided report elsewhere sums it, and including a few harmless extra SKUs in the
// cache query costs nothing (it's one indexed query, not a per-SKU network call).
func collectOrderSKUs(orderLists ...[]*ent.POSOrder) []string {
	set := make(map[string]struct{})
	for _, orders := range orderLists {
		for _, o := range orders {
			for _, l := range o.Edges.Lines {
				set[l.Sku] = struct{}{}
			}
		}
	}
	skus := make([]string, 0, len(set))
	for sku := range set {
		skus = append(skus, sku)
	}
	return skus
}
