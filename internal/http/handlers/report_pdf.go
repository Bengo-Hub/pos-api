package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/posrefund"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
	enttender "github.com/bengobox/pos-service/internal/ent/tender"
	entuser "github.com/bengobox/pos-service/internal/ent/user"
	"github.com/bengobox/pos-service/internal/modules/docs"
)

// ReportPDFHandler renders professional, tenant-branded POS report documents (PDF/CSV) via the
// docs engine. It wires the SAME ent queries the JSON report endpoints use (reports.go,
// reports_extended.go, reports_profitability.go, closings.go) into docs.Report so the printed
// figures match the JSON endpoints exactly.
type ReportPDFHandler struct {
	log     *zap.Logger
	db      *ent.Client
	cache   *sharedcache.Aside // tenant branding cache (auth-api source)
	authURL string
}

// NewReportPDFHandler creates a new ReportPDFHandler.
func NewReportPDFHandler(log *zap.Logger, db *ent.Client, cache *sharedcache.Aside, authURL string) *ReportPDFHandler {
	return &ReportPDFHandler{log: log, db: db, cache: cache, authURL: authURL}
}

// ── shared helpers ────────────────────────────────────────────────────────────

// branding fetches tenant branding (logo/name/primary-color) from the shared cache. This is an
// exact copy of ReceiptHandler.branding — best-effort, returns a zero-value brand on any failure.
func (h *ReportPDFHandler) branding(ctx context.Context, tenantID uuid.UUID) receiptBrand {
	var b receiptBrand
	if h.cache == nil || h.authURL == "" {
		return b
	}
	t, err := h.db.Tenant.Query().Where(enttenant.ID(tenantID)).Only(ctx)
	if err != nil {
		return b
	}
	b.CompanyName = t.Name
	td, err := sharedcache.GetTenantDetails(ctx, h.cache, h.authURL, t.Slug, sharedcache.DefaultTenantTTL)
	if err != nil {
		return b
	}
	tb := sharedcache.GetTenantBranding(td)
	if tb.Name != "" {
		b.CompanyName = tb.Name
	}
	b.LogoURL = tb.LogoURL
	b.PrimaryColor = tb.PrimaryColor
	return b
}

// outletScope resolves the outlet the report is scoped to: an explicit ?outlet_id= wins, otherwise
// the outlet in the request context (httpware). Returns nil when the report spans all outlets.
func (h *ReportPDFHandler) outletScope(r *http.Request) *uuid.UUID {
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

// outletInfo resolves the display name + address line for an outlet in scope (empty when none).
func (h *ReportPDFHandler) outletInfo(ctx context.Context, tid uuid.UUID, oid *uuid.UUID) (name, addr string) {
	if oid == nil {
		return "", ""
	}
	o, err := h.db.Outlet.Query().Where(entoutlet.ID(*oid), entoutlet.TenantID(tid)).Only(ctx)
	if err != nil {
		return "", ""
	}
	name = o.Name
	if a := o.AddressJSON; a != nil {
		if street, ok := a["street"].(string); ok && street != "" {
			addr = street
		} else if city, ok := a["city"].(string); ok {
			addr = city
		}
	}
	return name, addr
}

// newReport builds a docs.Report pre-populated with tenant/outlet branding and the report meta.
func (h *ReportPDFHandler) newReport(ctx context.Context, tid uuid.UUID, oid *uuid.UUID, title, subtitle string, from, to time.Time, landscape bool) *docs.Report {
	brand := h.branding(ctx, tid)
	logo, logoType := fetchReceiptLogo(brand.LogoURL)
	outletName, outletAddr := h.outletInfo(ctx, tid, oid)
	return &docs.Report{
		Title:        title,
		Subtitle:     subtitle,
		TenantName:   brand.CompanyName,
		OutletName:   outletName,
		Address:      outletAddr,
		PrimaryColor: brand.PrimaryColor,
		LogoPNG:      logo,
		LogoType:     logoType,
		PeriodFrom:   from,
		PeriodTo:     to,
		GeneratedAt:  time.Now().UTC(),
		Currency:     "KES",
		Landscape:    landscape,
	}
}

// write generates the document in the requested format (?format=pdf|csv, default pdf) and streams
// it inline with a dated filename.
func (h *ReportPDFHandler) write(w http.ResponseWriter, r *http.Request, report *docs.Report, baseName string) {
	format, ferr := docs.FormatFromString(r.URL.Query().Get("format"))
	if ferr != nil {
		jsonError(w, ferr.Error(), http.StatusBadRequest)
		return
	}
	svc := &docs.DocumentService{}
	b, mime, err := svc.Generate(report, format)
	if err != nil {
		h.log.Error("generate report document", zap.String("report", baseName), zap.Error(err))
		jsonError(w, "failed to generate report", http.StatusInternalServerError)
		return
	}
	ext := "pdf"
	if format == docs.FormatCSV {
		ext = "csv"
	}
	filename := fmt.Sprintf("%s-%s.%s", baseName, time.Now().UTC().Format("2006-01-02"), ext)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))
	_, _ = w.Write(b)
}

