package handlers

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/modules/docs"
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

	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		dateStr = time.Now().UTC().Format("2006-01-02")
	}
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		jsonError(w, "invalid date, use YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	dayStart := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)

	preds := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.StatusEQ("completed"),
		posorder.CreatedAtGTE(dayStart),
		posorder.CreatedAtLT(dayEnd),
	}
	if oid != nil {
		preds = append(preds, posorder.OutletID(*oid))
	}
	orders, err := h.db.POSOrder.Query().Where(preds...).All(ctx)
	if err != nil {
		h.log.Error("sales-by-hour-document: query failed", zap.Error(err))
		jsonError(w, "failed to generate sales by hour report", http.StatusInternalServerError)
		return
	}

	type hourBucket struct {
		orders  int
		revenue float64
	}
	buckets := make([]hourBucket, 24)
	var totalRevenue float64
	var totalOrders int
	for _, o := range orders {
		hr := o.CreatedAt.UTC().Hour()
		buckets[hr].orders++
		buckets[hr].revenue += o.TotalAmount
		totalOrders++
		totalRevenue += o.TotalAmount
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
		rows = append(rows, []docs.Cell{docs.Text(label), docs.Text(strconv.Itoa(b.orders)), docs.Text(fmtAmount(b.revenue))})
		bars = append(bars, docs.Bar{Label: label, Value: b.revenue})
	}

	report := h.newReport(ctx, tid, oid, "Sales by Hour", dateStr, dayStart, dayStart, true)
	report.Cards = []docs.Card{
		{Label: "Total Revenue", Value: "KES " + fmtAmount(totalRevenue)},
		{Label: "Orders", Value: strconv.Itoa(totalOrders)},
		{Label: "Peak Hour", Value: strconv.Itoa(peakHour) + ":00"},
	}
	report.Sections = []docs.Section{
		{
			Kind:    docs.SectionTable,
			Title:   "Hourly Breakdown",
			Columns: []docs.Column{{Header: "Hour", Weight: 1}, {Header: "Orders", Weight: 1, Align: "R"}, {Header: "Revenue", Weight: 1.4, Money: true}},
			Rows:    rows,
			Total:   []docs.Cell{docs.BoldText("Total"), docs.BoldText(strconv.Itoa(totalOrders)), docs.BoldText(fmtAmount(totalRevenue))},
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
	from, to := parseReportRange(r)

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
		for _, line := range o.Edges.Lines {
			cat, _ := line.Metadata["category"].(string)
			if cat == "" {
				cat = "Uncategorised"
			}
			if byCategory[cat] == nil {
				byCategory[cat] = &catBucket{}
			}
			byCategory[cat].revenue += line.TotalPrice
			byCategory[cat].qty += line.Quantity
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

// ProductMixDoc handles GET /{tenantID}/pos/reports/product-mix-document?from=&to=&outlet_id=&format=
// Mirrors ReportsHandler.ProductMix's byItem grouping (the table the UI actually renders).
func (h *ReportPDFHandler) ProductMixDoc(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)
	from, to := parseReportRange(r)

	orderPreds := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.StatusEQ("completed"),
		posorder.CreatedAtGTE(from),
		posorder.CreatedAtLTE(to),
	}
	if oid != nil {
		orderPreds = append(orderPreds, posorder.OutletID(*oid))
	}
	lines, err := h.db.POSOrderLine.Query().
		Where(posorderline.HasOrderWith(orderPreds...)).
		All(ctx)
	if err != nil {
		h.log.Error("product-mix-document: query failed", zap.Error(err))
		jsonError(w, "failed to generate product mix report", http.StatusInternalServerError)
		return
	}

	type mixBucket struct {
		qty, revenue float64
	}
	byItem := make(map[string]*mixBucket)
	for _, l := range lines {
		if byItem[l.Name] == nil {
			byItem[l.Name] = &mixBucket{}
		}
		byItem[l.Name].qty += l.Quantity
		byItem[l.Name].revenue += l.TotalPrice
	}

	type mixRow struct {
		name string
		b    *mixBucket
	}
	list := make([]mixRow, 0, len(byItem))
	var totalRevenue, totalQty float64
	for name, b := range byItem {
		list = append(list, mixRow{name: name, b: b})
		totalRevenue += b.revenue
		totalQty += b.qty
	}
	sort.Slice(list, func(i, j int) bool { return list[i].b.revenue > list[j].b.revenue })

	rows := make([][]docs.Cell, 0, len(list))
	for _, m := range list {
		rows = append(rows, []docs.Cell{docs.Text(m.name), docs.Text(fmtQty(m.b.qty)), docs.Text(fmtAmount(m.b.revenue))})
	}
	// Chart capped to the top 12 products so bar labels stay legible; the full ranking is in the table.
	chartList := list
	if len(chartList) > 12 {
		chartList = chartList[:12]
	}
	bars := make([]docs.Bar, 0, len(chartList))
	for _, m := range chartList {
		bars = append(bars, docs.Bar{Label: m.name, Value: m.b.revenue})
	}

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
			Columns: []docs.Column{{Header: "Product", Weight: 2.2}, {Header: "Qty", Weight: 1, Align: "R"}, {Header: "Revenue", Weight: 1.4, Money: true}},
			Rows:    rows,
			Total:   []docs.Cell{docs.BoldText("Total"), docs.BoldText(fmtQty(totalQty)), docs.BoldText(fmtAmount(totalRevenue))},
		},
		{Kind: docs.SectionChart, Title: "Top Products by Revenue", ValueUnit: "KES", Bars: bars},
	}
	h.write(w, r, report, "product-mix")
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
	from, to := parseReportRange(r)

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
	names := h.resolveStaffNames(ctx, tid, ids)

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
