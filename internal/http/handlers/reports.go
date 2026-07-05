package handlers

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcommrec "github.com/bengobox/pos-service/internal/ent/commissionrecord"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/ent/posrefund"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	entuser "github.com/bengobox/pos-service/internal/ent/user"
)

type ReportsHandler struct {
	log       *zap.Logger
	db        *ent.Client
	inventory brandResolver
}

// brandResolver resolves sku → brand name (satisfied by the inventory S2S client). Kept as a
// narrow interface so reports don't depend on the whole inventory client and it can be nil.
type brandResolver interface {
	GetBrandsBySKU(ctx context.Context, tenantID string, skus []string) (map[string]string, error)
}

// SetInventoryClient wires the inventory S2S client used to resolve brands for the
// register-details "products sold by brand" section.
func (h *ReportsHandler) SetInventoryClient(inv brandResolver) {
	h.inventory = inv
}

func NewReportsHandler(log *zap.Logger, db *ent.Client) *ReportsHandler {
	return &ReportsHandler{log: log, db: db}
}

// GetSummary handles GET /{tenantID}/pos/reports/summary
// Returns today's KPI snapshot: total revenue, order count, avg ticket, active shifts, and day-over-day growth.
func (h *ReportsHandler) GetSummary(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterdayStart := todayStart.AddDate(0, 0, -1)

	var outletFilters []predicate.POSOrder
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			outletFilters = []predicate.POSOrder{posorder.OutletID(oid)}
		}
	}

	queryRevenue := func(from, to time.Time) (float64, int, error) {
		preds := append([]predicate.POSOrder{
			posorder.TenantID(tid),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLT(to),
		}, outletFilters...)
		orders, qErr := h.db.POSOrder.Query().Where(preds...).All(r.Context())
		if qErr != nil {
			return 0, 0, qErr
		}
		var total float64
		for _, o := range orders {
			total += o.TotalAmount
		}
		return total, len(orders), nil
	}

	todayRev, todayOrders, err := queryRevenue(todayStart, now)
	if err != nil {
		h.log.Error("summary: today revenue query failed", zap.Error(err))
		jsonError(w, "failed to generate summary", http.StatusInternalServerError)
		return
	}

	yesterdayRev, yesterdayOrders, err := queryRevenue(yesterdayStart, todayStart)
	if err != nil {
		h.log.Error("summary: yesterday revenue query failed", zap.Error(err))
		jsonError(w, "failed to generate summary", http.StatusInternalServerError)
		return
	}

	activeShifts, err := h.db.POSDeviceSession.Query().
		Where(
			posdevicesession.TenantID(tid),
			posdevicesession.SessionStatusEQ("open"),
		).Count(r.Context())
	if err != nil {
		h.log.Warn("summary: active sessions query failed", zap.Error(err))
		activeShifts = 0
	}

	var avgTicket float64
	if todayOrders > 0 {
		avgTicket = todayRev / float64(todayOrders)
	}

	revenueGrowth := 0.0
	ordersGrowth := 0.0
	if yesterdayRev > 0 {
		revenueGrowth = (todayRev - yesterdayRev) / yesterdayRev * 100
	}
	if yesterdayOrders > 0 {
		ordersGrowth = float64(todayOrders-yesterdayOrders) / float64(yesterdayOrders) * 100
	}

	jsonOK(w, map[string]any{
		"total_revenue":    todayRev,
		"total_orders":     todayOrders,
		"avg_ticket":       avgTicket,
		"active_staff":     activeShifts,
		"revenue_growth":   revenueGrowth,
		"orders_growth":    ordersGrowth,
		"as_of":            now.Format(time.RFC3339),
	})
}

// SalesSummary handles GET /{tenantID}/pos/reports/sales-summary
// Query params: from (RFC3339 or YYYY-MM-DD), to, outlet_id (optional)
func (h *ReportsHandler) SalesSummary(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)

	q := h.db.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLTE(to),
		)

	if outletStr := r.URL.Query().Get("outlet_id"); outletStr != "" {
		if oid, err2 := uuid.Parse(outletStr); err2 == nil {
			q = q.Where(posorder.OutletID(oid))
		}
	}

	orders, err := q.All(r.Context())
	if err != nil {
		h.log.Error("sales summary query failed", zap.Error(err))
		jsonError(w, "failed to generate sales summary", http.StatusInternalServerError)
		return
	}

	var totalRevenue, totalTax, totalDiscount float64
	for _, o := range orders {
		totalRevenue += o.TotalAmount
		totalTax += o.TaxTotal
		totalDiscount += o.DiscountTotal
	}

	var avgOrderValue float64
	if len(orders) > 0 {
		avgOrderValue = totalRevenue / float64(len(orders))
	}

	jsonOK(w, map[string]any{
		"from":            from.Format(time.RFC3339),
		"to":              to.Format(time.RFC3339),
		"order_count":     len(orders),
		"total_revenue":   totalRevenue,
		"total_tax":       totalTax,
		"total_discount":  totalDiscount,
		"avg_order_value": avgOrderValue,
	})
}