// completedOrders queries completed orders in [from,to] for the tenant, honoring outlet scope.
func (h *ReportPDFHandler) completedOrders(ctx context.Context, tid uuid.UUID, oid *uuid.UUID, from, to time.Time, withLines bool) ([]*ent.POSOrder, error) {
	preds := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.StatusEQ("completed"),
		posorder.CreatedAtGTE(from),
		posorder.CreatedAtLTE(to),
	}
	if oid != nil {
		preds = append(preds, posorder.OutletID(*oid))
	}
	q := h.db.POSOrder.Query().Where(preds...)
	if withLines {
		q = q.WithLines()
	}
	return q.All(ctx)
}

// resolveStaffNames maps POS user_ids to human staff names (mirrors ReportsHandler.resolveStaffNames).
func (h *ReportPDFHandler) resolveStaffNames(ctx context.Context, tid uuid.UUID, ids []uuid.UUID) map[uuid.UUID]string {
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	users, err := h.db.User.Query().
		Where(entuser.TenantID(tid), entuser.Or(entuser.IDIn(ids...), entuser.AuthServiceUserIDIn(ids...))).
		All(ctx)
	if err != nil {
		h.log.Warn("report: resolve staff names failed", zap.Error(err))
		return out
	}
	for _, u := range users {
		name := strings.TrimSpace(u.FullName)
		if name == "" {
			name = strings.TrimSpace(u.Email)
		}
		if name == "" {
			continue
		}
		out[u.ID] = name
		out[u.AuthServiceUserID] = name
	}
	return out
}

// tenderBreakdown groups completed payments for the given order ids by tender, returning ordered
// AccuPOS-style rows ("NN - NAME" → amount) and the grand tender total.
func (h *ReportPDFHandler) tenderBreakdown(ctx context.Context, tid uuid.UUID, orderIDs []uuid.UUID) (rows []docs.KV, total float64) {
	if len(orderIDs) == 0 {
		return nil, 0
	}
	payments, err := h.db.POSPayment.Query().
		Where(pospayment.StatusEQ("completed"), pospayment.OrderIDIn(orderIDs...)).
		All(ctx)
	if err != nil {
		h.log.Warn("report: tender breakdown query failed", zap.Error(err))
		return nil, 0
	}
	byTender := make(map[uuid.UUID]float64)
	for _, p := range payments {
		byTender[p.TenderID] += p.Amount
		total += p.Amount
	}
	if len(byTender) == 0 {
		return nil, total
	}
	// Resolve tender names.
	ids := make([]uuid.UUID, 0, len(byTender))
	for id := range byTender {
		ids = append(ids, id)
	}
	names := make(map[uuid.UUID]string)
	tenders, terr := h.db.Tender.Query().Where(enttender.IDIn(ids...)).All(ctx)
	if terr == nil {
		for _, t := range tenders {
			names[t.ID] = t.Name
		}
	}
	type tRow struct {
		name   string
		amount float64
	}
	tmp := make([]tRow, 0, len(byTender))
	for id, amt := range byTender {
		n := names[id]
		if n == "" {
			n = "Unknown"
		}
		tmp = append(tmp, tRow{name: n, amount: amt})
	}
	// Stable, alphabetical order so the AccuPOS "NN - NAME" codes are deterministic.
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].name < tmp[j].name })
	for i, t := range tmp {
		rows = append(rows, docs.KV{Label: fmt.Sprintf("%02d - %s", i+1, strings.ToUpper(t.name)), Value: fmtAmount(t.amount)})
	}
	return rows, total
}

// ── amount/quantity formatting (display strings for docs cells) ─────────────────

// fmtAmount renders a money value as "1,234.56" (thousands separators, 2 decimals). The docs engine
// right-aligns Money columns and carries the currency code separately, matching the render tests.
func fmtAmount(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	dec := s[len(s)-3:]
	intPart := s[:len(s)-3]
	neg := strings.HasPrefix(intPart, "-")
	if neg {
		intPart = intPart[1:]
	}
	out := ""
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			out += ","
		}
		out += string(c)
	}
	if neg {
		out = "-" + out
	}
	return out + dec
}

// fmtQty renders a quantity trimming trailing zeros ("2", "2.5", "12").
func fmtQty(v float64) string {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// parseReportRange parses ?from/?to (RFC3339 or YYYY-MM-DD), defaulting to today 00:00 → now (UTC).
func parseReportRange(r *http.Request) (from, to time.Time) {
	now := time.Now().UTC()
	from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	to = now
	if s := r.URL.Query().Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			from = t
		} else if t, err := time.Parse("2006-01-02", s); err == nil {
			from = t
		}
	}
	if s := r.URL.Query().Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			to = t
		} else if t, err := time.Parse("2006-01-02", s); err == nil {
			to = t.Add(24*time.Hour - time.Second)
		}
	}
	return from, to
}

// ── 1. Reset Summary Report ─────────────────────────────────────────────────────

