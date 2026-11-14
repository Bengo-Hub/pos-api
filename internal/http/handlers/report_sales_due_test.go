package handlers

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

type dueSeed struct {
	outlet       uuid.UUID
	status       string
	total, paid  float64
	onAccount    bool
	created      time.Time
	businessDate *time.Time
}

func seedDueOrder(t *testing.T, client *ent.Client, tenantID uuid.UUID, s dueSeed) *ent.POSOrder {
	t.Helper()
	c := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(s.outlet).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus(s.status).
		SetSubtotal(s.total).SetTaxTotal(0).SetTotalAmount(s.total).SetPaidTotal(s.paid).
		SetMetadata(map[string]any{"on_account": s.onAccount}).
		SetCreatedAt(s.created)
	if s.businessDate != nil {
		c = c.SetBusinessDate(*s.businessDate)
	}
	o, err := c.Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return o
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 0.001 }

// TestOpenSalesDue_SumsAndAgesOpenSales checks that the dashboard and analytics "Sales due"
// figure is the ComputeSettlement due of every committed open sale up to asOf (returns netted,
// voided and paid sales left out, later sales left out), and that the ageing buckets follow the
// sale's effective date.
func TestOpenSalesDue_SumsAndAgesOpenSales(t *testing.T) {
	client := openSQLiteTestClient(t, "handlers_salesdue")
	ctx := context.Background()
	tid := uuid.New()
	outlet := uuid.New()
	asOf := time.Now()
	day := 24 * time.Hour

	// 100 owed, 10 days old.
	seedDueOrder(t, client, tid, dueSeed{outlet: outlet, status: "completed", total: 100, onAccount: true, created: asOf.Add(-10 * day)})
	// 150 owed, created today but its business_date is 45 days back.
	bd := asOf.Add(-45 * day)
	seedDueOrder(t, client, tid, dueSeed{outlet: outlet, status: "completed", total: 200, paid: 50, onAccount: true, created: asOf.Add(-time.Hour), businessDate: &bd})
	// 80 less a completed store-credit return of 30 = 50 owed, 100 days old.
	withReturn := seedDueOrder(t, client, tid, dueSeed{outlet: outlet, status: "completed", total: 80, onAccount: true, created: asOf.Add(-100 * day)})
	if _, err := client.POSReturn.Create().
		SetTenantID(tid).SetOrderID(withReturn.ID).SetOutletID(outlet).
		SetReturnNumber("RET-" + uuid.NewString()[:8]).SetStatus(posreturn.StatusCompleted).
		SetReturnType(posreturn.ReturnTypeRefund).SetReason("test").SetRefundAmount(30).
		SetRefundChannel(posreturn.RefundChannelStoreCredit).SetRequestedBy(uuid.New()).
		Save(ctx); err != nil {
		t.Fatalf("seed return: %v", err)
	}
	// Left out: voided, paid in full, dated after asOf.
	seedDueOrder(t, client, tid, dueSeed{outlet: outlet, status: "voided", total: 500, onAccount: true, created: asOf.Add(-5 * day)})
	// A running tab and a held order are not a debt yet (owner decision 2026-11-14).
	seedDueOrder(t, client, tid, dueSeed{outlet: outlet, status: "open", total: 900, created: asOf.Add(-2 * day)})
	seedDueOrder(t, client, tid, dueSeed{outlet: outlet, status: "pending_payment", total: 450, created: asOf.Add(-2 * day)})
	seedDueOrder(t, client, tid, dueSeed{outlet: outlet, status: "completed", total: 60, paid: 60, created: asOf.Add(-5 * day)})
	later := asOf.Add(2 * day)
	seedDueOrder(t, client, tid, dueSeed{outlet: outlet, status: "completed", total: 70, onAccount: true, created: asOf.Add(-time.Hour), businessDate: &later})
	// Another outlet: counted tenant-wide, left out when scoped.
	seedDueOrder(t, client, tid, dueSeed{outlet: uuid.New(), status: "completed", total: 40, onAccount: true, created: asOf.Add(-3 * day)})

	got, err := openSalesDue(ctx, client, zap.NewNop(), tid, []predicate.POSOrder{posorder.OutletID(outlet)}, asOf)
	if err != nil {
		t.Fatalf("openSalesDue: %v", err)
	}
	if !approxEqual(got.Amount, 300) || got.Count != 3 {
		t.Errorf("amount/count = %v/%v, want 300/3", got.Amount, got.Count)
	}
	want := salesDueAgeing{Days0To30: 100, Days31To60: 150, Days61To90: 0, Days90Plus: 50}
	if !approxEqual(got.Ageing.Days0To30, want.Days0To30) || !approxEqual(got.Ageing.Days31To60, want.Days31To60) ||
		!approxEqual(got.Ageing.Days61To90, want.Days61To90) || !approxEqual(got.Ageing.Days90Plus, want.Days90Plus) {
		t.Errorf("ageing = %+v, want %+v", got.Ageing, want)
	}

	all, err := openSalesDue(ctx, client, zap.NewNop(), tid, nil, asOf)
	if err != nil {
		t.Fatalf("openSalesDue tenant-wide: %v", err)
	}
	if !approxEqual(all.Amount, 340) || all.Count != 4 {
		t.Errorf("tenant-wide amount/count = %v/%v, want 340/4", all.Amount, all.Count)
	}
}

// TestSalesDueAgeing_Boundaries pins the bucket edges: 30 days is still 0-30, 31 is 31-60.
func TestSalesDueAgeing_Boundaries(t *testing.T) {
	day := 24 * time.Hour
	var a salesDueAgeing
	a.add(30*day, 1)
	a.add(31*day, 2)
	a.add(60*day, 4)
	a.add(61*day, 8)
	a.add(90*day, 16)
	a.add(91*day, 32)
	a.add(-day, 64)
	if a.Days0To30 != 65 || a.Days31To60 != 6 || a.Days61To90 != 24 || a.Days90Plus != 32 {
		t.Errorf("ageing = %+v", a)
	}
}