// RefundSummary handles GET /{tenantID}/pos/reports/refund-summary
// Filters by occurred_at range; joined through orders for tenant scoping.
func (h *ReportsHandler) RefundSummary(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)

	// Scope to this tenant's orders
	orderIDs, err := h.db.POSOrder.Query().
		Where(posorder.TenantID(tid)).
		IDs(r.Context())
	if err != nil {
		h.log.Error("refund summary: order id query failed", zap.Error(err))
		jsonError(w, "failed to generate refund summary", http.StatusInternalServerError)
		return
	}

	refunds, err := h.db.POSRefund.Query().
		Where(
			posrefund.OrderIDIn(orderIDs...),
			posrefund.OccurredAtGTE(from),
			posrefund.OccurredAtLTE(to),
		).
		All(r.Context())
	if err != nil {
		h.log.Error("refund summary query failed", zap.Error(err))
		jsonError(w, "failed to generate refund summary", http.StatusInternalServerError)
		return
	}

	var totalRefunded float64
	for _, ref := range refunds {
		totalRefunded += ref.Amount
	}

	jsonOK(w, map[string]any{
		"from":           from.Format(time.RFC3339),
		"to":             to.Format(time.RFC3339),
		"refund_count":   len(refunds),
		"total_refunded": totalRefunded,
	})
}

// DailyBreakdown handles GET /{tenantID}/pos/reports/daily-breakdown
// Returns per-day revenue totals within the date range.
func (h *ReportsHandler) DailyBreakdown(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)

	orders, err := h.db.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLTE(to),
		).
		All(r.Context())
	if err != nil {
		h.log.Error("daily breakdown query failed", zap.Error(err))
		jsonError(w, "failed to generate daily breakdown", http.StatusInternalServerError)
		return
	}

	// Granularity drill-down: day | week | month | quarter | semiannual | year.
	// "bi_quarter"/"biannual" are accepted aliases for the 6-month (semiannual) bucket.
	gran := normalizeGranularity(r.URL.Query().Get("granularity"))

	type dayBucket struct {
		Revenue    float64
		OrderCount int
	}
	buckets := make(map[string]*dayBucket)
	for _, o := range orders {
		key := granularityBucketStart(o.CreatedAt.UTC(), gran).Format("2006-01-02")
		if _, ok := buckets[key]; !ok {
			buckets[key] = &dayBucket{}
		}
		buckets[key].Revenue += o.TotalAmount
		buckets[key].OrderCount++
	}

	type dayRow struct {
		Date        string  `json:"date"`
		Granularity string  `json:"granularity"`
		Revenue     float64 `json:"revenue"`
		OrderCount  int     `json:"order_count"`
	}
	var rows []dayRow
	// Walk the range one bucket at a time so gaps render as zero-revenue points.
	cur := granularityBucketStart(from.UTC(), gran)
	end := to.UTC()
	for !cur.After(end) {
		key := cur.Format("2006-01-02")
		b := buckets[key]
		row := dayRow{Date: key, Granularity: gran}
		if b != nil {
			row.Revenue = b.Revenue
			row.OrderCount = b.OrderCount
		}
		rows = append(rows, row)
		cur = granularityNext(cur, gran)
	}

	jsonOK(w, rows)
}

// normalizeGranularity maps the granularity query param onto a canonical bucket name,
// defaulting to "day". Accepts common aliases (weekly, monthly, bi_quarter, biannual…).
func normalizeGranularity(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "week", "weekly":
		return "week"
	case "month", "monthly":
		return "month"
	case "quarter", "quarterly":
		return "quarter"
	case "semiannual", "semi_annual", "semi-annual", "biannual", "bi_annual", "bi_quarter", "bi-quarter", "biquarter", "half_year", "halfyear":
		return "semiannual"
	case "year", "yearly", "annual", "annually":
		return "year"
	default:
		return "day"
	}
}