// ResetSummary handles GET /{tenantID}/pos/reports/reset-summary — the AccuPOS "Reset Summary
// Report" (Z/reset): tender summary, item types, voids/returns, taxable/tax totals and servers.
func (h *ReportPDFHandler) ResetSummary(w http.ResponseWriter, r *http.Request) {
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
		h.log.Error("reset-summary: orders query failed", zap.Error(err))
		jsonError(w, "failed to generate reset summary", http.StatusInternalServerError)
		return
	}
	orderIDs := make([]uuid.UUID, 0, len(orders))
	for _, o := range orders {
		orderIDs = append(orderIDs, o.ID)
	}

	// (a) Tender Summary.
	tenderRows, tenderTotal := h.tenderBreakdown(ctx, tid, orderIDs)
	tenderRows = append(tenderRows, docs.KV{Label: "Tendering Total", Value: fmtAmount(tenderTotal), Bold: true, Rule: true})

	// (b) Item Types (grouped by catalog category) + totals (taxable/nontaxable/tax) + items sold.
	type catAgg struct {
		qty, amount float64
	}
	byCat := make(map[string]*catAgg)
	var itemsSold, totalTaxable, totalNontaxable, totalTax, grandTotal float64
	for _, o := range orders {
		grandTotal += o.TotalAmount
		for _, l := range o.Edges.Lines {
			// POSOrderLine.Category is the real, always-populated column — see the same
			// fix in reports.go SalesByCategory (l.Metadata never carries "category").
			cat := l.Category
			if cat == "" {
				cat = "Uncategorised"
			}
			if byCat[cat] == nil {
				byCat[cat] = &catAgg{}
			}
			byCat[cat].qty += l.Quantity
			byCat[cat].amount += l.TotalPrice
			itemsSold += l.Quantity

			// Taxable/tax split mirrors TaxReport: rate>0 ⇒ taxable, else nontaxable.
			lineTotal := l.UnitPrice * l.Quantity
			rate := 0.0
			if l.TaxRate != nil {
				rate = *l.TaxRate
			}
			if rate > 0 {
				totalTaxable += lineTotal
				totalTax += lineTotal * (rate / 100)
			} else {
				totalNontaxable += lineTotal
			}
		}
	}
	catNames := make([]string, 0, len(byCat))
	for name := range byCat {
		catNames = append(catNames, name)
	}
	sort.Slice(catNames, func(i, j int) bool { return byCat[catNames[i]].amount > byCat[catNames[j]].amount })
	itemTypeRows := make([][]docs.Cell, 0, len(catNames))
	for _, name := range catNames {
		a := byCat[name]
		itemTypeRows = append(itemTypeRows, []docs.Cell{docs.Text(strings.ToUpper(name)), docs.Text(fmtQty(a.qty)), docs.Text(fmtAmount(a.amount))})
	}

	// (c) Voids / Returns.
	voided, verr := h.db.POSOrder.Query().
		Where(voidedPreds(tid, oid, from, to)...).All(ctx)
	if verr != nil {
		h.log.Warn("reset-summary: voided query failed", zap.Error(verr))
	}
	var voidQty, voidAmount float64
	for _, o := range voided {
		voidAmount += o.TotalAmount
		voidQty++
	}
	returns, rerr := h.db.POSReturn.Query().
		Where(posreturn.TenantID(tid), posreturn.CreatedAtGTE(from), posreturn.CreatedAtLT(to)).
		All(ctx)
	if rerr != nil {
		h.log.Warn("reset-summary: returns query failed", zap.Error(rerr))
	}
	var returnCount int
	var returnAmount float64
	for _, ret := range returns {
		returnCount++
		returnAmount += ret.RefundAmount
	}
	voidReturnPairs := []docs.KV{
		{Label: "Voided Orders (qty)", Value: fmtQty(voidQty)},
		{Label: "Voided Amount", Value: fmtAmount(voidAmount)},
		{Label: "Returns (count)", Value: strconv.Itoa(returnCount)},
		{Label: "Returns Refunded", Value: fmtAmount(returnAmount), Rule: true},
	}

	// (d) Totals.
	totalsPairs := []docs.KV{
		{Label: "Taxable", Value: fmtAmount(totalTaxable)},
		{Label: "Nontaxable", Value: fmtAmount(totalNontaxable)},
		{Label: "Tax", Value: fmtAmount(totalTax)},
		{Label: "Total", Value: fmtAmount(grandTotal), Bold: true, Rule: true},
	}

	// (e) Servers.
	byUser := make(map[uuid.UUID]float64)
	for _, o := range orders {
		byUser[o.UserID] += o.TotalAmount
	}
	uids := make([]uuid.UUID, 0, len(byUser))
	for id := range byUser {
		uids = append(uids, id)
	}
	names := h.resolveStaffNames(ctx, tid, uids)
	serverPairs := make([]docs.KV, 0, len(byUser))
	for id, net := range byUser {
		n := names[id]
		if n == "" {
			n = "Unknown"
		}
		serverPairs = append(serverPairs, docs.KV{Label: n, Value: fmtAmount(net)})
	}
	sort.Slice(serverPairs, func(i, j int) bool { return serverPairs[i].Label < serverPairs[j].Label })

	report := h.newReport(ctx, tid, oid, "Reset Summary Report", "Z / Reset", from, to, false)
	report.Cards = []docs.Card{
		{Label: "Tendering Total", Value: "KES " + fmtAmount(tenderTotal), Sub: fmt.Sprintf("%d order(s)", len(orders))},
		{Label: "Items Sold", Value: fmtQty(itemsSold), Sub: fmt.Sprintf("%d item type(s)", len(byCat))},
		{Label: "Voids", Value: fmtQty(voidQty), Sub: "KES " + fmtAmount(voidAmount)},
	}
	report.Sections = []docs.Section{
		{Kind: docs.SectionKeyValue, Title: "Tender Summary", Pairs: tenderRows},
		{
			Kind:    docs.SectionTable,
			Title:   "Item Types",
			Columns: []docs.Column{{Header: "Item Type", Weight: 2}, {Header: "Quantity", Weight: 1, Align: "R"}, {Header: "Amount", Weight: 1, Money: true}},
			Rows:    itemTypeRows,
			Total:   []docs.Cell{docs.BoldText("Report Total"), docs.BoldText(fmtQty(itemsSold)), docs.BoldText(fmtAmount(grandTotal))},
		},
		{Kind: docs.SectionKeyValue, Title: "Voids / Returns", Pairs: voidReturnPairs},
		{Kind: docs.SectionKeyValue, Title: "Totals", Pairs: totalsPairs},
		{Kind: docs.SectionKeyValue, Title: "Servers", Pairs: serverPairs},
	}
	h.write(w, r, report, "reset-summary")
}

