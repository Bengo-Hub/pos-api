package handlers

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsstation "github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/modules/docs"
	ordersmod "github.com/bengobox/pos-service/internal/modules/orders"
)

// This file adds branded PDF/CSV document endpoints (via ReportPDFHandler.write, which already
// dispatches ?format=pdf|csv from a single docs.Report) for the four Analytics-page reports that
// previously had no document export at all: Sales by Hour, Sales by Category, Product Mix, and
// Voids. Each mirrors the equivalent JSON aggregation in reports.go/reports_extended.go exactly,
// so the printed/exported figures always match what the UI shows.

// ── Sales by Hour ────────────────────────────────────────────────────────────────

// SalesByHourDoc handles GET /{tenantID}/pos/reports/sales-by-hour-document?date=&outlet_id=&format=
// Single-day hour-of-day breakdown — mirrors ReportsHandler.SalesByHour's bucketing exactly.
func (h *ReportPDFHandler) SalesByHourDoc(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)

	loc := tenantLocation(ctx, h.db, tid)
	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		dateStr = time.Now().In(loc).Format("2006-01-02")
	}
	dayStart, err := parseDayStartIn(dateStr, loc)
	if err != nil {
		jsonError(w, "invalid date, use YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	dayEnd := dayStart.Add(24 * time.Hour)

	preds := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.StatusEQ("completed"),
		effectiveDateGTE(dayStart),
		effectiveDateLT(dayEnd),
	}
	if oid != nil {
		preds = append(preds, posorder.OutletID(*oid))
	}
	orders, err := h.db.POSOrder.Query().Where(preds...).WithLines().All(ctx)
	if err != nil {
		h.log.Error("sales-by-hour-document: query failed", zap.Error(err))
		jsonError(w, "failed to generate sales by hour report", http.StatusInternalServerError)
		return
	}

	type hourBucket struct {
		orders        int
		revenue, cost float64
	}
	buckets := make([]hourBucket, 24)
	var totalRevenue, totalCost float64
	var totalOrders int

	// Real per-sku cost (GOODS Item.cost_price vs RECIPE cost_per_portion) — see
	// resolveUnitCostsBySKU. Mirrors ReportsHandler.SalesByHour's profit calc exactly.
	costBySKU := resolveUnitCostsBySKU(r, h.db, h.log)
	for _, o := range orders {
		hr := o.CreatedAt.In(loc).Hour()
		buckets[hr].orders++
		buckets[hr].revenue += o.TotalAmount
		totalOrders++
		totalRevenue += o.TotalAmount
		// Cost must use the ACTIVE (void-adjusted) quantity — a voided line was never actually
		// sold, so costing it overstates cost and understates the printed profit margin.
		for _, al := range AttributeOrderLines(o) {
			c := costBySKU[al.SKU] * al.Quantity
			buckets[hr].cost += c
			totalCost += c
		}
	}
	peakHour, peakRevenue := 0, 0.0
	for hr, b := range buckets {
		if b.revenue > peakRevenue {
			peakHour, peakRevenue = hr, b.revenue
		}
	}

	rows := make([][]docs.Cell, 0, 24)
	bars := make([]docs.Bar, 0, 24)
	for hr, b := range buckets {
		label := strconv.Itoa(hr) + ":00"
		profit := b.revenue - b.cost
		marginPct := 0.0
		if b.revenue != 0 {
			marginPct = profit / b.revenue * 100
		}
		rows = append(rows, []docs.Cell{
			docs.Text(label), docs.Text(strconv.Itoa(b.orders)), docs.Text(fmtAmount(b.revenue)),
			docs.Text(fmtAmount(profit)), docs.Text(fmtQty(marginPct) + "%"),
		})
		bars = append(bars, docs.Bar{Label: label, Value: b.revenue})
	}
	totalProfit := totalRevenue - totalCost
	totalMarginPct := 0.0
	if totalRevenue != 0 {
		totalMarginPct = totalProfit / totalRevenue * 100
	}

	report := h.newReport(ctx, tid, oid, "Sales by Hour", dateStr, dayStart, dayStart, true)
	report.Cards = []docs.Card{
		{Label: "Total Revenue", Value: "KES " + fmtAmount(totalRevenue)},
		{Label: "Orders", Value: strconv.Itoa(totalOrders)},
		{Label: "Peak Hour", Value: strconv.Itoa(peakHour) + ":00"},
		{Label: "Profit Margin", Value: fmtQty(totalMarginPct) + "%"},
	}
	report.Sections = []docs.Section{
		{
			Kind:  docs.SectionTable,
			Title: "Hourly Breakdown",
			Columns: []docs.Column{
				{Header: "Hour", Weight: 1}, {Header: "Orders", Weight: 1, Align: "R"},
				{Header: "Revenue", Weight: 1.4, Money: true}, {Header: "Profit", Weight: 1.4, Money: true},
				{Header: "Margin", Weight: 1, Align: "R"},
			},
			Rows: rows,
			Total: []docs.Cell{
				docs.BoldText("Total"), docs.BoldText(strconv.Itoa(totalOrders)), docs.BoldText(fmtAmount(totalRevenue)),
				docs.BoldText(fmtAmount(totalProfit)), docs.BoldText(fmtQty(totalMarginPct) + "%"),
			},
		},
		{Kind: docs.SectionChart, Title: "Revenue by Hour", ValueUnit: "KES", Bars: bars},
	}
	h.write(w, r, report, "sales-by-hour")
}

