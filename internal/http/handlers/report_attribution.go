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
		var revenue float64
		if orderActiveGross > 0 {
			revenue = activeGross[i] / orderActiveGross * o.TotalAmount
		}
		out[i] = AttributedLine{
			OrderID:      o.ID,
			SKU:          l.Sku,
			Name:         l.Name,
			Category:     l.Category,
			KdsStationID: l.KdsStationID,
			Quantity:     activeQty[i],
			Revenue:      revenue,
		}
	}
	return out
}