// voidedPreds builds the predicate set for voided orders in range (outlet-scoped) — mirrors VoidSummary.
func voidedPreds(tid uuid.UUID, oid *uuid.UUID, from, to time.Time) []predicate.POSOrder {
	preds := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.StatusEQ("voided"),
		posorder.CreatedAtGTE(from),
		posorder.CreatedAtLTE(to),
	}
	if oid != nil {
		preds = append(preds, posorder.OutletID(*oid))
	}
	return preds
}

// ── 2. Sales by Item Type ───────────────────────────────────────────────────────

// SalesByItemType handles GET /{tenantID}/pos/reports/sales-by-item-type — one table per item
// type/category (item, description, qty, amount) with per-group totals, a report total and a chart.
func (h *ReportPDFHandler) SalesByItemType(w http.ResponseWriter, r *http.Request) {
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
		h.log.Error("sales-by-item-type: orders query failed", zap.Error(err))
		jsonError(w, "failed to generate sales by item type", http.StatusInternalServerError)
		return
	}

	type itemAgg struct {
		sku, name   string
		qty, amount float64
	}
	// category → sku → agg
	byCat := make(map[string]map[string]*itemAgg)
	catTotals := make(map[string]float64)
	var grandTotal float64
	for _, o := range orders {
		for _, l := range o.Edges.Lines {
			// POSOrderLine.Category is the real, always-populated column — see the same
			// fix in reports.go SalesByCategory (l.Metadata never carries "category").
			cat := l.Category
			if cat == "" {
				cat = "Uncategorised"
			}
			if byCat[cat] == nil {
				byCat[cat] = make(map[string]*itemAgg)
			}
			key := l.Sku
			if key == "" {
				key = l.Name
			}
			if byCat[cat][key] == nil {
				byCat[cat][key] = &itemAgg{sku: l.Sku, name: l.Name}
			}
			byCat[cat][key].qty += l.Quantity
			byCat[cat][key].amount += l.TotalPrice
			catTotals[cat] += l.TotalPrice
			grandTotal += l.TotalPrice
		}
	}

	catNames := make([]string, 0, len(byCat))
	for name := range byCat {
		catNames = append(catNames, name)
	}
	sort.Slice(catNames, func(i, j int) bool { return catTotals[catNames[i]] > catTotals[catNames[j]] })

	cols := []docs.Column{
		{Header: "Item", Weight: 1.2},
		{Header: "Description", Weight: 2.2},
		{Header: "Quantity", Weight: 1, Align: "R"},
		{Header: "Amount", Weight: 1.2, Money: true},
	}
	sections := make([]docs.Section, 0, len(catNames)+2)
	bars := make([]docs.Bar, 0, len(catNames))
	for _, cat := range catNames {
		items := byCat[cat]
		rows := make([][]docs.Cell, 0, len(items))
		var catQty float64
		itemsList := make([]*itemAgg, 0, len(items))
		for _, a := range items {
			itemsList = append(itemsList, a)
		}
		sort.Slice(itemsList, func(i, j int) bool { return itemsList[i].amount > itemsList[j].amount })
		for _, a := range itemsList {
			catQty += a.qty
			rows = append(rows, []docs.Cell{docs.Text(a.sku), docs.Text(a.name), docs.Text(fmtQty(a.qty)), docs.Text(fmtAmount(a.amount))})
		}
		sections = append(sections, docs.Section{
			Kind:    docs.SectionTable,
			Title:   strings.ToUpper(cat),
			Columns: cols,
			Rows:    rows,
			Total:   []docs.Cell{docs.BoldText("Subtotal"), docs.Text(""), docs.BoldText(fmtQty(catQty)), docs.BoldText(fmtAmount(catTotals[cat]))},
		})
		bars = append(bars, docs.Bar{Label: cat, Value: catTotals[cat]})
	}
	// Final report total.
	sections = append(sections, docs.Section{
		Kind:  docs.SectionKeyValue,
		Title: "Report Total",
		Pairs: []docs.KV{{Label: "All Item Types", Value: fmtAmount(grandTotal), Bold: true, Rule: true}},
	})
	// Chart of each group's amount.
	sections = append(sections, docs.Section{Kind: docs.SectionChart, Title: "Sales by Item Type", ValueUnit: "KES", Bars: bars})

	report := h.newReport(ctx, tid, oid, "Sales by Item Type", "", from, to, false)
	report.Sections = sections
	h.write(w, r, report, "sales-by-item-type")
}