// ── Sales by Category ────────────────────────────────────────────────────────────

// SalesByCategoryDoc handles GET /{tenantID}/pos/reports/sales-by-category-document?from=&to=&outlet_id=&format=
// Mirrors ReportsHandler.SalesByCategory's grouping exactly.
func (h *ReportPDFHandler) SalesByCategoryDoc(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)
	from, to := parseReportRange(r, requestTenantLocation(r, h.db))

	orders, err := h.completedOrders(ctx, tid, oid, from, to, true)
	if err != nil {
		h.log.Error("sales-by-category-document: query failed", zap.Error(err))
		jsonError(w, "failed to generate sales by category report", http.StatusInternalServerError)
		return
	}

	type catBucket struct {
		revenue, qty float64
	}
	byCategory := make(map[string]*catBucket)
	for _, o := range orders {
		// AttributeOrderLines (see report_attribution.go) fixes the same two bugs found across
		// every line-level report: a voided line no longer contributes its pre-void gross, and
		// revenue is each line's prorated share of order.TotalAmount, not raw total_price — so
		// this exported document now agrees with the JSON endpoint and Sales-by-Staff.
		for i, al := range AttributeOrderLines(o) {
			line := o.Edges.Lines[i]
			// POSOrderLine.Category is the real, always-populated column — see the same
			// fix in reports.go SalesByCategory (line.Metadata never carries "category").
			cat := line.Category
			if cat == "" {
				cat = "Uncategorised"
			}
			if byCategory[cat] == nil {
				byCategory[cat] = &catBucket{}
			}
			byCategory[cat].revenue += al.Revenue
			byCategory[cat].qty += al.Quantity
		}
	}

	type catRow struct {
		name string
		b    *catBucket
	}
	list := make([]catRow, 0, len(byCategory))
	var totalRevenue, totalQty float64
	for name, b := range byCategory {
		list = append(list, catRow{name: name, b: b})
		totalRevenue += b.revenue
		totalQty += b.qty
	}
	sort.Slice(list, func(i, j int) bool { return list[i].b.revenue > list[j].b.revenue })

	rows := make([][]docs.Cell, 0, len(list))
	bars := make([]docs.Bar, 0, len(list))
	for _, c := range list {
		rows = append(rows, []docs.Cell{docs.Text(c.name), docs.Text(fmtQty(c.b.qty)), docs.Text(fmtAmount(c.b.revenue))})
		bars = append(bars, docs.Bar{Label: c.name, Value: c.b.revenue})
	}

	report := h.newReport(ctx, tid, oid, "Sales by Category", "", from, to, false)
	report.Cards = []docs.Card{
		{Label: "Total Revenue", Value: "KES " + fmtAmount(totalRevenue)},
		{Label: "Categories", Value: strconv.Itoa(len(list))},
		{Label: "Qty Sold", Value: fmtQty(totalQty)},
	}
	report.Sections = []docs.Section{
		{
			Kind:    docs.SectionTable,
			Title:   "Sales by Category",
			Columns: []docs.Column{{Header: "Category", Weight: 2}, {Header: "Qty Sold", Weight: 1, Align: "R"}, {Header: "Revenue", Weight: 1.4, Money: true}},
			Rows:    rows,
			Total:   []docs.Cell{docs.BoldText("Total"), docs.BoldText(fmtQty(totalQty)), docs.BoldText(fmtAmount(totalRevenue))},
		},
		{Kind: docs.SectionChart, Title: "Revenue by Category", ValueUnit: "KES", Bars: bars},
	}
	h.write(w, r, report, "sales-by-category")
}

