package handlers

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	ordersmod "github.com/bengobox/pos-service/internal/modules/orders"
)

// itemAgg is one per-SKU profitability row — populated by MostProfitableItems (reports_
// profitability.go) and read by computeProfitabilityGroups below for the manufacturer/category/
// brand rollup. Package-scoped (not local to MostProfitableItems) so both can share it.
type itemAgg struct {
	SKU       string  `json:"sku"`
	Name      string  `json:"name"`
	UnitsSold float64 `json:"units_sold"`
	Revenue   float64 `json:"revenue"`
	UnitCost  float64 `json:"unit_cost"`
	Profit    float64 `json:"profit"`
	MarginPct float64 `json:"margin_pct"`
	// tax is this SKU's attributed share of order VAT (unexported — not part of the response,
	// used only to net revenue before computing Profit/MarginPct).
	tax float64
	// cost accumulates each line's OWN resolved cost (LineProfit — preferring that line's
	// point-in-time UnitCostAtSale snapshot over the live per-SKU cache) as it's summed, rather
	// than being derived after the fact from a single bucket-level UnitCost * UnitsSold. That
	// post-hoc multiply is wrong the moment a SKU's cost changed partway through the reporting
	// window (see report_attribution.go's UnitCostAtSale doc) — every line must contribute its own
	// cost. UnitCost above is then just cost/UnitsSold, a display-only weighted average.
	cost float64
}

// groupAggT is one row of a MostProfitableItems ?group_by= rollup.
type groupAggT struct {
	Group     string  `json:"group"`
	UnitsSold float64 `json:"units_sold"`
	Revenue   float64 `json:"revenue"`
	Profit    float64 `json:"profit"`
	MarginPct float64 `json:"margin_pct"`
	// netRevenue (unexported) is the margin-pct denominator — see itemAgg.tax above.
	netRevenue float64
}

// computeProfitabilityGroups computes MostProfitableItems' group_by rollup — shared by the JSON
// handler (reports_profitability.go's MostProfitableItems) and the grouped PDF/CSV export
// (report_pdf.go's ProfitabilityGroupedDocument) so the two surfaces can never disagree about the
// same numbers. Returns (nil, false) when groupBy isn't a recognized dimension, so callers can
// fall through to their own default (item-level ranking for JSON, or an error for the export).
//
// buckets/skus back the per-SKU dimensions (manufacturer/category/brand — an item-level attribute,
// rolled up from the already-computed per-SKU buckets). orders/costBySKU back the per-order
// dimensions (outlet/staff/day/customer — an order belongs to exactly one of each, so these
// aggregate directly off the order list rather than the per-SKU buckets); every line still uses
// the SAME AttributeOrderLines + costBySKU machinery, so a line's contribution here is identical
// to its contribution to the item-level ranking.
func computeProfitabilityGroups(ctx context.Context, db *ent.Client, log *zap.Logger, tid uuid.UUID, groupBy string, buckets map[string]*itemAgg, skus []string, orders []*ent.POSOrder, costBySKU map[string]float64, limit int) ([]*groupAggT, bool) {
	finishGroups := func(groups map[string]*groupAggT) []*groupAggT {
		groupRows := make([]*groupAggT, 0, len(groups))
		for _, g := range groups {
			if g.netRevenue != 0 {
				g.MarginPct = g.Profit / g.netRevenue * 100
			}
			groupRows = append(groupRows, g)
		}
		sort.Slice(groupRows, func(i, j int) bool {
			if groupRows[i].Profit != groupRows[j].Profit {
				return groupRows[i].Profit > groupRows[j].Profit
			}
			return groupRows[i].Revenue > groupRows[j].Revenue
		})
		if len(groupRows) > limit {
			groupRows = groupRows[:limit]
		}
		return groupRows
	}

	if groupBy == "manufacturer" || groupBy == "category" || groupBy == "brand" {
		metaBySKU := resolveManufacturerCategoryBySKU(ctx, db, tid, skus)
		groups := make(map[string]*groupAggT)
		for sku, b := range buckets {
			key := "Unspecified"
			if meta, ok := metaBySKU[sku]; ok {
				switch {
				case groupBy == "manufacturer" && meta.Manufacturer != "":
					key = meta.Manufacturer
				case groupBy == "category" && meta.CategoryName != "":
					key = meta.CategoryName
				case groupBy == "brand" && meta.Brand != "":
					key = meta.Brand
				}
			}
			g, ok := groups[key]
			if !ok {
				g = &groupAggT{Group: key}
				groups[key] = g
			}
			g.UnitsSold += b.UnitsSold
			g.Revenue += b.Revenue
			g.Profit += b.Profit
			g.netRevenue += b.Revenue - b.tax
		}
		return finishGroups(groups), true
	}

	if groupBy == "outlet" || groupBy == "staff" || groupBy == "day" || groupBy == "customer" {
		groups := make(map[string]*groupAggT)
		var staffIDs, outletKeys []uuid.UUID
		for _, o := range orders {
			var key string
			switch groupBy {
			case "outlet":
				key = o.OutletID.String()
				outletKeys = append(outletKeys, o.OutletID)
			case "staff":
				key = o.UserID.String()
				staffIDs = append(staffIDs, o.UserID)
			case "day":
				key = ordersmod.EffectiveOrderDate(o).UTC().Format("2006-01-02")
			case "customer":
				// Walk-ins (no phone captured) bucket under "Unknown" — POS orders have no direct
				// customer FK, only the phone/name captured at sale time.
				if o.CustomerPhone == nil || *o.CustomerPhone == "" {
					key = "Unknown"
				} else {
					key = *o.CustomerPhone
				}
			}
			g, ok := groups[key]
			if !ok {
				g = &groupAggT{Group: key}
				groups[key] = g
			}
			for _, al := range AttributeOrderLines(o) {
				g.UnitsSold += al.Quantity
				g.Revenue += al.Revenue
				netLineRevenue, _, profit := LineProfit(al, costBySKU[al.SKU])
				g.Profit += profit
				g.netRevenue += netLineRevenue
			}
			// customer display name: prefer the name captured at sale time (POSOrder.CustomerName)
			// over the phone key itself, best-effort — a later order for the same phone with no name
			// never downgrades an already-resolved display name.
			if groupBy == "customer" && key != "Unknown" && o.CustomerName != nil && *o.CustomerName != "" {
				g.Group = *o.CustomerName
			}
		}
		// Resolve outlet/staff display names in one batch each (customer already resolved above;
		// day's key is already the display value).
		switch groupBy {
		case "outlet":
			outletNames := make(map[uuid.UUID]string, len(outletKeys))
			if rows, oerr := db.Outlet.Query().Where(entoutlet.TenantID(tid), entoutlet.IDIn(outletKeys...)).All(ctx); oerr == nil {
				for _, o := range rows {
					outletNames[o.ID] = o.Name
				}
			}
			for key, g := range groups {
				if oid, perr := uuid.Parse(key); perr == nil {
					if name, ok := outletNames[oid]; ok && name != "" {
						g.Group = name
					}
				}
			}
		case "staff":
			staffNames := resolveStaffNames(ctx, db, log, tid, staffIDs)
			for key, g := range groups {
				if uid, perr := uuid.Parse(key); perr == nil {
					if name, ok := staffNames[uid]; ok && name != "" {
						g.Group = name
					}
				}
			}
		}
		return finishGroups(groups), true
	}

	return nil, false
}