// ── 3. Daily Sales ──────────────────────────────────────────────────────────────

// DailySales handles GET /{tenantID}/pos/reports/daily-sales — cards + a per-day table (orders, net,
// VAT, gross). Mirrors ExportDailyReport's day-bucket computation.
func (h *ReportPDFHandler) DailySales(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)
	from, to := parseReportRange(r)

	orders, err := h.completedOrders(ctx, tid, oid, from, to, false)
	if err != nil {
		h.log.Error("daily-sales: orders query failed", zap.Error(err))
		jsonError(w, "failed to generate daily sales", http.StatusInternalServerError)
		return
	}

	type dayRow struct {
		orders          int
		net, tax, gross float64
	}
	buckets := make(map[string]*dayRow)
	var totOrders int
	var totNet, totTax, totGross float64
	for _, o := range orders {
		day := o.CreatedAt.UTC().Format("2006-01-02")
		if buckets[day] == nil {
			buckets[day] = &dayRow{}
		}
		buckets[day].orders++
		buckets[day].net += o.Subtotal
		buckets[day].tax += o.TaxTotal
		buckets[day].gross += o.TotalAmount
		totOrders++
		totNet += o.Subtotal
		totTax += o.TaxTotal
		totGross += o.TotalAmount
	}
	days := make([]string, 0, len(buckets))
	for d := range buckets {
		days = append(days, d)
	}
	sort.Strings(days)
	rows := make([][]docs.Cell, 0, len(days))
	for _, d := range days {
		b := buckets[d]
		rows = append(rows, []docs.Cell{
			docs.Text(d),
			docs.Text(strconv.Itoa(b.orders)),
			docs.Text(fmtAmount(b.net)),
			docs.Text(fmtAmount(b.tax)),
			docs.Text(fmtAmount(b.gross)),
		})
	}
	var avgTicket float64
	if totOrders > 0 {
		avgTicket = totGross / float64(totOrders)
	}

	report := h.newReport(ctx, tid, oid, "Daily Sales", "", from, to, false)
	report.Cards = []docs.Card{
		{Label: "Gross Revenue", Value: "KES " + fmtAmount(totGross), Sub: fmt.Sprintf("%d order(s)", totOrders)},
		{Label: "Orders", Value: strconv.Itoa(totOrders)},
		{Label: "Avg Ticket", Value: "KES " + fmtAmount(avgTicket)},
	}
	report.Sections = []docs.Section{{
		Kind:  docs.SectionTable,
		Title: "Sales by Day",
		Columns: []docs.Column{
			{Header: "Date", Weight: 1.6},
			{Header: "Orders", Weight: 1, Align: "R"},
			{Header: "Net", Weight: 1.2, Money: true},
			{Header: "VAT", Weight: 1.2, Money: true},
			{Header: "Gross", Weight: 1.2, Money: true},
		},
		Rows: rows,
		Total: []docs.Cell{
			docs.BoldText("Total"),
			docs.BoldText(strconv.Itoa(totOrders)),
			docs.BoldText(fmtAmount(totNet)),
			docs.BoldText(fmtAmount(totTax)),
			docs.BoldText(fmtAmount(totGross)),
		},
	}}
	h.write(w, r, report, "daily-sales")
}

// ── 4. Shift (X) Report ─────────────────────────────────────────────────────────