// ── Product Mix ───────────────────────────────────────────────────────────────────

// ProductMixDoc handles GET /{tenantID}/pos/reports/product-mix-document?from=&to=&outlet_id=
// &categories=&stations=&format=
// Mirrors ReportsHandler.ProductMix's full breakdown — not just the flat top-items table — so the
// exported document separates every sold item under its resolved category and its resolved KDS
// station (same per-outlet resolution reports_extended.go's ProductMix/computeKDSStationBreakdown
// use), in addition to the overall ranking. ?categories=/?stations= are comma-separated lists that
// scope the document to the same chip-filter selections the Product Mix tab exposes on screen —
// when supplied, only the matching categories/stations (and the lines under them) are included;
// omitted → every category/station a line resolved to.
func (h *ReportPDFHandler) ProductMixDoc(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)
	from, to := parseReportRange(r, requestTenantLocation(r, h.db))

	catFilter := parseCommaSet(r.URL.Query().Get("categories"))
	stationFilter := parseCommaSet(r.URL.Query().Get("stations"))

	orderPreds := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.StatusEQ("completed"),
		effectiveDateGTE(from),
		effectiveDateLTE(to),
	}
	if oid != nil {
		orderPreds = append(orderPreds, posorder.OutletID(*oid))
	}
	// Orders-with-lines (not lines-with-order) so AttributeOrderLines can prorate each order's
	// net total_amount across its own lines — matches reports_extended.go's ProductMix.
	mixOrders, err := h.db.POSOrder.Query().
		Where(orderPreds...).
		WithLines().
		All(ctx)
	if err != nil {
		h.log.Error("product-mix-document: query failed", zap.Error(err))
		jsonError(w, "failed to generate product mix report", http.StatusInternalServerError)
		return
	}

	type mixBucket struct {
		qty, revenue float64
	}
	accumulate := func(m map[string]*mixBucket, key string, qty, revenue float64) {
		b := m[key]
		if b == nil {
			b = &mixBucket{}
			m[key] = b
		}
		b.qty += qty
		b.revenue += revenue
	}

	byItem := make(map[string]*mixBucket)
	byCategory := make(map[string]*mixBucket)
	byStation := make(map[string]*mixBucket)
	itemsByCategory := make(map[string]map[string]*mixBucket)
	itemsByStation := make(map[string]map[string]*mixBucket)

	// KDS station lookup, same per-outlet cache pattern as reports_extended.go's ProductMix — a
	// multi-outlet report can mix outlets with different station configs.
	stationsByOutlet := make(map[uuid.UUID][]*ent.KDSStation)
	stationByID := make(map[uuid.UUID]*ent.KDSStation)
	resolveStation := func(o *ent.POSOrder, l *ent.POSOrderLine) string {
		stations, ok := stationsByOutlet[o.OutletID]
		if !ok {
			stations, _ = h.db.KDSStation.Query().
				Where(entkdsstation.TenantID(tid), entkdsstation.OutletID(o.OutletID), entkdsstation.IsActive(true)).
				All(ctx)
			stationsByOutlet[o.OutletID] = stations
			for _, st := range stations {
				stationByID[st.ID] = st
			}
		}
		stationID := l.KdsStationID
		if stationID == nil {
			stationID = ordersmod.ResolveStationForLineOrFallback(l.Name, l.Category, nil, stations)
		}
		if stationID == nil {
			return ""
		}
		if st := stationByID[*stationID]; st != nil {
			return st.Name
		}
		return ""
	}

	var totalRevenue, totalQty float64
	for _, o := range mixOrders {
		// AttributeOrderLines (see report_attribution.go) fixes the same two bugs found across
		// every line-level report: a voided line no longer contributes its pre-void gross, and
		// revenue is each line's prorated share of order.TotalAmount, not raw total_price — so
		// this exported document now agrees with the JSON endpoint and Sales-by-Staff.
		for i, al := range AttributeOrderLines(o) {
			l := o.Edges.Lines[i]
			if al.Quantity <= 0 && al.Revenue <= 0 {
				continue // fully voided — nothing active to attribute
			}
			category := l.Category
			if category == "" {
				category = "Uncategorised"
			}
			station := resolveStation(o, l)
			if station == "" {
				station = "Unassigned"
			}
			if len(catFilter) > 0 && !catFilter[category] {
				continue
			}
			if len(stationFilter) > 0 && !stationFilter[station] {
				continue
			}

			accumulate(byItem, l.Name, al.Quantity, al.Revenue)
			accumulate(byCategory, category, al.Quantity, al.Revenue)
			accumulate(byStation, station, al.Quantity, al.Revenue)
			if itemsByCategory[category] == nil {
				itemsByCategory[category] = make(map[string]*mixBucket)
			}
			accumulate(itemsByCategory[category], l.Name, al.Quantity, al.Revenue)
			if itemsByStation[station] == nil {
				itemsByStation[station] = make(map[string]*mixBucket)
			}
			accumulate(itemsByStation[station], l.Name, al.Quantity, al.Revenue)
			totalRevenue += al.Revenue
			totalQty += al.Quantity
		}
	}

	type mixRow struct {
		name string
		b    *mixBucket
	}
	sortedRows := func(m map[string]*mixBucket) []mixRow {
		out := make([]mixRow, 0, len(m))
		for name, b := range m {
			out = append(out, mixRow{name: name, b: b})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].b.revenue > out[j].b.revenue })
		return out
	}
	toTableRows := func(rows []mixRow) [][]docs.Cell {
		out := make([][]docs.Cell, 0, len(rows))
		for _, m := range rows {
			out = append(out, []docs.Cell{docs.Text(m.name), docs.Text(fmtQty(m.b.qty)), docs.Text(fmtAmount(m.b.revenue))})
		}
		return out
	}
	toBars := func(rows []mixRow, limit int) []docs.Bar {
		if limit > 0 && len(rows) > limit {
			rows = rows[:limit]
		}
		out := make([]docs.Bar, 0, len(rows))
		for _, m := range rows {
			out = append(out, docs.Bar{Label: m.name, Value: m.b.revenue})
		}
		return out
	}

	list := sortedRows(byItem)
	itemCols := []docs.Column{{Header: "Product", Weight: 2.2}, {Header: "Qty", Weight: 1, Align: "R"}, {Header: "Revenue", Weight: 1.4, Money: true}}

	report := h.newReport(ctx, tid, oid, "Product Mix", "", from, to, false)
	report.Cards = []docs.Card{
		{Label: "Total Revenue", Value: "KES " + fmtAmount(totalRevenue)},
		{Label: "Products Sold", Value: strconv.Itoa(len(list))},
		{Label: "Qty Sold", Value: fmtQty(totalQty)},
	}
	report.Sections = []docs.Section{
		{
			Kind:    docs.SectionTable,
			Title:   "Top Products",
			Columns: itemCols,
			Rows:    toTableRows(list),
			Total:   []docs.Cell{docs.BoldText("Total"), docs.BoldText(fmtQty(totalQty)), docs.BoldText(fmtAmount(totalRevenue))},
		},
		{Kind: docs.SectionChart, Title: "Top Products by Revenue", ValueUnit: "KES", Bars: toBars(list, 20)},
	}

	// Category breakdown — one chart summarizing every category's revenue, then a table of every
	// item under each category (highest-revenue category first).
	categoryTotals := sortedRows(byCategory)
	if len(categoryTotals) > 0 {
		report.Sections = append(report.Sections,
			docs.Section{Kind: docs.SectionChart, Title: "Revenue by Category", ValueUnit: "KES", Bars: toBars(categoryTotals, 0)})
		for _, cat := range categoryTotals {
			items := sortedRows(itemsByCategory[cat.name])
			report.Sections = append(report.Sections, docs.Section{
				Kind:    docs.SectionTable,
				Title:   "Category: " + cat.name,
				Columns: itemCols,
				Rows:    toTableRows(items),
				Total:   []docs.Cell{docs.BoldText("Total"), docs.BoldText(fmtQty(cat.b.qty)), docs.BoldText(fmtAmount(cat.b.revenue))},
			})
		}
	}

	// Station breakdown — same shape, grouped by resolved KDS station instead of category.
	stationTotals := sortedRows(byStation)
	if len(stationTotals) > 0 {
		report.Sections = append(report.Sections,
			docs.Section{Kind: docs.SectionChart, Title: "Revenue by KDS Station", ValueUnit: "KES", Bars: toBars(stationTotals, 0)})
		for _, st := range stationTotals {
			items := sortedRows(itemsByStation[st.name])
			report.Sections = append(report.Sections, docs.Section{
				Kind:    docs.SectionTable,
				Title:   "KDS Station: " + st.name,
				Columns: itemCols,
				Rows:    toTableRows(items),
				Total:   []docs.Cell{docs.BoldText("Total"), docs.BoldText(fmtQty(st.b.qty)), docs.BoldText(fmtAmount(st.b.revenue))},
			})
		}
	}

	h.write(w, r, report, "product-mix")
}

