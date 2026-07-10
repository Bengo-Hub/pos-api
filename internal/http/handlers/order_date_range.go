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
// NOT every date-ranged report in this package has been switched to these helpers yet — only
// the primary sales-record surfaces (All-Sales list, dashboard summary, sales summary, daily
// breakdown). The remaining analytics slices (top items, sales-by-staff/hour/category, tax,
// commission, stock consumption, exports) still filter on raw created_at; a moved sale will
// not yet be reflected there. Widen this list's callers if/when those need the same fix.
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
