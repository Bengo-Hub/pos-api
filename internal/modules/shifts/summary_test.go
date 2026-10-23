package shifts

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/returns"
)

// ── pure-Go sqlite shim (duplicated per-package, see saledelete/service_test.go) ──
type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() { sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}}) }

type fixture struct {
	t      *testing.T
	client *ent.Client
	tid    uuid.UUID
	outlet uuid.UUID
}

func (f fixture) order(user uuid.UUID, total float64, at time.Time, meta map[string]any) *ent.POSOrder {
	f.t.Helper()
	if meta == nil {
		meta = map[string]any{}
	}
	o, err := f.client.POSOrder.Create().
		SetTenantID(f.tid).SetOutletID(f.outlet).SetDeviceID(uuid.New()).SetUserID(user).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(total).SetTaxTotal(0).SetTotalAmount(total).SetPaidTotal(total).
		SetMetadata(meta).SetCreatedAt(at).
		Save(context.Background())
	if err != nil {
		f.t.Fatalf("seed order: %v", err)
	}
	return o
}

func (f fixture) payment(order uuid.UUID, method string, amount float64, collectedBy *uuid.UUID, at time.Time) {
	f.t.Helper()
	data := map[string]any{"method": method}
	if collectedBy != nil {
		data["collected_by"] = collectedBy.String()
	}
	if _, err := f.client.POSPayment.Create().
		SetOrderID(order).SetTenderID(uuid.Nil).SetAmount(amount).SetStatus("completed").
		SetPaymentData(data).SetOccurredAt(at).
		Save(context.Background()); err != nil {
		f.t.Fatalf("seed payment: %v", err)
	}
}

func (f fixture) cashReturn(order uuid.UUID, typ posreturn.ReturnType, amount float64, completedBy uuid.UUID, at time.Time, exchangeOrder *uuid.UUID, meta map[string]any) {
	f.t.Helper()
	md := map[string]any{"completed_by": completedBy.String(), "completed_at": at.UTC().Format(time.RFC3339)}
	for k, v := range meta {
		md[k] = v
	}
	b := f.client.POSReturn.Create().
		SetTenantID(f.tid).SetOutletID(f.outlet).SetOrderID(order).
		SetReturnNumber("RET-" + uuid.NewString()[:8]).SetReturnType(typ).
		SetStatus(posreturn.StatusCompleted).SetRefundAmount(amount).SetRequestedBy(completedBy).
		SetRefundChannel(posreturn.RefundChannelCash).SetMetadata(md).SetUpdatedAt(at)
	if exchangeOrder != nil {
		b = b.SetExchangeOrderID(*exchangeOrder)
	}
	if _, err := b.Save(context.Background()); err != nil {
		f.t.Fatalf("seed return: %v", err)
	}
}

func TestSummarize_ScopesCashToTheShiftUserAndWindow(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:shiftstest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	f := fixture{t: t, client: client, tid: uuid.New(), outlet: uuid.New()}

	cashier, other := uuid.New(), uuid.New()
	now := time.Now()
	in := now.Add(-30 * time.Minute)
	before := now.Add(-3 * time.Hour)
	session := &ent.POSDeviceSession{
		ID: uuid.New(), TenantID: f.tid, DeviceID: uuid.New(), UserID: cashier,
		SessionStatus: "open", OpenedAt: now.Add(-time.Hour), FloatAmount: 500,
	}

	a := f.order(cashier, 1250, in, nil)
	f.payment(a.ID, "cash", 1000, &cashier, in)        // counted
	f.payment(a.ID, "mpesa_manual", 250, &cashier, in) // mobile money, not drawer cash
	b := f.order(cashier, 300, in, nil)
	f.payment(b.ID, "cash", 300, nil, in) // legacy row: falls back to the order's cashier
	c := f.order(other, 500, in, nil)
	f.payment(c.ID, "cash", 500, &cashier, in) // cashier took the cash on someone else's order
	d := f.order(cashier, 200, in, nil)
	f.payment(d.ID, "cash", 200, &other, in) // someone else took it
	e := f.order(cashier, 700, before, nil)
	f.payment(e.ID, "cash", 700, &cashier, before) // before the shift

	f.cashReturn(a.ID, posreturn.ReturnTypeRefund, 100, cashier, in, nil, map[string]any{returns.MetaCashPaidOut: 100.0})
	newSwap := f.order(other, 0, in, map[string]any{"exchange_credit": 150.0})
	f.cashReturn(a.ID, posreturn.ReturnTypeExchange, 200, cashier, in, &newSwap.ID, map[string]any{returns.MetaCashPaidOut: 50.0})
	oldSwap := f.order(other, 0, in, map[string]any{"exchange_credit": 350.0})
	f.cashReturn(b.ID, posreturn.ReturnTypeExchange, 400, cashier, in, &oldSwap.ID, nil) // legacy: leftover 400 - 350
	f.cashReturn(b.ID, posreturn.ReturnTypeRefund, 80, cashier, in, nil, nil)            // legacy refund
	f.cashReturn(c.ID, posreturn.ReturnTypeRefund, 999, other, in, nil, map[string]any{returns.MetaCashPaidOut: 999.0})

	sum, err := Summarize(context.Background(), client, session)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	check := func(name string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 0.001 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("cash taken", sum.CashTaken, 1800)
	check("mobile money", sum.MobileMoney, 250)
	check("cash refunds", sum.CashRefunds, 280) // 100 + 50 + 50 + 80
	check("expected cash", sum.ExpectedCash, 500+1800-280)
	if sum.OrderCount != 3 {
		t.Errorf("order count = %d, want 3 (a, b, d)", sum.OrderCount)
	}
	check("revenue", sum.Revenue, 1250+300+200)
}

func TestSummarize_CountsOfflineSalesByRingUpTime(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:shiftstest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	f := fixture{t: t, client: client, tid: uuid.New(), outlet: uuid.New()}
	cashier := uuid.New()
	now := time.Now()
	closed := now.Add(-2 * time.Hour)
	session := &ent.POSDeviceSession{
		ID: uuid.New(), TenantID: f.tid, DeviceID: uuid.New(), UserID: cashier,
		SessionStatus: "closed", OpenedAt: now.Add(-5 * time.Hour), ClosedAt: &closed,
	}
	// Rung up during the shift, synced after it closed: belongs to the shift.
	o := f.order(cashier, 900, now, nil)
	rungUp := now.Add(-3 * time.Hour)
	if err := client.POSOrder.UpdateOneID(o.ID).SetOfflineCreatedAt(rungUp).Exec(context.Background()); err != nil {
		t.Fatalf("set offline time: %v", err)
	}
	sum, err := Summarize(context.Background(), client, session)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if sum.OrderCount != 1 || sum.Revenue != 900 {
		t.Fatalf("got %d orders / %v revenue, want 1 / 900", sum.OrderCount, sum.Revenue)
	}
}
