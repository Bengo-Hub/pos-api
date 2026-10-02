package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

func seedReturnRow(t *testing.T, db *ent.Client, tid, outlet uuid.UUID, status posreturn.Status, amount float64, sku string, qty float64) {
	t.Helper()
	ctx := context.Background()
	ret, err := db.POSReturn.Create().
		SetTenantID(tid).SetOutletID(outlet).SetOrderID(uuid.New()).
		SetReturnNumber("R-" + uuid.NewString()[:6]).
		SetReturnType(posreturn.ReturnTypeRefund).SetStatus(status).
		SetReason("Defective").SetRefundAmount(amount).SetRequestedBy(uuid.New()).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed return: %v", err)
	}
	if _, err := db.POSReturnLine.Create().SetReturnID(ret.ID).SetOrderLineID(uuid.New()).
		SetSku(sku).SetName(sku).SetQuantity(qty).SetUnitPrice(amount).SetTotalPrice(amount).
		Save(ctx); err != nil {
		t.Fatalf("seed line: %v", err)
	}
}

// Only completed returns count as refunded/returned, and the outlet filter is honored.
func TestReturnAggregates_CompletedOnlyAndOutletScoped(t *testing.T) {
	db := openSQLiteTestClient(t, "returns_agg")
	ctx := context.Background()
	tid, outA, outB := uuid.New(), uuid.New(), uuid.New()
	seedReturnRow(t, db, tid, outA, posreturn.StatusCompleted, 100, "X", 2)
	seedReturnRow(t, db, tid, outA, posreturn.StatusCompleted, 50, "X", 1)
	seedReturnRow(t, db, tid, outA, posreturn.StatusRejected, 999, "X", 9)
	seedReturnRow(t, db, tid, outA, posreturn.StatusPending, 777, "X", 7)
	seedReturnRow(t, db, tid, outB, posreturn.StatusCompleted, 30, "Y", 3)
	seedReturnRow(t, db, uuid.New(), outA, posreturn.StatusCompleted, 5000, "X", 50)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	n, amt, err := returnTotals(ctx, db, returnPreds(tid, outA, from, to, posreturn.StatusCompleted))
	if err != nil || n != 2 || amt != 150 {
		t.Fatalf("outlet A completed: want 2/150, got %d/%.2f err=%v", n, amt, err)
	}
	n, amt, _ = returnTotals(ctx, db, returnPreds(tid, uuid.Nil, from, to, posreturn.StatusCompleted))
	if n != 3 || amt != 180 {
		t.Fatalf("tenant-wide completed: want 3/180, got %d/%.2f", n, amt)
	}
	qty, err := returnedQtyBySKU(ctx, db, returnPreds(tid, uuid.Nil, from, to, posreturn.StatusCompleted))
	if err != nil || qty["X"] != 3 || qty["Y"] != 3 {
		t.Fatalf("returned qty by sku: want X=3 Y=3, got %+v err=%v", qty, err)
	}
	n, _, _ = returnTotals(ctx, db, returnPreds(tid, outA, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour), posreturn.StatusCompleted))
	if n != 0 {
		t.Fatalf("window must exclude, got %d", n)
	}
}
