package handlers

import (
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// effectiveDateGTE/effectiveDateLTE filter POSOrder by its EFFECTIVE reporting date: the
// admin business_date override (see orders.Service.MoveOrderDate) when set, else the offline
// device-clock time (offline_created_at) for an order rung up offline and synced later, else
// created_at. This is the query-level counterpart of orders.EffectiveOrderDate — use it
// anywhere a report buckets/filters orders by date so a moved (or offline-delayed) sale
// reports under its true day instead of its server-ingestion/sync day.
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
		posorder.And(posorder.BusinessDateIsNil(), posorder.OfflineCreatedAtNotNil(), posorder.OfflineCreatedAtGTE(t)),
		posorder.And(posorder.BusinessDateIsNil(), posorder.OfflineCreatedAtIsNil(), posorder.CreatedAtGTE(t)),
	)
}

func effectiveDateLTE(t time.Time) predicate.POSOrder {
	return posorder.Or(
		posorder.And(posorder.BusinessDateNotNil(), posorder.BusinessDateLTE(t)),
		posorder.And(posorder.BusinessDateIsNil(), posorder.OfflineCreatedAtNotNil(), posorder.OfflineCreatedAtLTE(t)),
		posorder.And(posorder.BusinessDateIsNil(), posorder.OfflineCreatedAtIsNil(), posorder.CreatedAtLTE(t)),
	)
}

func effectiveDateLT(t time.Time) predicate.POSOrder {
	return posorder.Or(
		posorder.And(posorder.BusinessDateNotNil(), posorder.BusinessDateLT(t)),
		posorder.And(posorder.BusinessDateIsNil(), posorder.OfflineCreatedAtNotNil(), posorder.OfflineCreatedAtLT(t)),
		posorder.And(posorder.BusinessDateIsNil(), posorder.OfflineCreatedAtIsNil(), posorder.CreatedAtLT(t)),
	)
}

// orderByEffectiveDate sorts POSOrder rows by the same effective date effectiveDateGTE/LTE
// filter on (business_date when set, else offline_created_at when set, else created_at),
// descending by default — so a backdated/postdated (business_date-moved) or offline-delayed
// sale sorts into its true chronological position among the rows actually entered on that day,
// instead of always floating to the top/bottom of the list under its real entry/sync date.
// Without this, a sale entered today but business_date-moved to last week (or rung up offline
// and only synced days later) would still sort as if it happened today, defeating the point of
// correcting its reporting date.
//
// Needs a raw SQL expression because ent's generated Asc/Desc only order by a single column, not
// a COALESCE across several — mirrors effectiveDateGTE/LTE's Or/And predicate shape, just
// expressed as an ORDER BY instead of a WHERE. A secondary "created_at DESC" term keeps
// same-effective-date rows in a stable, real-entry-order tiebreak (matters for ties and for the
// common case, where both overrides are nil and created_at already IS the effective date).
func orderByEffectiveDate(desc bool) posorder.OrderOption {
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	return func(s *sql.Selector) {
		expr := "COALESCE(" + s.C(posorder.FieldBusinessDate) + ", " + s.C(posorder.FieldOfflineCreatedAt) + ", " + s.C(posorder.FieldCreatedAt) + ")"
		s.OrderExprFunc(func(b *sql.Builder) {
			b.WriteString(expr + " " + dir + ", " + s.C(posorder.FieldCreatedAt) + " " + dir)
		})
	}
}
