package handlers

import (
	"context"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// salesDueAgeing splits the amount still owed on POS sales by how old each sale is, counted
// from its sale date (business_date when set, else the offline or server creation time, the
// same EffectiveOrderDate every report uses) up to the as-of time.
type salesDueAgeing struct {
	Days0To30  float64 `json:"0_30"`
	Days31To60 float64 `json:"31_60"`
	Days61To90 float64 `json:"61_90"`
	Days90Plus float64 `json:"90_plus"`
}

// salesDueTotals is the money still owed on POS sales in a scope.
type salesDueTotals struct {
	Amount float64        // sum of AmountDue
	Count  int            // sales with something still owed
	Ageing salesDueAgeing // Amount split by sale age
}

// paidBelowTotal keeps only orders whose stored paid_total is below their total. Any order
// outside it already owes 0 under ComputeSettlement (total less collected less returns, never
// negative), so it narrows the rows loaded without changing the answer.
func paidBelowTotal() predicate.POSOrder {
	return predicate.POSOrder(func(s *sql.Selector) {
		s.Where(sql.ColumnsLT(s.C(posorder.FieldPaidTotal), s.C(posorder.FieldTotalAmount)))
	})
}

// openSalesDue sums what is still owed on completed POS sales dated from `from` up to asOf. Every
// row goes through orders.ComputeSettlement with its completed returns from returnsRollupFor (one
// batched query, no per-order lookups), so it can never disagree with the sales list or the export.
//
// A zero `from` leaves the period open at the start (every sale still owed up to asOf). The
// dashboard passes the selected period so Sales due follows the same day, week, month or year as
// the other cards (owner, 2026-11-18), and reports the all-open figure alongside it. Amounts are
// today's balances (payments taken after asOf already reduce them); no historical balance is rebuilt.
//
// Only completed sales count (owner decision 2026-11-14): a running tab or a held order (open,
// pending payment) is not a debt yet, so it never shows as owed here, while All-Sales' Sell Due
// keeps its wider rule.
//
// scope carries the caller's outlet and own-sales predicates; the tenant, completed status and
// paid-below-total predicates are added here.
func openSalesDue(ctx context.Context, client *ent.Client, log *zap.Logger, tid uuid.UUID, scope []predicate.POSOrder, from, asOf time.Time) (salesDueTotals, error) {
	var out salesDueTotals
	preds := append([]predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.Status(orders.StatusCompleted),
		paidBelowTotal(),
		effectiveDateLTE(asOf),
	}, scope...)
	if !from.IsZero() {
		preds = append(preds, effectiveDateGTE(from))
	}
	list, err := client.POSOrder.Query().Where(preds...).
		Select(
			posorder.FieldID,
			posorder.FieldStatus,
			posorder.FieldTotalAmount,
			posorder.FieldPaidTotal,
			posorder.FieldMetadata,
			posorder.FieldBusinessDate,
			posorder.FieldOfflineCreatedAt,
			posorder.FieldCreatedAt,
		).
		All(ctx)
	if err != nil {
		return out, err
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, o := range list {
		ids = append(ids, o.ID)
	}
	rets := returnsRollupFor(ctx, client, log, tid, ids)
	for _, o := range list {
		var completedReturns float64
		if agg := rets[o.ID]; agg != nil {
			completedReturns = agg.completedTotal
		}
		due := orders.ComputeSettlement(o, completedReturns).AmountDue
		if due <= 0 {
			continue
		}
		out.Amount += due
		out.Count++
		out.Ageing.add(asOf.Sub(orders.EffectiveOrderDate(o)), due)
	}
	return out, nil
}

// add puts amount into the bucket for a sale of the given age. A sale dated after asOf (not
// possible through openSalesDue's own filter) falls in the youngest bucket.
func (a *salesDueAgeing) add(age time.Duration, amount float64) {
	days := int(age.Hours() / 24)
	switch {
	case days <= 30:
		a.Days0To30 += amount
	case days <= 60:
		a.Days31To60 += amount
	case days <= 90:
		a.Days61To90 += amount
	default:
		a.Days90Plus += amount
	}
}