// ShiftReportPDF handles GET /{tenantID}/pos/reports/shift/{sessionID} — a per-shift X report:
// open/close, sales, tender breakdown and cash variance. Mirrors ReportsHandler.ShiftReport.
func (h *ReportPDFHandler) ShiftReportPDF(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	sessionID, err := uuid.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		jsonError(w, "invalid session_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	session, err := h.db.POSDeviceSession.Get(ctx, sessionID)
	if err != nil || session.TenantID != tid {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}

	orders, err := h.db.POSOrder.Query().
		Where(posorder.TenantID(tid), posorder.DeviceID(session.DeviceID), posorder.StatusEQ("completed")).
		All(ctx)
	if err != nil {
		h.log.Error("shift report: orders query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	orderIDs := make([]uuid.UUID, 0, len(orders))
	var totalRevenue, totalTax, totalDiscount float64
	for _, o := range orders {
		orderIDs = append(orderIDs, o.ID)
		totalRevenue += o.TotalAmount
		totalTax += o.TaxTotal
		totalDiscount += o.DiscountTotal
	}
	var totalRefunds float64
	if len(orderIDs) > 0 {
		refunds, rerr := h.db.POSRefund.Query().Where(posrefund.OrderIDIn(orderIDs...)).All(ctx)
		if rerr != nil {
			h.log.Warn("shift report: refunds query failed", zap.Error(rerr))
		}
		for _, ref := range refunds {
			totalRefunds += ref.Amount
		}
	}
	tenderRows, tenderTotal := h.tenderBreakdown(ctx, tid, orderIDs)
	tenderRows = append(tenderRows, docs.KV{Label: "Tendering Total", Value: fmtAmount(tenderTotal), Bold: true, Rule: true})

	closedAt := "—"
	if session.ClosedAt != nil {
		closedAt = session.ClosedAt.UTC().Format("02 Jan 2006 15:04")
	}
	closingFloat := 0.0
	if session.ClosingFloat != nil {
		closingFloat = *session.ClosingFloat
	}
	variance := 0.0
	if session.Variance != nil {
		variance = *session.Variance
	}

	report := h.newReport(ctx, tid, nil, "Shift Report", "X Report", session.OpenedAt.UTC(), timeOrNow(session.ClosedAt), false)
	report.Cards = []docs.Card{
		{Label: "Net Sales", Value: "KES " + fmtAmount(totalRevenue-totalRefunds), Sub: fmt.Sprintf("%d order(s)", len(orders))},
		{Label: "Refunds", Value: "KES " + fmtAmount(totalRefunds)},
		{Label: "Cash Variance", Value: "KES " + fmtAmount(variance)},
	}
	report.Sections = []docs.Section{
		{Kind: docs.SectionKeyValue, Title: "Shift", Pairs: []docs.KV{
			{Label: "Session", Value: session.ID.String()},
			{Label: "Device", Value: session.DeviceID.String()},
			{Label: "Opened", Value: session.OpenedAt.UTC().Format("02 Jan 2006 15:04")},
			{Label: "Closed", Value: closedAt},
			{Label: "Status", Value: strings.ToUpper(session.SessionStatus)},
		}},
		{Kind: docs.SectionKeyValue, Title: "Sales", Pairs: []docs.KV{
			{Label: "Orders", Value: strconv.Itoa(len(orders))},
			{Label: "Gross Sales", Value: fmtAmount(totalRevenue)},
			{Label: "Tax", Value: fmtAmount(totalTax)},
			{Label: "Discounts", Value: fmtAmount(totalDiscount)},
			{Label: "Refunds", Value: fmtAmount(totalRefunds)},
			{Label: "Net Sales", Value: fmtAmount(totalRevenue - totalRefunds), Bold: true, Rule: true},
		}},
		{Kind: docs.SectionKeyValue, Title: "Tender Breakdown", Pairs: tenderRows},
		{Kind: docs.SectionKeyValue, Title: "Cash", Pairs: []docs.KV{
			{Label: "Opening Float", Value: fmtAmount(session.FloatAmount)},
			{Label: "Closing Float", Value: fmtAmount(closingFloat)},
			{Label: "Variance", Value: fmtAmount(variance), Bold: true, Rule: true},
		}},
	}
	h.write(w, r, report, "shift-report")
}

// timeOrNow returns the pointed-to time (UTC) or now when nil — used for an open shift's period end.
func timeOrNow(t *time.Time) time.Time {
	if t != nil {
		return t.UTC()
	}
	return time.Now().UTC()
}

// ── 5. Sales by Staff ───────────────────────────────────────────────────────────

// SalesByStaffPDF handles GET /{tenantID}/pos/reports/staff — landscape per-server sales table.
// Mirrors ReportsHandler.SalesByStaff.
func (h *ReportPDFHandler) SalesByStaffPDF(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)
	from, to := parseReportRange(r)

	completed, err := h.completedOrders(ctx, tid, oid, from, to, false)
	if err != nil {
		h.log.Error("staff report: completed query failed", zap.Error(err))
		jsonError(w, "failed to generate staff report", http.StatusInternalServerError)
		return
	}
	voidPreds := voidedPreds(tid, oid, from, to)
	voided, err := h.db.POSOrder.Query().Where(voidPreds...).All(ctx)
	if err != nil {
		h.log.Error("staff report: voided query failed", zap.Error(err))
		jsonError(w, "failed to generate staff report", http.StatusInternalServerError)
		return
	}

	type staffBucket struct {
		orders            int
		revenue, discount float64
		voids             int
	}
	buckets := make(map[uuid.UUID]*staffBucket)
	for _, o := range completed {
		if buckets[o.UserID] == nil {
			buckets[o.UserID] = &staffBucket{}
		}
		buckets[o.UserID].orders++
		buckets[o.UserID].revenue += o.TotalAmount
		buckets[o.UserID].discount += o.DiscountTotal
	}
	for _, o := range voided {
		if buckets[o.UserID] == nil {
			buckets[o.UserID] = &staffBucket{}
		}
		buckets[o.UserID].voids++
	}
	ids := make([]uuid.UUID, 0, len(buckets))
	for id := range buckets {
		ids = append(ids, id)
	}
	names := h.resolveStaffNames(ctx, tid, ids)

	type staffRow struct {
		name   string
		bucket *staffBucket
	}
	list := make([]staffRow, 0, len(buckets))
	for id, b := range buckets {
		n := names[id]
		if n == "" {
			n = "Unknown"
		}
		list = append(list, staffRow{name: n, bucket: b})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].bucket.revenue > list[j].bucket.revenue })

	rows := make([][]docs.Cell, 0, len(list))
	var totOrders, totVoids int
	var totRevenue, totDiscount float64
	for _, s := range list {
		b := s.bucket
		avg := 0.0
		if b.orders > 0 {
			avg = b.revenue / float64(b.orders)
		}
		rows = append(rows, []docs.Cell{
			docs.Text(s.name),
			docs.Text(strconv.Itoa(b.orders)),
			docs.Text(fmtAmount(b.revenue)),
			docs.Text(fmtAmount(b.discount)),
			docs.Text(strconv.Itoa(b.voids)),
			docs.Text(fmtAmount(avg)),
		})
		totOrders += b.orders
		totVoids += b.voids
		totRevenue += b.revenue
		totDiscount += b.discount
	}
	totAvg := 0.0
	if totOrders > 0 {
		totAvg = totRevenue / float64(totOrders)
	}

	// Chart: revenue by server. Capped at the top 12 so the bar labels stay readable — the
	// full breakdown (including everyone else) is still in the table above.
	chartList := list
	if len(chartList) > 12 {
		chartList = chartList[:12]
	}
	bars := make([]docs.Bar, 0, len(chartList))
	for _, s := range chartList {
		bars = append(bars, docs.Bar{Label: s.name, Value: s.bucket.revenue})
	}

	report := h.newReport(ctx, tid, oid, "Sales by Staff", "", from, to, true)
	report.Cards = []docs.Card{
		{Label: "Total Revenue", Value: "KES " + fmtAmount(totRevenue)},
		{Label: "Orders", Value: strconv.Itoa(totOrders)},
		{Label: "Avg Ticket", Value: "KES " + fmtAmount(totAvg)},
		{Label: "Voids", Value: strconv.Itoa(totVoids)},
	}
	report.Sections = []docs.Section{
		{
			Kind:  docs.SectionTable,
			Title: "Servers",
			Columns: []docs.Column{
				{Header: "Server", Weight: 2.2},
				{Header: "Orders", Weight: 1, Align: "R"},
				{Header: "Net Sales", Weight: 1.4, Money: true},
				{Header: "Discounts", Weight: 1.4, Money: true},
				{Header: "Voids", Weight: 1, Align: "R"},
				{Header: "Avg Ticket", Weight: 1.4, Money: true},
			},
			Rows: rows,
			Total: []docs.Cell{
				docs.BoldText("Total"),
				docs.BoldText(strconv.Itoa(totOrders)),
				docs.BoldText(fmtAmount(totRevenue)),
				docs.BoldText(fmtAmount(totDiscount)),
				docs.BoldText(strconv.Itoa(totVoids)),
				docs.BoldText(fmtAmount(totAvg)),
			},
		},
		{Kind: docs.SectionChart, Title: "Revenue by Server", ValueUnit: "KES", Bars: bars},
	}
	h.write(w, r, report, "sales-by-staff")
}