// parseCommaSet splits a comma-separated query param into a lookup set, trimming whitespace and
// dropping empties. Returns an empty (non-nil) map when s is blank — callers treat an empty set as
// "no filter" (len(...) == 0), not "match nothing".
func parseCommaSet(s string) map[string]bool {
	out := make(map[string]bool)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out[part] = true
		}
	}
	return out
}

// ── Voids ─────────────────────────────────────────────────────────────────────────

// VoidSummaryDoc handles GET /{tenantID}/pos/reports/void-summary-document?from=&to=&outlet_id=&format=
// Mirrors ReportsHandler.VoidSummary's per-staff grouping exactly.
func (h *ReportPDFHandler) VoidSummaryDoc(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)
	from, to := parseReportRange(r, requestTenantLocation(r, h.db))

	orders, err := h.db.POSOrder.Query().Where(voidedPreds(tid, oid, from, to)...).All(ctx)
	if err != nil {
		h.log.Error("void-summary-document: query failed", zap.Error(err))
		jsonError(w, "failed to generate void summary report", http.StatusInternalServerError)
		return
	}

	type voidBucket struct {
		count   int
		amount  float64
		reasons map[string]int
	}
	unattributed := uuid.Nil
	buckets := make(map[uuid.UUID]*voidBucket)
	for _, o := range orders {
		staffID := unattributed
		if o.VoidedBy != nil {
			staffID = *o.VoidedBy
		}
		if buckets[staffID] == nil {
			buckets[staffID] = &voidBucket{reasons: make(map[string]int)}
		}
		buckets[staffID].count++
		buckets[staffID].amount += o.TotalAmount
		reason := "unspecified"
		if o.VoidedReason != nil && *o.VoidedReason != "" {
			reason = *o.VoidedReason
		}
		buckets[staffID].reasons[reason]++
	}

	ids := make([]uuid.UUID, 0, len(buckets))
	for id := range buckets {
		if id != unattributed {
			ids = append(ids, id)
		}
	}
	names := resolveStaffNames(ctx, h.db, h.log, tid, ids)

	type voidRow struct {
		name string
		b    *voidBucket
	}
	list := make([]voidRow, 0, len(buckets))
	var totalVoids int
	var totalAmount float64
	for id, b := range buckets {
		name := "Unattributed"
		if id != unattributed {
			if n := names[id]; n != "" {
				name = n
			} else {
				name = "Unknown"
			}
		}
		list = append(list, voidRow{name: name, b: b})
		totalVoids += b.count
		totalAmount += b.amount
	}
	sort.Slice(list, func(i, j int) bool { return list[i].b.count > list[j].b.count })

	rows := make([][]docs.Cell, 0, len(list))
	bars := make([]docs.Bar, 0, len(list))
	for _, v := range list {
		reasonList := make([]string, 0, len(v.b.reasons))
		for reason := range v.b.reasons {
			reasonList = append(reasonList, reason)
		}
		sort.Strings(reasonList)
		rows = append(rows, []docs.Cell{
			docs.Text(v.name), docs.Text(strconv.Itoa(v.b.count)), docs.Text(fmtAmount(v.b.amount)), docs.Text(strings.Join(reasonList, ", ")),
		})
		bars = append(bars, docs.Bar{Label: v.name, Value: float64(v.b.count)})
	}

	report := h.newReport(ctx, tid, oid, "Voids", "", from, to, false)
	report.Cards = []docs.Card{
		{Label: "Total Voids", Value: strconv.Itoa(totalVoids)},
		{Label: "Total Voided Amount", Value: "KES " + fmtAmount(totalAmount)},
		{Label: "Staff Involved", Value: strconv.Itoa(len(list))},
	}
	report.Sections = []docs.Section{
		{
			Kind:  docs.SectionTable,
			Title: "Voids by Staff",
			Columns: []docs.Column{
				{Header: "Staff", Weight: 1.8}, {Header: "Voids", Weight: 1, Align: "R"},
				{Header: "Amount", Weight: 1.4, Money: true}, {Header: "Reasons", Weight: 2.2},
			},
			Rows:  rows,
			Total: []docs.Cell{docs.BoldText("Total"), docs.BoldText(strconv.Itoa(totalVoids)), docs.BoldText(fmtAmount(totalAmount)), docs.Text("")},
		},
		{Kind: docs.SectionChart, Title: "Void Count by Staff", Bars: bars},
	}
	h.write(w, r, report, "void-summary")
}
