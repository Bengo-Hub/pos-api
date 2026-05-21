package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posrefund"
)

type ReportsHandler struct {
	log *zap.Logger
	db  *ent.Client
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

	queryRevenue := func(from, to time.Time) (float64, int, error) {
		orders, qErr := h.db.POSOrder.Query().
			Where(
				posorder.TenantID(tid),
				posorder.StatusEQ("completed"),
				posorder.CreatedAtGTE(from),
				posorder.CreatedAtLT(to),
			).All(r.Context())
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

	type dayBucket struct {
		Revenue    float64
		OrderCount int
	}
	buckets := make(map[string]*dayBucket)
	for _, o := range orders {
		day := o.CreatedAt.UTC().Format("2006-01-02")
		if _, ok := buckets[day]; !ok {
			buckets[day] = &dayBucket{}
		}
		buckets[day].Revenue += o.TotalAmount
		buckets[day].OrderCount++
	}

	type dayRow struct {
		Date       string  `json:"date"`
		Revenue    float64 `json:"revenue"`
		OrderCount int     `json:"order_count"`
	}
	var rows []dayRow
	cur := from
	for !cur.After(to) {
		day := cur.UTC().Format("2006-01-02")
		b := buckets[day]
		row := dayRow{Date: day}
		if b != nil {
			row.Revenue = b.Revenue
			row.OrderCount = b.OrderCount
		}
		rows = append(rows, row)
		cur = cur.AddDate(0, 0, 1)
	}

	jsonOK(w, rows)
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

// SalesByStaff handles GET /{tenantID}/pos/reports/sales-by-staff
// Groups completed orders by user_id and sums revenue.
func (h *ReportsHandler) SalesByStaff(w http.ResponseWriter, r *http.Request) {
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
		h.log.Error("sales by staff query failed", zap.Error(err))
		jsonError(w, "failed to generate sales-by-staff report", http.StatusInternalServerError)
		return
	}

	type staffBucket struct {
		UserID     uuid.UUID `json:"user_id"`
		OrderCount int       `json:"order_count"`
		Revenue    float64   `json:"revenue"`
	}
	buckets := make(map[uuid.UUID]*staffBucket)
	for _, o := range orders {
		if _, ok := buckets[o.UserID]; !ok {
			buckets[o.UserID] = &staffBucket{UserID: o.UserID}
		}
		buckets[o.UserID].OrderCount++
		buckets[o.UserID].Revenue += o.TotalAmount
	}

	rows := make([]*staffBucket, 0, len(buckets))
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

	jsonOK(w, rows)
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
