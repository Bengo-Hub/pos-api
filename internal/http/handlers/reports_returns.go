package handlers

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/posreturnline"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// Every report that counts returns goes through these helpers so the definition is the same
// everywhere: a return counts as refunded only once COMPLETED (pending/approved have moved no
// money or stock, rejected never will), it belongs to its own outlet, and it is dated by its
// return date (created_at, which a backdated return sets). Sums run in SQL, never by loading rows.

// returnPreds scopes returns to a tenant, an optional outlet (uuid.Nil = all) and a
// [from, to) window, optionally restricted to statuses.
func returnPreds(tid, outlet uuid.UUID, from, to time.Time, statuses ...posreturn.Status) []predicate.POSReturn {
	preds := []predicate.POSReturn{
		posreturn.TenantID(tid),
		posreturn.CreatedAtGTE(from),
		posreturn.CreatedAtLT(to),
	}
	if outlet != uuid.Nil {
		preds = append(preds, posreturn.OutletID(outlet))
	}
	if len(statuses) > 0 {
		preds = append(preds, posreturn.StatusIn(statuses...))
	}
	return preds
}

// returnTotals is COUNT(*) and SUM(refund_amount) over the matching returns.
func returnTotals(ctx context.Context, db *ent.Client, preds []predicate.POSReturn) (int, float64, error) {
	var rows []struct {
		Count  int             `json:"count"`
		Refund sql.NullFloat64 `json:"refund"`
	}
	err := db.POSReturn.Query().Where(preds...).
		Aggregate(ent.As(ent.Count(), "count"), ent.As(ent.Sum(posreturn.FieldRefundAmount), "refund")).
		Scan(ctx, &rows)
	if err != nil || len(rows) == 0 {
		return 0, 0, err
	}
	return rows[0].Count, rows[0].Refund.Float64, nil
}

// returnedQtyBySKU is SUM(quantity) per SKU over the lines of the matching returns.
func returnedQtyBySKU(ctx context.Context, db *ent.Client, preds []predicate.POSReturn) (map[string]float64, error) {
	var rows []struct {
		Sku      string          `json:"sku"`
		Quantity sql.NullFloat64 `json:"quantity"`
	}
	err := db.POSReturnLine.Query().
		Where(posreturnline.HasReturnWith(preds...)).
		GroupBy(posreturnline.FieldSku).
		Aggregate(ent.As(ent.Sum(posreturnline.FieldQuantity), "quantity")).
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Sku] = r.Quantity.Float64
	}
	return out, nil
}