// granularityBucketStart returns the start of the bucket that t falls into for a granularity.
func granularityBucketStart(t time.Time, gran string) time.Time {
	y, m, d := t.Date()
	loc := t.Location()
	switch gran {
	case "week":
		// ISO-ish week starting Monday.
		offset := (int(t.Weekday()) + 6) % 7 // Monday=0
		return time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, -offset)
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	case "quarter":
		qStartMonth := time.Month((int(m)-1)/3*3 + 1)
		return time.Date(y, qStartMonth, 1, 0, 0, 0, 0, loc)
	case "semiannual":
		hStartMonth := time.Month(1)
		if int(m) > 6 {
			hStartMonth = time.Month(7)
		}
		return time.Date(y, hStartMonth, 1, 0, 0, 0, 0, loc)
	case "year":
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc)
	default: // day
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
}

// granularityNext advances a bucket-start time to the next bucket-start for a granularity.
func granularityNext(t time.Time, gran string) time.Time {
	switch gran {
	case "week":
		return t.AddDate(0, 0, 7)
	case "month":
		return t.AddDate(0, 1, 0)
	case "quarter":
		return t.AddDate(0, 3, 0)
	case "semiannual":
		return t.AddDate(0, 6, 0)
	case "year":
		return t.AddDate(1, 0, 0)
	default:
		return t.AddDate(0, 0, 1)
	}
}

// TopItems handles GET /{tenantID}/pos/reports/top-items
// Returns the top-selling items by quantity and revenue in the date range.
// Query params: from, to, limit (default 20)
func (h *ReportsHandler) TopItems(w http.ResponseWriter, r *http.Request) {
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
		h.log.Error("top items query failed", zap.Error(err))
		jsonError(w, "failed to generate top items report", http.StatusInternalServerError)
		return
	}

	type itemBucket struct {
		Name     string  `json:"name"`
		SKU      string  `json:"sku"`
		Quantity float64 `json:"quantity_sold"`
		Revenue  float64 `json:"revenue"`
	}
	buckets := make(map[string]*itemBucket)
	for _, o := range orders {
		for _, l := range o.Edges.Lines {
			if _, ok := buckets[l.Sku]; !ok {
				buckets[l.Sku] = &itemBucket{SKU: l.Sku, Name: l.Name}
			}
			buckets[l.Sku].Quantity += l.Quantity
			buckets[l.Sku].Revenue += l.TotalPrice
		}
	}

	rows := make([]*itemBucket, 0, len(buckets))
	for _, b := range buckets {
		rows = append(rows, b)
	}
	// sort by revenue descending
	for i := 0; i < len(rows)-1; i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].Revenue > rows[i].Revenue {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}

	jsonOK(w, rows)
}

// S2SSalesBySKU handles GET /api/v1/s2s/{tenant}/pos/sales/by-sku?from=&to=
// It returns POS units sold per SKU for completed orders in the range. Other services (e.g.
// inventory-api menu-engineering / variance) merge this with their own sales sources so POS-driven
// sales are counted, not just ordering-service orders. Authenticated by the internal service key.
func (h *ReportsHandler) S2SSalesBySKU(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	from, to := parseDateRange(r)

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
		h.log.Error("s2s sales-by-sku query failed", zap.Error(err))
		jsonError(w, "failed to aggregate sales", http.StatusInternalServerError)
		return
	}

	type skuRow struct {
		SKU          string  `json:"sku"`
		Name         string  `json:"name"`
		QuantitySold float64 `json:"quantity_sold"`
		Revenue      float64 `json:"revenue"`
	}
	buckets := make(map[string]*skuRow)
	for _, o := range orders {
		for _, l := range o.Edges.Lines {
			if l.Sku == "" {
				continue
			}
			if _, ok := buckets[l.Sku]; !ok {
				buckets[l.Sku] = &skuRow{SKU: l.Sku, Name: l.Name}
			}
			buckets[l.Sku].QuantitySold += l.Quantity
			buckets[l.Sku].Revenue += l.TotalPrice
		}
	}
	rows := make([]*skuRow, 0, len(buckets))
	for _, b := range buckets {
		rows = append(rows, b)
	}
	jsonOK(w, map[string]any{
		"from": from.Format("2006-01-02"),
		"to":   to.Format("2006-01-02"),
		"data": rows,
	})
}

