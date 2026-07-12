package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsstation "github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/modules/docs"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// KDSStationBreakdownRow is one station's slice of a sales-by-KDS-station report — the same
// grouping the kitchen/bar tickets use, so "how much did the bar do today" always matches what
// actually printed/displayed at that station.
//
// Revenue is each station's PRORATED SHARE of its orders' actual net total_amount (the same
// payable figure Sales-by-Staff sums) — NOT a raw sum of line.total_price. Summing Revenue
// across every row (incl. "Unassigned") for the same date range/outlet therefore always equals
// the Sales-by-Staff total exactly; see computeKDSStationBreakdown for why a naive line-gross
// sum was wrong.
type KDSStationBreakdownRow struct {
	StationID   string  `json:"station_id"`
	StationName string  `json:"station_name"`
	StationType string  `json:"station_type"`
	OrderCount  int     `json:"order_count"`
	ItemCount   float64 `json:"item_count"`
	Revenue     float64 `json:"revenue"`
}

// computeKDSStationBreakdown aggregates completed orders in [from,to] (optionally scoped to one
// outlet) by the KDS station each line belongs to. It is the single source of truth for "sales by
// station" — used by the JSON report, the PDF/CSV document, and the daily-close breakdown — so all
// three always agree.
//
// Every line created after the kds_station_id backfill (2026-07-07) carries its resolved station
// directly; any older/legacy line with a null kds_station_id is resolved on the fly via the same
// priority order routeLinesToStations uses, scoped to its OWN order's outlet (a multi-outlet report
// mixes outlets with different station configs, so this can't be hoisted out of the loop).
//
// Revenue attribution (fixed 2026-07-12 — diagnosed against urban-loft prod data): this used to
// sum each line's raw total_price directly, which diverged from Sales-by-Staff (sum of
// order.total_amount) in two ways, confirmed live:
//  1. A partially/fully VOIDED line still contributed its full pre-void total_price — the exact
//     bug already fixed in the order-recompute path (service.go ~L1544) but never applied here.
//     Reproduced: 2026-07-09 station sum read 62140 vs the correct 61690 (450 = one voided line).
//  2. Even with zero voids, line.total_price is gross (pre-tax, pre-discount, pre-charges,
//     pre-round-off) while total_amount is the net payable — so ANY order carrying a discount
//     (e.g. the happy-hour auto-discount) inflates the station sum by exactly that discount.
//     Reproduced: 2026-07-11 gap of 4600 after the void fix == that day's discount_total exactly.
//
// Fix: each line's contribution to its order is first scaled by voided_qty (active fraction),
// then the order's ENTIRE net total_amount is prorated across its lines by their SHARE of the
// order's active gross value — so every tax/discount/charges/round-off cent lands on a station
// too, and the per-station rows sum EXACTLY to the same total Sales-by-Staff reports. Staff
// totals (order.total_amount) were always the accurate figure; this makes KDS-station agree.
func computeKDSStationBreakdown(ctx context.Context, db *ent.Client, tid uuid.UUID, oid *uuid.UUID, from, to time.Time) ([]KDSStationBreakdownRow, error) {
	preds := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.StatusEQ("completed"),
		effectiveDateGTE(from),
		effectiveDateLTE(to),
	}
	if oid != nil {
		preds = append(preds, posorder.OutletID(*oid))
	}
	ordersList, err := db.POSOrder.Query().Where(preds...).WithLines().All(ctx)
	if err != nil {
		return nil, err
	}

	type bucket struct {
		name, stype        string
		orderIDs           map[uuid.UUID]struct{}
		itemCount, revenue float64
	}
	byStation := make(map[uuid.UUID]*bucket)
	unassigned := &bucket{name: "Unassigned", stype: "", orderIDs: map[uuid.UUID]struct{}{}}

	stationsByOutlet := make(map[uuid.UUID][]*ent.KDSStation)
	stationNameByID := make(map[uuid.UUID]*ent.KDSStation)

	for _, o := range ordersList {
		stations, ok := stationsByOutlet[o.OutletID]
		if !ok {
			stations, _ = db.KDSStation.Query().
				Where(entkdsstation.TenantID(tid), entkdsstation.OutletID(o.OutletID), entkdsstation.IsActive(true)).
				All(ctx)
			stationsByOutlet[o.OutletID] = stations
			for _, st := range stations {
				stationNameByID[st.ID] = st
			}
		}

		// Attributed lines carry each line's void-adjusted quantity and its prorated share of
		// the order's actual net total_amount (see AttributeOrderLines) — NOT raw
		// quantity/total_price, which is what previously made this report disagree with
		// Sales-by-Staff whenever an order had a void or a discount.
		attributed := AttributeOrderLines(o)
		for i, l := range o.Edges.Lines {
			al := attributed[i]
			stationID := l.KdsStationID
			if stationID == nil {
				stationID = orders.ResolveStationForLineOrFallback(l.Name, l.Category, nil, stations)
			}
			if stationID == nil {
				// A fully-voided line (activeQty 0) shouldn't count this order as having
				// touched "Unassigned" — nothing active actually landed there.
				if al.Quantity > 0 {
					unassigned.orderIDs[o.ID] = struct{}{}
				}
				unassigned.itemCount += al.Quantity
				unassigned.revenue += al.Revenue
				continue
			}
			b, ok := byStation[*stationID]
			if !ok {
				name, stype := "Unassigned", ""
				if st := stationNameByID[*stationID]; st != nil {
					name, stype = st.Name, string(st.StationType)
				}
				b = &bucket{name: name, stype: stype, orderIDs: map[uuid.UUID]struct{}{}}
				byStation[*stationID] = b
			}
			// Same guard: a fully-voided line shouldn't inflate this station's order count.
			if al.Quantity > 0 {
				b.orderIDs[o.ID] = struct{}{}
			}
			b.itemCount += al.Quantity
			b.revenue += al.Revenue
		}
	}

	rows := make([]KDSStationBreakdownRow, 0, len(byStation)+1)
	for id, b := range byStation {
		rows = append(rows, KDSStationBreakdownRow{
			StationID:   id.String(),
			StationName: b.name,
			StationType: b.stype,
			OrderCount:  len(b.orderIDs),
			ItemCount:   b.itemCount,
			Revenue:     b.revenue,
		})
	}
	if len(unassigned.orderIDs) > 0 {
		rows = append(rows, KDSStationBreakdownRow{
			StationID:   "",
			StationName: unassigned.name,
			StationType: "",
			OrderCount:  len(unassigned.orderIDs),
			ItemCount:   unassigned.itemCount,
			Revenue:     unassigned.revenue,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Revenue > rows[j].Revenue })
	return rows, nil
}

