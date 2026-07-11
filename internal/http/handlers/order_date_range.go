package handlers

import (
	"time"

	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// effectiveDateGTE/effectiveDateLTE filter POSOrder by its EFFECTIVE reporting date: the
// admin business_date override (see orders.Service.MoveOrderDate) when set, else created_at.
// This is the query-level counterpart of orders.EffectiveOrderDate — use it anywhere a report
// buckets/filters orders by date so a moved sale reports under its corrected day instead of
// its original server-ingestion day.
//
// Coverage: every SALES/revenue report now buckets by the effective date — the All-Sales list,
// dashboard/sales summaries, daily breakdown, top items, sales-by-SKU/staff/hour/category, tax,
// stock consumption, exports, product mix, most-profitable and KDS-station reports (JSON handlers
// in reports*.go and their PDF twins via ReportPDFHandler.completedOrders / the analytics docs).
// Deliberately still on raw created_at (NOT reporting-date surfaces): register-session reports
// (closings.go DailyClosing snapshot, reports_register.go, devices.go shift windows) and the
// void/reset audit reports (voidedPreds, VoidSummary) — these track when an order was physically
// rung up / voided at the till, which a business_date move must not retroactively shift.
func effectiveDateGTE(t time.Time) predicate.POSOrder {
	return posorder.Or(
		posorder.And(posorder.BusinessDateNotNil(), posorder.BusinessDateGTE(t)),
		posorder.And(posorder.BusinessDateIsNil(), posorder.CreatedAtGTE(t)),
	)
}

func effectiveDateLTE(t time.Time) predicate.POSOrder {
	return posorder.Or(
		posorder.And(posorder.BusinessDateNotNil(), posorder.BusinessDateLTE(t)),
		posorder.And(posorder.BusinessDateIsNil(), posorder.CreatedAtLTE(t)),
	)
}

func effectiveDateLT(t time.Time) predicate.POSOrder {
	return posorder.Or(
		posorder.And(posorder.BusinessDateNotNil(), posorder.BusinessDateLT(t)),
		posorder.And(posorder.BusinessDateIsNil(), posorder.CreatedAtLT(t)),
	)
}