// SalesByStaff handles GET /{tenantID}/pos/reports/sales-by-staff
// Groups orders by user_id: completed revenue + void_count + discount_total + avg_order_value.
func (h *ReportsHandler) SalesByStaff(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)

	completed, err := h.db.POSOrder.Query().
		Where(posorder.TenantID(tid), posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(from), posorder.CreatedAtLTE(to)).
		All(r.Context())
	if err != nil {
		h.log.Error("sales by staff (completed) query failed", zap.Error(err))
		jsonError(w, "failed to generate sales-by-staff report", http.StatusInternalServerError)
		return
	}
	voided, err := h.db.POSOrder.Query().
		Where(posorder.TenantID(tid), posorder.StatusEQ("voided"),
			posorder.CreatedAtGTE(from), posorder.CreatedAtLTE(to)).
		All(r.Context())
	if err != nil {
		h.log.Error("sales by staff (voided) query failed", zap.Error(err))
		jsonError(w, "failed to generate sales-by-staff report", http.StatusInternalServerError)
		return
	}

	type staffBucket struct {
		UserID        uuid.UUID `json:"user_id"`
		StaffName     string    `json:"staff_name"`
		OrderCount    int       `json:"order_count"`
		Revenue       float64   `json:"revenue"`
		DiscountTotal float64   `json:"discount_total"`
		VoidCount     int       `json:"void_count"`
		AvgOrderValue float64   `json:"avg_order_value"`
	}
	buckets := make(map[uuid.UUID]*staffBucket)
	for _, o := range completed {
		if _, ok := buckets[o.UserID]; !ok {
			buckets[o.UserID] = &staffBucket{UserID: o.UserID}
		}
		buckets[o.UserID].OrderCount++
		buckets[o.UserID].Revenue += o.TotalAmount
		buckets[o.UserID].DiscountTotal += o.DiscountTotal
	}
	for _, o := range voided {
		if _, ok := buckets[o.UserID]; !ok {
			buckets[o.UserID] = &staffBucket{UserID: o.UserID}
		}
		buckets[o.UserID].VoidCount++
	}
	for _, b := range buckets {
		if b.OrderCount > 0 {
			b.AvgOrderValue = b.Revenue / float64(b.OrderCount)
		}
	}

	// Enrich each row with the staff member's name so the UI shows a human name, not a UUID.
	ids := make([]uuid.UUID, 0, len(buckets))
	for id := range buckets {
		ids = append(ids, id)
	}
	names := h.resolveStaffNames(r.Context(), tid, ids)
	for id, b := range buckets {
		if n := names[id]; n != "" {
			b.StaffName = n
		} else {
			b.StaffName = "Unknown"
		}
	}

	rows := make([]*staffBucket, 0, len(buckets))
	for _, b := range buckets {
		rows = append(rows, b)
	}
	for i := 0; i < len(rows)-1; i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].Revenue > rows[i].Revenue {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}

	jsonOK(w, rows)
}