// SalesByKDSStation handles GET /{tenantID}/pos/reports/sales/by-kds-station
// Returns revenue, item count and order count grouped by KDS station (kitchen, bar, etc.) — the
// same grouping the kitchen/bar KDS screens use — so a manager can see "how much did the bar do
// today" vs "how much did the kitchen do today" at a glance.
func (h *ReportsHandler) SalesByKDSStation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	from, to := parseDateRange(r, requestTenantLocation(r, h.db))
	oid := reportOutletScope(r)

	rows, err := computeKDSStationBreakdown(r.Context(), h.db, tid, oid, from, to)
	if err != nil {
		h.log.Error("by-kds-station query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": rows, "total": len(rows)})
}

// SalesByKDSStationDoc handles GET /{tenantID}/pos/reports/sales-by-kds-station-document — one
// table row per KDS station (orders, items, revenue) with a report total and a chart, via the
// SAME computeKDSStationBreakdown the JSON endpoint uses. PDF/CSV via ?format=.
func (h *ReportPDFHandler) SalesByKDSStationDoc(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)
	from, to := parseReportRange(r, requestTenantLocation(r, h.db))

	rows, err := computeKDSStationBreakdown(ctx, h.db, tid, oid, from, to)
	if err != nil {
		h.log.Error("sales-by-kds-station: query failed", zap.Error(err))
		jsonError(w, "failed to generate sales by KDS station", http.StatusInternalServerError)
		return
	}

	var grandRevenue float64
	tableRows := make([][]docs.Cell, 0, len(rows))
	bars := make([]docs.Bar, 0, len(rows))
	for _, row := range rows {
		grandRevenue += row.Revenue
		label := row.StationName
		if row.StationType != "" {
			label = fmt.Sprintf("%s (%s)", row.StationName, row.StationType)
		}
		tableRows = append(tableRows, []docs.Cell{
			docs.Text(label),
			docs.Text(strconv.Itoa(row.OrderCount)),
			docs.Text(fmtQty(row.ItemCount)),
			docs.Text(fmtAmount(row.Revenue)),
		})
		bars = append(bars, docs.Bar{Label: row.StationName, Value: row.Revenue})
	}

	report := h.newReport(ctx, tid, oid, "Sales by KDS Station", "", from, to, false)
	report.Cards = []docs.Card{
		{Label: "Total Revenue", Value: "KES " + fmtAmount(grandRevenue)},
		{Label: "Stations", Value: strconv.Itoa(len(rows))},
	}
	report.Sections = []docs.Section{
		{
			Kind:  docs.SectionTable,
			Title: "Sales by Station",
			Columns: []docs.Column{
				{Header: "Station", Weight: 1.6},
				{Header: "Orders", Weight: 1, Align: "R"},
				{Header: "Items", Weight: 1, Align: "R"},
				{Header: "Revenue", Weight: 1.2, Money: true},
			},
			Rows:  tableRows,
			Total: []docs.Cell{docs.BoldText("Report Total"), docs.Text(""), docs.Text(""), docs.BoldText(fmtAmount(grandRevenue))},
		},
		{Kind: docs.SectionChart, Title: "Revenue by Station", ValueUnit: "KES", Bars: bars},
	}
	h.write(w, r, report, "sales-by-kds-station")
}

// reportOutletScope resolves the same outlet_id precedence report_pdf.go's outletScope uses
// (explicit ?outlet_id= wins, else the request's active outlet), for JSON report handlers that
// aren't methods on ReportPDFHandler.
func reportOutletScope(r *http.Request) *uuid.UUID {
	if s := r.URL.Query().Get("outlet_id"); s != "" {
		if oid, err := uuid.Parse(s); err == nil {
			return &oid
		}
	}
	if s := httpware.GetOutletID(r.Context()); s != "" {
		if oid, err := uuid.Parse(s); err == nil {
			return &oid
		}
	}
	return nil
}
