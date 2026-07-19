package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/ent/tender"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/docs"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/outletpolicy"
)

// ── Shared All-Sales filter building ─────────────────────────────────────────
//
// allSalesOrderFilters translates the All-Sales query params into POSOrder predicates. It is the
// SINGLE source of truth for the page's filter semantics: POSOrderHandler.ListOrders (the JSON
// list) and ReportPDFHandler.AllSalesDocument (the PDF/CSV export) both call it, so the exported
// document always contains exactly the rows the user is looking at — an export that silently
// disagreed with the on-screen list would be worse than no export at all.
//
// Tenant scoping and the per-cashier visibility predicate are NOT applied here — callers prepend
// posorder.TenantID(tid) and ownOrdersScope themselves (they differ in how they obtain the rbac
// checker).
func allSalesOrderFilters(r *http.Request, client *ent.Client, tid uuid.UUID, loc *time.Location) []predicate.POSOrder {
	q := r.URL.Query()
	var filters []predicate.POSOrder

	// Business Location: an explicit ?outlet_id= query param wins (the All-Sales location
	// filter). "all" (admins) lists every outlet. Absent → scope to the header outlet context.
	if outletParam := q.Get("outlet_id"); outletParam != "" {
		if !strings.EqualFold(outletParam, "all") {
			if oid, parseErr := uuid.Parse(outletParam); parseErr == nil {
				filters = append(filters, posorder.OutletID(oid))
			}
		}
	} else if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			filters = append(filters, posorder.OutletID(oid))
		}
	}

	if status := q.Get("status"); status != "" {
		statuses := strings.Split(status, ",")
		if len(statuses) > 1 {
			filters = append(filters, posorder.StatusIn(statuses...))
		} else {
			filters = append(filters, posorder.Status(statuses[0]))
		}
	}
	// staff_id / user_id scope the list to orders created by a specific staff member.
	staffFilter := q.Get("staff_id")
	if staffFilter == "" {
		staffFilter = q.Get("user_id")
	}
	if staffFilter != "" {
		if staffUID, err := uuid.Parse(staffFilter); err == nil {
			filters = append(filters, posorder.UserID(staffUID))
		}
	}
	// Free-text invoice/receipt search: case-insensitive CONTAINS on order_number. Receipt
	// numbers are just "RCT-"+order_number (see printing.ReceiptView), so a pasted receipt
	// number matches its order once the RCT- prefix is stripped. Whitespace-trimmed so a
	// copy-paste with stray spaces still hits.
	if orderNum := strings.TrimSpace(q.Get("order_number")); orderNum != "" {
		orderNum = strings.TrimPrefix(orderNum, "RCT-")
		orderNum = strings.TrimPrefix(orderNum, "rct-")
		filters = append(filters, posorder.OrderNumberContainsFold(orderNum))
	}
	// Sources: pos_terminal vs back_office.
	if src := q.Get("source"); src != "" && !strings.EqualFold(src, "all") {
		filters = append(filters, posorder.Source(src))
	}
	// Date range on the order's EFFECTIVE date (accepts RFC3339 or YYYY-MM-DD) — the
	// admin business_date override when a sale's date was moved, else created_at.
	// Date-only bounds are read in the tenant timezone so a day matches its wall clock.
	if from := parseDateParam(q.Get("from"), false, loc); from != nil {
		filters = append(filters, effectiveDateGTE(*from))
	}
	if to := parseDateParam(q.Get("to"), true, loc); to != nil {
		filters = append(filters, effectiveDateLTE(*to))
	}
	// Customer: match phone or name (contains, case-insensitive).
	if cust := strings.TrimSpace(q.Get("customer")); cust != "" {
		filters = append(filters, posorder.Or(
			posorder.CustomerPhoneContainsFold(cust),
			posorder.CustomerNameContainsFold(cust),
		))
	}
	// Payment status — driven by the stored paid_total column (sum of completed payments,
	// maintained by the payments service) so the filter provably agrees with the per-row
	// badge from derivePaymentStatus: paid = settled in full, partial = 0 < paid < total,
	// due = nothing paid yet. Terminal statuses (refunded/voided/cancelled) are filterable
	// too, and "overdue" surfaces sales past their metadata.payment_due_date — either still
	// owing at the till, or on-account credit sales (stamped by recordCreditSale from the
	// customer's payment period) whose settlement lives in treasury AR.
	switch strings.ToLower(q.Get("payment_status")) {
	case "paid":
		// A completed on-account (credit) sale whose money hasn't been collected is a DEBT —
		// it must NOT match "paid" even though the order status is completed.
		filters = append(filters, posorder.Or(
			posorder.And(
				posorder.Status("completed"),
				posorder.Not(posorder.And(onAccountOrder(), paidBelowTotal())),
			),
			posorder.And(
				posorder.StatusNotIn("refunded", "voided", "cancelled"),
				paidCoversTotal(),
			),
		))
	case "partial":
		// Completed credit sales with partial collections surface here too (on-account only —
		// regular completed sales are always fully settled by definition).
		filters = append(filters, posorder.And(
			posorder.StatusNotIn("refunded", "voided", "cancelled"),
			posorder.PaidTotalGT(0),
			paidBelowTotal(),
			posorder.Or(posorder.StatusNEQ("completed"), onAccountOrder()),
		))
	case "due", "unpaid":
		filters = append(filters, posorder.And(
			posorder.StatusNotIn("refunded", "voided", "cancelled"),
			posorder.PaidTotalLTE(0),
			posorder.Or(posorder.StatusNEQ("completed"), onAccountOrder()),
		))
	case "overdue":
		filters = append(filters, posorder.And(
			posorder.StatusNotIn("refunded", "voided", "cancelled"),
			pastPaymentDueDate(),
			posorder.Or(paidBelowTotal(), onAccountOrder()),
		))
	case "refunded", "voided", "cancelled":
		filters = append(filters, posorder.Status(strings.ToLower(q.Get("payment_status"))))
	}
	// Payment method → the real method the terminal used is stamped on each payment's
	// payment_data.method (the POS reuses ONE generic tender across cash/card/mpesa/…, so the
	// tender's own type does NOT identify the method). Match orders with a payment carrying that
	// method, OR — for legacy setups that configured a distinct tender per method — a payment on a
	// tender of that type. Either path satisfies the filter.
	if pm := strings.TrimSpace(q.Get("payment_method")); pm != "" && !strings.EqualFold(pm, "all") {
		methodFilters := []predicate.POSOrder{
			posorder.HasPaymentsWith(predicate.POSPayment(func(s *sql.Selector) {
				s.Where(sqljson.ValueEQ(pospayment.FieldPaymentData, pm, sqljson.Path("method")))
			})),
		}
		if tenderIDs, _ := client.Tender.Query().
			Where(tender.TenantID(tid), tender.TypeEQ(pm)).
			IDs(r.Context()); len(tenderIDs) > 0 {
			methodFilters = append(methodFilters, posorder.HasPaymentsWith(pospayment.TenderIDIn(tenderIDs...)))
		}
		filters = append(filters, posorder.Or(methodFilters...))
	}
	// Shipping status — stored in metadata.shipping_status (set by Edit Shipping / Add Sale).
	// "any" matches every order that HAS shipping info (the Sell → Shipments page's base set).
	if ship := strings.TrimSpace(q.Get("shipping_status")); ship != "" && !strings.EqualFold(ship, "all") {
		if strings.EqualFold(ship, "any") {
			filters = append(filters, predicate.POSOrder(func(s *sql.Selector) {
				s.Where(sqljson.ValueIsNotNull(posorder.FieldMetadata, sqljson.Path("shipping_status")))
			}))
		} else {
			filters = append(filters, predicate.POSOrder(func(s *sql.Selector) {
				s.Where(sqljson.ValueEQ(posorder.FieldMetadata, ship, sqljson.Path("shipping_status")))
			}))
		}
	}
	// Subscriptions-only: orders flagged as subscription sales in metadata.
	if strings.EqualFold(q.Get("subscriptions"), "true") || q.Get("subscriptions") == "1" {
		filters = append(filters, predicate.POSOrder(func(s *sql.Selector) {
			s.Where(sqljson.ValueEQ(posorder.FieldMetadata, true, sqljson.Path("is_subscription")))
		}))
	}
	// Order-total range — the "amount between" filter (the All-Sales slider). Bounds match on
	// the sale's payable (total_amount); either bound is optional.
	if v := strings.TrimSpace(q.Get("min_total")); v != "" {
		if min, err := strconv.ParseFloat(v, 64); err == nil {
			filters = append(filters, posorder.TotalAmountGTE(min))
		}
	}
	if v := strings.TrimSpace(q.Get("max_total")); v != "" {
		if max, err := strconv.ParseFloat(v, 64); err == nil {
			filters = append(filters, posorder.TotalAmountLTE(max))
		}
	}
	// KDS station — match orders that have at least one line routed to the given station
	// (kds_station_id is stamped on every line at order creation; see resolveStationForLine).
	if ks := strings.TrimSpace(q.Get("kds_station_id")); ks != "" && !strings.EqualFold(ks, "all") {
		if ksID, perr := uuid.Parse(ks); perr == nil {
			filters = append(filters, posorder.HasLinesWith(posorderline.KdsStationID(ksID)))
		}
	}
	// Category — match orders with at least one line in the given catalog category
	// (case-insensitive exact match, mirroring the reports' category grouping).
	if cat := strings.TrimSpace(q.Get("category")); cat != "" && !strings.EqualFold(cat, "all") {
		filters = append(filters, posorder.HasLinesWith(posorderline.CategoryEqualFold(cat)))
	}
	return filters
}