// ── 6. Tax Report ───────────────────────────────────────────────────────────────

// TaxReportPDF handles GET /{tenantID}/pos/reports/tax-document — tax lines grouped by KRA code +
// rate with totals. Mirrors ReportsHandler.TaxReport.
func (h *ReportPDFHandler) TaxReportPDF(w http.ResponseWriter, r *http.Request) {
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
		h.log.Error("tax report: orders query failed", zap.Error(err))
		jsonError(w, "failed to generate tax report", http.StatusInternalServerError)
		return
	}

	type bucketKey struct {
		code string
		rate float64
	}
	type taxBucket struct {
		code         string
		rate         float64
		taxable, tax float64
	}
	buckets := make(map[bucketKey]*taxBucket)
	var totalTaxable, totalTax float64
	for _, o := range orders {
		for _, l := range o.Edges.Lines {
			rate := 0.0
			if l.TaxRate != nil {
				rate = *l.TaxRate
			}
			lineTotal := l.UnitPrice * l.Quantity
			taxAmt := lineTotal * (rate / 100)
			k := bucketKey{code: l.TaxKraCode, rate: rate}
			if buckets[k] == nil {
				buckets[k] = &taxBucket{code: l.TaxKraCode, rate: rate}
			}
			buckets[k].taxable += lineTotal
			buckets[k].tax += taxAmt
			totalTaxable += lineTotal
			totalTax += taxAmt
		}
	}
	list := make([]*taxBucket, 0, len(buckets))
	for _, b := range buckets {
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].tax > list[j].tax })

	rows := make([][]docs.Cell, 0, len(list))
	for _, b := range list {
		code := b.code
		if code == "" {
			code = "—"
		}
		rows = append(rows, []docs.Cell{
			docs.Text(code),
			docs.Text(fmtQty(b.rate) + "%"),
			docs.Text(fmtAmount(b.taxable)),
			docs.Text(fmtAmount(b.tax)),
		})
	}

	report := h.newReport(ctx, tid, oid, "Tax Report", "eTIMS / VAT", from, to, false)
	report.Sections = []docs.Section{{
		Kind:  docs.SectionTable,
		Title: "Tax by Code & Rate",
		Columns: []docs.Column{
			{Header: "KRA Code", Weight: 1.4},
			{Header: "Rate", Weight: 1, Align: "R"},
			{Header: "Taxable Amount", Weight: 1.6, Money: true},
			{Header: "Tax Amount", Weight: 1.6, Money: true},
		},
		Rows: rows,
		Total: []docs.Cell{
			docs.BoldText("Total"),
			docs.Text(""),
			docs.BoldText(fmtAmount(totalTaxable)),
			docs.BoldText(fmtAmount(totalTax)),
		},
	}}
	h.write(w, r, report, "tax-document")
}