// resolveStaffNames maps POS order user_ids to human staff names. Order.user_id is the auth
// service user id (JWT subject); the local User projection carries full_name keyed by BOTH
// its own id and auth_service_user_id, so match on either. Returns id → name (best effort;
// missing ids are simply absent from the map).
func (h *ReportsHandler) resolveStaffNames(ctx context.Context, tid uuid.UUID, ids []uuid.UUID) map[uuid.UUID]string {
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	users, err := h.db.User.Query().
		Where(
			entuser.TenantID(tid),
			entuser.Or(entuser.IDIn(ids...), entuser.AuthServiceUserIDIn(ids...)),
		).
		All(ctx)
	if err != nil {
		h.log.Warn("resolve staff names failed", zap.Error(err))
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

// ExportDailyReport handles GET /{tenantID}/pos/reports/export
// Streams a CSV of daily sales totals for the requested date range.
// Query params: from, to (YYYY-MM-DD or RFC3339), format (default "csv")
func (h *ReportsHandler) ExportDailyReport(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)

	orders, err := h.db.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLTE(to),
		).
		All(r.Context())
	if err != nil {
		h.log.Error("export report query failed", zap.Error(err))
		jsonError(w, "failed to export report", http.StatusInternalServerError)
		return
	}

	type dayRow struct {
		Date       string
		OrderCount int
		Revenue    float64
		Tax        float64
		Net        float64
	}

	buckets := make(map[string]*dayRow)
	for _, o := range orders {
		day := o.CreatedAt.UTC().Format("2006-01-02")
		if _, ok := buckets[day]; !ok {
			buckets[day] = &dayRow{Date: day}
		}
		buckets[day].OrderCount++
		buckets[day].Revenue += o.TotalAmount
		buckets[day].Tax += o.TaxTotal
		buckets[day].Net += o.Subtotal
	}

	// Build rows in date order
	var rows []*dayRow
	cur := from
	for !cur.After(to) {
		day := cur.UTC().Format("2006-01-02")
		if b, ok := buckets[day]; ok {
			rows = append(rows, b)
		} else {
			rows = append(rows, &dayRow{Date: day})
		}
		cur = cur.AddDate(0, 0, 1)
	}

	filename := fmt.Sprintf("sales-%s-%s.csv", from.Format("2006-01-02"), to.Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Date", "Orders", "Net Sales (KES)", "VAT (KES)", "Gross Revenue (KES)"})
	for _, row := range rows {
		_ = cw.Write([]string{
			row.Date,
			strconv.Itoa(row.OrderCount),
			fmt.Sprintf("%.2f", row.Net),
			fmt.Sprintf("%.2f", row.Tax),
			fmt.Sprintf("%.2f", row.Revenue),
		})
	}
	cw.Flush()
}

// ShiftReport handles GET /{tenantID}/pos/reports/shifts/{sessionID}
// Returns a per-session (device shift) sales summary: order count, revenue, tender breakdown, refunds, voids.
func (h *ReportsHandler) ShiftReport(w http.ResponseWriter, r *http.Request) {
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

	session, err := h.db.POSDeviceSession.Get(r.Context(), sessionID)
	if err != nil || session.TenantID != tid {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}

	orders, err := h.db.POSOrder.Query().
		Where(posorder.TenantID(tid), posorder.DeviceID(session.DeviceID), posorder.StatusEQ("completed")).
		All(r.Context())
	if err != nil {
		h.log.Error("shift report orders query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Collect order IDs so we can query refunds (POSRefund has no tenant_id/session_id).
	orderIDs := make([]uuid.UUID, 0, len(orders))
	for _, o := range orders {
		orderIDs = append(orderIDs, o.ID)
	}
	var refunds []*ent.POSRefund
	if len(orderIDs) > 0 {
		refunds, err = h.db.POSRefund.Query().
			Where(posrefund.OrderIDIn(orderIDs...)).
			All(r.Context())
		if err != nil {
			h.log.Error("shift report refunds query failed", zap.Error(err))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	totalRevenue, totalTax, totalDiscount := 0.0, 0.0, 0.0
	for _, o := range orders {
		totalRevenue += o.TotalAmount
		totalTax += o.TaxTotal
		totalDiscount += o.DiscountTotal
	}
	totalRefunds := 0.0
	for _, ref := range refunds {
		totalRefunds += ref.Amount
	}

	jsonOK(w, map[string]any{
		"session_id":      sessionID,
		"device_id":       session.DeviceID,
		"started_at":      session.OpenedAt,
		"ended_at":        session.ClosedAt,
		"order_count":     len(orders),
		"total_revenue":   totalRevenue,
		"total_tax":       totalTax,
		"total_discounts": totalDiscount,
		"total_refunds":   totalRefunds,
		"net_sales":       totalRevenue - totalRefunds,
		"opening_cash":    session.FloatAmount,
	})
}

// ShiftReportList handles GET /{tenantID}/pos/reports/shifts
// Lists all device sessions with summary stats. Filter: device_id, from, to.
func (h *ReportsHandler) ShiftReportList(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)
	q := h.db.POSDeviceSession.Query().
		Where(posdevicesession.TenantID(tid),
			posdevicesession.OpenedAtGTE(from),
			posdevicesession.OpenedAtLTE(to))

	if devID := r.URL.Query().Get("device_id"); devID != "" {
		if did, parseErr := uuid.Parse(devID); parseErr == nil {
			q = q.Where(posdevicesession.DeviceID(did))
		}
	}

	sessions, err := q.Order(ent.Desc(posdevicesession.FieldOpenedAt)).All(r.Context())
	if err != nil {
		h.log.Error("shift list query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": sessions, "total": len(sessions)})
}

// CommissionReport handles GET /{tenantID}/pos/reports/commissions
// Returns unpaid (pending) commission totals per staff member.
func (h *ReportsHandler) CommissionReport(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)
	recs, err := h.db.CommissionRecord.Query().
		Where(
			entcommrec.TenantID(tid),
			entcommrec.StatusEQ("pending"),
			entcommrec.CreatedAtGTE(from),
			entcommrec.CreatedAtLTE(to),
		).
		All(r.Context())
	if err != nil {
		h.log.Error("commission report query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type staffSummary struct {
		StaffID         uuid.UUID `json:"staff_member_id"`
		PendingAmount   float64   `json:"pending_amount"`
		RecordCount     int       `json:"record_count"`
	}
	byStaff := make(map[uuid.UUID]*staffSummary)
	for _, rec := range recs {
		if _, ok := byStaff[rec.StaffMemberID]; !ok {
			byStaff[rec.StaffMemberID] = &staffSummary{StaffID: rec.StaffMemberID}
		}
		byStaff[rec.StaffMemberID].PendingAmount += rec.CommissionAmount
		byStaff[rec.StaffMemberID].RecordCount++
	}

	rows := make([]*staffSummary, 0, len(byStaff))
	for _, s := range byStaff {
		rows = append(rows, s)
	}
	jsonOK(w, map[string]any{"data": rows, "total": len(rows)})
}

// TaxReport handles GET /{tenantID}/pos/reports/tax
// Returns tax collected by date range grouped by KRA code + VAT rate (eTIMS-ready).
func (h *ReportsHandler) TaxReport(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)

	lines, err := h.db.POSOrderLine.Query().
		Where(posorderline.HasOrderWith(
			posorder.TenantID(tid),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLTE(to),
		)).
		All(r.Context())
	if err != nil {
		h.log.Error("tax report query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type taxBucket struct {
		KRACode       string  `json:"kra_code"`
		TaxRate       float64 `json:"tax_rate"`
		TaxableAmount float64 `json:"taxable_amount"`
		TaxAmount     float64 `json:"tax_amount"`
	}
	type bucketKey struct {
		code string
		rate float64
	}
	buckets := make(map[bucketKey]*taxBucket)
	totalTaxable, totalTax := 0.0, 0.0

	for _, l := range lines {
		rate := 0.0
		if l.TaxRate != nil {
			rate = *l.TaxRate
		}
		lineTotal := l.UnitPrice * float64(l.Quantity)
		taxAmt := lineTotal * (rate / 100)
		k := bucketKey{code: l.TaxKraCode, rate: rate}
		if _, ok := buckets[k]; !ok {
			buckets[k] = &taxBucket{KRACode: l.TaxKraCode, TaxRate: rate}
		}
		buckets[k].TaxableAmount += lineTotal
		buckets[k].TaxAmount += taxAmt
		totalTaxable += lineTotal
		totalTax += taxAmt
	}

	breakdown := make([]*taxBucket, 0, len(buckets))
	for _, b := range buckets {
		breakdown = append(breakdown, b)
	}

	jsonOK(w, map[string]any{
		"from":                from.Format("2006-01-02"),
		"to":                  to.Format("2006-01-02"),
		"total_taxable_sales": totalTaxable,
		"total_tax_amount":    totalTax,
		"breakdown":           breakdown,
	})
}

// SalesByHour handles GET /{tenantID}/pos/reports/sales/by-hour?date=YYYY-MM-DD
// Returns hourly sales breakdown for a single day.
func (h *ReportsHandler) SalesByHour(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

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

	orders, err := h.db.POSOrder.Query().
		Where(posorder.TenantID(tid), posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(dayStart), posorder.CreatedAtLT(dayEnd)).
		All(r.Context())
	if err != nil {
		h.log.Error("by-hour query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type hourBucket struct {
		Hour       int     `json:"hour"`
		OrderCount int     `json:"order_count"`
		Revenue    float64 `json:"revenue"`
	}
	buckets := make([]hourBucket, 24)
	for i := range buckets {
		buckets[i].Hour = i
	}
	for _, o := range orders {
		h := o.CreatedAt.UTC().Hour()
		buckets[h].OrderCount++
		buckets[h].Revenue += o.TotalAmount
	}

	jsonOK(w, map[string]any{"date": dateStr, "hours": buckets})
}

// SalesByCategory handles GET /{tenantID}/pos/reports/sales/by-category
// Returns revenue and order count grouped by catalog item category.
func (h *ReportsHandler) SalesByCategory(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)

	// POSOrderLine has no tenant_id/created_at — filter through completed orders.
	orders, err := h.db.POSOrder.Query().
		Where(posorder.TenantID(tid),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLTE(to)).
		WithLines().
		All(r.Context())
	if err != nil {
		h.log.Error("by-category query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type catBucket struct {
		Category string  `json:"category"`
		Revenue  float64 `json:"revenue"`
		Quantity float64 `json:"quantity"`
	}
	byCategory := make(map[string]*catBucket)
	for _, o := range orders {
		for _, line := range o.Edges.Lines {
			cat, _ := line.Metadata["category"].(string)
			if cat == "" {
				cat = "Uncategorised"
			}
			if _, ok := byCategory[cat]; !ok {
				byCategory[cat] = &catBucket{Category: cat}
			}
			byCategory[cat].Revenue += line.TotalPrice
			byCategory[cat].Quantity += line.Quantity
		}
	}

	rows := make([]*catBucket, 0, len(byCategory))
	for _, b := range byCategory {
		rows = append(rows, b)
	}
	jsonOK(w, map[string]any{"data": rows, "total": len(rows)})
}

// StockConsumptionReport handles GET /{tenantID}/pos/reports/stock-consumption
// Returns per-SKU consumed quantities for completed orders in the requested date range.
func (h *ReportsHandler) StockConsumptionReport(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	from, to := parseDateRange(r)

	lines, err := h.db.POSOrderLine.Query().
		Where(posorderline.HasOrderWith(
			posorder.TenantID(tid),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLT(to),
		)).
		All(r.Context())
	if err != nil {
		h.log.Error("stock consumption report query failed", zap.Error(err))
		jsonError(w, "failed to query stock consumption", http.StatusInternalServerError)
		return
	}

	type skuRow struct {
		SKU      string  `json:"sku"`
		Name     string  `json:"name"`
		Quantity float64 `json:"quantity"`
	}
	bysku := map[string]*skuRow{}
	for _, l := range lines {
		if row, ok := bysku[l.Sku]; ok {
			row.Quantity += l.Quantity
		} else {
			bysku[l.Sku] = &skuRow{SKU: l.Sku, Name: l.Name, Quantity: l.Quantity}
		}
	}
	rows := make([]*skuRow, 0, len(bysku))
	for _, r := range bysku {
		rows = append(rows, r)
	}
	jsonOK(w, map[string]any{"data": rows, "from": from, "to": to})
}

// ReturnsSummary handles GET /{tenantID}/pos/reports/returns
// Returns a summary of returns in the requested date range grouped by reason.
func (h *ReportsHandler) ReturnsSummary(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	from, to := parseDateRange(r)

	returns, err := h.db.POSReturn.Query().
		Where(
			posreturn.TenantID(tid),
			posreturn.CreatedAtGTE(from),
			posreturn.CreatedAtLT(to),
		).
		All(r.Context())
	if err != nil {
		h.log.Error("returns report query failed", zap.Error(err))
		jsonError(w, "failed to query returns", http.StatusInternalServerError)
		return
	}

	type reasonRow struct {
		Reason      string  `json:"reason"`
		Count       int     `json:"count"`
		TotalRefund float64 `json:"total_refund"`
	}
	byReason := map[string]*reasonRow{}
	totalCount := 0
	totalRefund := 0.0
	for _, ret := range returns {
		totalCount++
		totalRefund += ret.RefundAmount
		key := ret.Reason
		if key == "" {
			key = "other"
		}
		if row, ok := byReason[key]; ok {
			row.Count++
			row.TotalRefund += ret.RefundAmount
		} else {
			byReason[key] = &reasonRow{Reason: key, Count: 1, TotalRefund: ret.RefundAmount}
		}
	}
	rows := make([]*reasonRow, 0, len(byReason))
	for _, r := range byReason {
		rows = append(rows, r)
	}
	jsonOK(w, map[string]any{
		"data":         rows,
		"total_count":  totalCount,
		"total_refund": totalRefund,
		"from":         from,
		"to":           to,
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

func parseDateRange(r *http.Request) (from, to time.Time) {
	now := time.Now().UTC()
	from = now.AddDate(0, 0, -30)
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
	return
}