// ownOrdersScope limits order reads to the caller's own orders (+ shared active bills) when the
// principal only holds pos.orders.view_own — the server-side backing of the "My Sales" view.
// Shared by ListOrders and the All-Sales export so a view_own cashier can never export other
// cashiers' sales either.
//
// Full-view = pos.orders.view / pos.orders.manage (NOT change — a waiter holds change but should
// still be own-scoped by default; granting pos.orders.view is the "super waiter" lever). When the
// principal is NOT full-view, the per-outlet cashier_sales_visibility policy decides: "outlet"
// widens a view_own cashier to all sales at their (tenant/outlet-bounded) outlet; "own" (the
// hospitality default) keeps the own + shared-active-bill scope. Server-authoritative.
func ownOrdersScope(r *http.Request, rbac outletmw.PermissionChecker, db *ent.Client) (predicate.POSOrder, bool) {
	if outletmw.HasServicePermission(r, rbac, "pos.orders.view", "pos.orders.manage") {
		return nil, false
	}
	// Per-outlet policy: an outlet configured (or defaulting) to "outlet" visibility lets a
	// view_own cashier see every sale at the active outlet (the retail/supermarket default).
	if resolveCashierSalesVisibility(r, db) == outletpolicy.VisibilityOutlet {
		return nil, false
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil || claims.Subject == "" {
		return nil, false
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil || uid == uuid.Nil {
		return nil, false
	}
	return posorder.Or(
		posorder.UserID(uid),
		posorder.StatusIn(orders.StatusOpen, orders.StatusPendingPayment),
	), true
}

// resolveCashierSalesVisibility resolves the active outlet's cashier_sales_visibility policy
// (outlet override → per-use-case default). The active outlet is the ?outlet_id= param (unless
// "all") else the X-Outlet-ID header. With no resolvable outlet (e.g. an HQ all-outlets view) we
// default to the safe/restrictive "own". Best-effort: any lookup error also yields "own".
func resolveCashierSalesVisibility(r *http.Request, db *ent.Client) string {
	if db == nil {
		return outletpolicy.VisibilityOwn
	}
	var outletID uuid.UUID
	if p := strings.TrimSpace(r.URL.Query().Get("outlet_id")); p != "" && !strings.EqualFold(p, "all") {
		if oid, err := uuid.Parse(p); err == nil {
			outletID = oid
		}
	}
	if outletID == uuid.Nil {
		if hv := strings.TrimSpace(httpware.GetOutletID(r.Context())); hv != "" && !strings.EqualFold(hv, "all") {
			if oid, err := uuid.Parse(hv); err == nil {
				outletID = oid
			}
		}
	}
	if outletID == uuid.Nil {
		return outletpolicy.VisibilityOwn
	}
	o, err := db.Outlet.Query().
		Where(entoutlet.ID(outletID)).
		WithSettings().
		Only(r.Context())
	if err != nil || o == nil {
		return outletpolicy.VisibilityOwn
	}
	useCase := ""
	if o.UseCase != nil {
		useCase = *o.UseCase
	}
	var override *string
	if o.Edges.Settings != nil {
		override = o.Edges.Settings.CashierSalesVisibility
	}
	return outletpolicy.ResolveCashierSalesVisibility(useCase, override)
}

// ── All-Sales export document ────────────────────────────────────────────────

// allSalesExportCap bounds a single export. Far above any real day/range at current volumes; when
// a range genuinely exceeds it the report says so explicitly (never a silent truncation).
const allSalesExportCap = 5000

// exportMethodLabels renders payment_data.method codes as the human labels the All-Sales UI uses
// (pos-ui sales-shared.tsx PAYMENT_METHOD_LABELS) so the printed report reads like the screen.
var exportMethodLabels = map[string]string{
	"cash": "Cash", "card": "Card", "card_manual": "Card / PDQ", "pdq": "Card / PDQ",
	"card_terminal": "Card / PDQ", "cheque": "Cheque", "bank_transfer": "Bank Transfer",
	"mpesa": "M-Pesa", "mpesa_manual": "M-Pesa (Code)", "manual": "M-Pesa (Code)",
	"wallet": "Wallet", "cod": "Cash on Delivery", "on_account": "On Account",
	"room_charge": "Room Charge", "complimentary": "Complimentary", "loyalty": "Loyalty Points",
	"multiple": "Multiple",
}

func exportMethodLabel(code string) string {
	if code == "" {
		return "—"
	}
	if l, ok := exportMethodLabels[strings.ToLower(code)]; ok {
		return l
	}
	return strings.ReplaceAll(code, "_", " ")
}

// SetRBAC wires the RBAC checker backing the per-cashier visibility scoping on the export.
func (h *ReportPDFHandler) SetRBAC(rbac outletmw.PermissionChecker) { h.rbac = rbac }

// AllSalesDocument handles GET /{tenantID}/pos/reports/all-sales-document?format=pdf|csv — the
// export behind the All-Sales page's Export button. It honors every filter the list honors (same
// query params, same allSalesOrderFilters predicates, same per-cashier scoping) and renders one
// audit-grade row per order: when it happened (till time + reported date when moved), what it was
// (invoice no, status incl. void reason), who (customer/contact/cashier), where (outlet), how it
// was paid (status + method), and the full money trail (subtotal, discount, tax, total, paid, due).
func (h *ReportPDFHandler) AllSalesDocument(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	loc := tenantLocation(ctx, h.db, tid)

	preds := []predicate.POSOrder{posorder.TenantID(tid)}
	if ownPred, scoped := ownOrdersScope(r, h.rbac, h.db); scoped {
		preds = append(preds, ownPred)
	}
	preds = append(preds, allSalesOrderFilters(r, h.db, tid, loc)...)

	baseQ := h.db.POSOrder.Query().Where(preds...)
	total, _ := baseQ.Clone().Count(ctx)
	list, err := baseQ.WithLines().WithPayments().
		Order(ent.Desc(posorder.FieldCreatedAt)).
		Limit(allSalesExportCap).
		All(ctx)
	if err != nil {
		h.log.Error("all-sales export: orders query failed", zap.Error(err))
		jsonError(w, "failed to generate sales export", http.StatusInternalServerError)
		return
	}

	// Batch-resolve the display names the audit columns need: cashiers, outlets, and tender
	// types (the legacy per-method-tender fallback the list view also applies).
	uidSet := map[uuid.UUID]struct{}{}
	tenderIDSet := map[uuid.UUID]struct{}{}
	for _, o := range list {
		uidSet[o.UserID] = struct{}{}
		for _, p := range o.Edges.Payments {
			tenderIDSet[p.TenderID] = struct{}{}
		}
	}
	uids := make([]uuid.UUID, 0, len(uidSet))
	for id := range uidSet {
		uids = append(uids, id)
	}
	staffNames := h.resolveStaffNames(ctx, tid, uids)
	tenderType := map[uuid.UUID]string{}
	if len(tenderIDSet) > 0 {
		ids := make([]uuid.UUID, 0, len(tenderIDSet))
		for id := range tenderIDSet {
			ids = append(ids, id)
		}
		if tenders, terr := h.db.Tender.Query().Where(tender.IDIn(ids...)).All(ctx); terr == nil {
			for _, t := range tenders {
				tenderType[t.ID] = t.Type
			}
		}
	}
	outletNames := map[uuid.UUID]string{}
	if outlets, oerr := h.db.Outlet.Query().Where(entoutlet.TenantID(tid)).All(ctx); oerr == nil {
		for _, o := range outlets {
			outletNames[o.ID] = o.Name
		}
	}

	rows := make([][]docs.Cell, 0, len(list))
	var sumSubtotal, sumDiscount, sumTax, sumTotal, sumPaid, sumDue float64
	for _, o := range list {
		// Settled method — identical derivation to the list view (enrichOrderList).
		methods := map[string]struct{}{}
		for _, pay := range o.Edges.Payments {
			if !strings.EqualFold(pay.Status, "completed") {
				continue
			}
			m := ""
			if pay.PaymentData != nil {
				m, _ = pay.PaymentData["method"].(string)
			}
			if m == "" {
				m = tenderType[pay.TenderID]
			}
			if m != "" {
				methods[m] = struct{}{}
			}
		}
		paid := o.PaidTotal
		due := o.TotalAmount - paid
		if due < 0 {
			due = 0
		}
		ps := derivePaymentStatus(o.Status, o.TotalAmount, paid, isOnAccount(o.Metadata))
		if (ps == "due" || ps == "partial") && isOrderOverdue(o.Metadata) {
			ps = "overdue"
		}

		// Till time in the tenant's wall clock; when an admin moved the sale's reporting date,
		// show both so the audit trail keeps the physical ring-up time AND the reported day.
		when := o.CreatedAt.In(loc).Format("02/01/06 15:04")
		if eff := orders.EffectiveOrderDate(o); !eff.Equal(o.CreatedAt) {
			when += " (rep. " + eff.In(loc).Format("02/01/06") + ")"
		}
		// Fold the void/cancellation reason into the status column so a voided row explains
		// itself on the same line (audit reads the why without a second lookup).
		statusCell := o.Status
		if o.VoidedReason != nil && *o.VoidedReason != "" {
			statusCell += ": " + *o.VoidedReason
		}
		customer := "Walk-In Customer"
		if o.CustomerName != nil && *o.CustomerName != "" {
			customer = *o.CustomerName
		}
		contact := "—"
		if o.CustomerPhone != nil && *o.CustomerPhone != "" {
			contact = *o.CustomerPhone
		}
		cashier := staffNames[o.UserID]
		if cashier == "" {
			cashier = "—"
		}
		outletName := outletNames[o.OutletID]
		if outletName == "" {
			outletName = "—"
		}

		rows = append(rows, []docs.Cell{
			docs.Text(when),
			docs.Text(o.OrderNumber),
			docs.Text(statusCell),
			docs.Text(customer),
			docs.Text(contact),
			docs.Text(cashier),
			docs.Text(outletName),
			docs.Text(ps),
			docs.Text(exportMethodLabel(dominantMethod(methods))),
			docs.Text(fmtAmount(o.Subtotal)),
			docs.Text(fmtAmount(o.DiscountTotal)),
			docs.Text(fmtAmount(o.TaxTotal)),
			docs.Text(fmtAmount(o.TotalAmount)),
			docs.Text(fmtAmount(paid)),
			docs.Text(fmtAmount(due)),
		})
		sumSubtotal += o.Subtotal
		sumDiscount += o.DiscountTotal
		sumTax += o.TaxTotal
		sumTotal += o.TotalAmount
		sumPaid += paid
		sumDue += due
	}

	// Report period for the header: the explicit range when given, else the fetched rows' span.
	q := r.URL.Query()
	var from, to time.Time
	if f := parseDateParam(q.Get("from"), false, loc); f != nil {
		from = *f
	}
	if t := parseDateParam(q.Get("to"), true, loc); t != nil {
		to = *t
	}
	if from.IsZero() && len(list) > 0 {
		from = list[len(list)-1].CreatedAt
	}
	if to.IsZero() {
		to = time.Now()
	}

	report := h.newReport(ctx, tid, h.outletScope(r), "All Sales", "Every sale in the selected filters — audit detail", from, to, true)
	report.Cards = []docs.Card{
		{Label: "Sales", Value: strconv.Itoa(total)},
		{Label: "Gross", Value: "KES " + fmtAmount(sumSubtotal)},
		{Label: "Discounts", Value: "KES " + fmtAmount(sumDiscount)},
		{Label: "Net Total", Value: "KES " + fmtAmount(sumTotal)},
		{Label: "Paid", Value: "KES " + fmtAmount(sumPaid)},
		{Label: "Outstanding", Value: "KES " + fmtAmount(sumDue)},
	}
	note := ""
	if total > len(list) {
		note = fmt.Sprintf("Showing the most recent %d of %d matching sales — narrow the date range to export the rest.", len(list), total)
	}
	report.Sections = []docs.Section{{
		Kind:  docs.SectionTable,
		Title: "Sales",
		Note:  note,
		Columns: []docs.Column{
			{Header: "Date", Weight: 1.5},
			{Header: "Invoice No.", Weight: 1.7},
			{Header: "Status", Weight: 1.1},
			{Header: "Customer", Weight: 1.4},
			{Header: "Contact", Weight: 1.2},
			{Header: "Cashier", Weight: 1.3},
			{Header: "Outlet", Weight: 1.1},
			{Header: "Pay Status", Weight: 0.9},
			{Header: "Method", Weight: 1.1},
			{Header: "Subtotal", Weight: 1, Money: true},
			{Header: "Discount", Weight: 1, Money: true},
			{Header: "Tax", Weight: 0.9, Money: true},
			{Header: "Total", Weight: 1, Money: true},
			{Header: "Paid", Weight: 1, Money: true},
			{Header: "Due", Weight: 1, Money: true},
		},
		Rows: rows,
		Total: []docs.Cell{
			docs.BoldText("Total"), docs.BoldText(""), docs.BoldText(""), docs.BoldText(""),
			docs.BoldText(""), docs.BoldText(""), docs.BoldText(""), docs.BoldText(""),
			docs.BoldText(strconv.Itoa(len(rows)) + " sale(s)"),
			docs.BoldText(fmtAmount(sumSubtotal)),
			docs.BoldText(fmtAmount(sumDiscount)),
			docs.BoldText(fmtAmount(sumTax)),
			docs.BoldText(fmtAmount(sumTotal)),
			docs.BoldText(fmtAmount(sumPaid)),
			docs.BoldText(fmtAmount(sumDue)),
		},
	}}
	h.write(w, r, report, "all-sales")
}
