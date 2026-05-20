package handlers

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
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