// ── 7. Most Profitable Items ────────────────────────────────────────────────────

// MostProfitablePDF handles GET /{tenantID}/pos/reports/most-profitable-document — items ranked by
// profit. Mirrors ReportsHandler.MostProfitableItems.
func (h *ReportPDFHandler) MostProfitablePDF(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)
	from, to := parseReportRange(r)

	limit := 20
	if ls := r.URL.Query().Get("limit"); ls != "" {
		if n, perr := strconv.Atoi(ls); perr == nil && n > 0 {
			limit = n
		}
	}

	orders, err := h.completedOrders(ctx, tid, oid, from, to, true)
	if err != nil {
		h.log.Error("most-profitable: orders query failed", zap.Error(err))
		jsonError(w, "failed to generate most profitable report", http.StatusInternalServerError)
		return
	}

	type itemAgg struct {
		sku, name                                   string
		units, revenue, unitCost, profit, marginPct float64
	}
	buckets := make(map[string]*itemAgg)
	currency := "KES"
	for _, o := range orders {
		if o.Currency != "" {
			currency = o.Currency
		}
		for _, l := range o.Edges.Lines {
			b := buckets[l.Sku]
			if b == nil {
				b = &itemAgg{sku: l.Sku, name: l.Name}
				buckets[l.Sku] = b
			}
			if b.name == "" {
				b.name = l.Name
			}
			b.units += l.Quantity
			b.revenue += l.Quantity * l.UnitPrice
		}
	}
	// Batched (not N+1) — see resolveUnitCostsBySKU for the GOODS-cost_price vs
	// RECIPE-cost_per_portion split. Mirrors ReportsHandler.MostProfitableItems exactly.
	costBySKU := resolveUnitCostsBySKU(r, h.db, h.log)
	for sku, b := range buckets {
		b.unitCost = costBySKU[sku]
		b.profit = b.revenue - b.unitCost*b.units
		if b.revenue != 0 {
			b.marginPct = b.profit / b.revenue * 100
		}
	}
	list := make([]*itemAgg, 0, len(buckets))
	var totRevenue, totCost, totProfit float64
	for _, b := range buckets {
		list = append(list, b)
		totRevenue += b.revenue
		totCost += b.unitCost * b.units
		totProfit += b.profit
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].profit != list[j].profit {
			return list[i].profit > list[j].profit
		}
		return list[i].revenue > list[j].revenue
	})
	if len(list) > limit {
		list = list[:limit]
	}

	rows := make([][]docs.Cell, 0, len(list))
	for _, b := range list {
		name := b.name
		if name == "" {
			name = b.sku
		}
		rows = append(rows, []docs.Cell{
			docs.Text(name),
			docs.Text(fmtQty(b.units)),
			docs.Text(fmtAmount(b.revenue)),
			docs.Text(fmtAmount(b.unitCost * b.units)),
			docs.Text(fmtAmount(b.profit)),
			docs.Text(fmtQty(b.marginPct) + "%"),
		})
	}
	totMargin := 0.0
	if totRevenue != 0 {
		totMargin = totProfit / totRevenue * 100
	}

	report := h.newReport(ctx, tid, oid, "Most Profitable Items", "", from, to, true)
	report.Currency = currency
	report.Sections = []docs.Section{{
		Kind:  docs.SectionTable,
		Title: "Profitability Ranking",
		Columns: []docs.Column{
			{Header: "Item", Weight: 2.4},
			{Header: "Qty", Weight: 1, Align: "R"},
			{Header: "Revenue", Weight: 1.4, Money: true},
			{Header: "Cost", Weight: 1.4, Money: true},
			{Header: "Profit", Weight: 1.4, Money: true},
			{Header: "Margin", Weight: 1, Align: "R"},
		},
		Rows: rows,
		Total: []docs.Cell{
			docs.BoldText("Total"),
			docs.Text(""),
			docs.BoldText(fmtAmount(totRevenue)),
			docs.BoldText(fmtAmount(totCost)),
			docs.BoldText(fmtAmount(totProfit)),
			docs.BoldText(fmtQty(totMargin) + "%"),
		},
	}}
	h.write(w, r, report, "most-profitable-document")
}
