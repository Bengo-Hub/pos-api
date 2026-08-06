package payments

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// ── pure-Go sqlite shim (duplicated per-package, see reversals/steps_test.go and
// orders/settlement_test.go — ent needs a driver registered as "sqlite3"). ──
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

func newTestPaymentsService(t *testing.T) (*Service, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:payments_arrecon_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	orderSvc := orders.NewService(client, orders.Config{DefaultCurrency: "KES"}, zap.NewNop())
	return NewService(client, orderSvc, "KES", zap.NewNop()), client
}

func seedOnAccountOrderWithReturn(t *testing.T, client *ent.Client, tenantID uuid.UUID, total, paidTotal, refundAmount float64, channel posreturn.RefundChannel) *ent.POSOrder {
	t.Helper()
	ctx := context.Background()
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(total).SetTaxTotal(0).SetTotalAmount(total).SetPaidTotal(paidTotal).
		SetMetadata(map[string]any{"on_account": true}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSReturn.Create().
		SetTenantID(tenantID).SetOrderID(order.ID).SetOutletID(order.OutletID).
		SetReturnNumber("RET-" + uuid.NewString()[:8]).SetStatus(posreturn.StatusCompleted).
		SetReturnType(posreturn.ReturnTypeRefund).SetReason("test").SetRefundAmount(refundAmount).
		SetRefundChannel(channel).SetRequestedBy(uuid.New()).
		Save(ctx); err != nil {
		t.Fatalf("seed return: %v", err)
	}
	return order
}

// TestCompletedReturnsTotal_ExcludesOffsetInvoiceChannel is the regression test for the live bug
// found 2026-08-06 on a fiscalized on-account test order: an offset_invoice-channel completed
// return reduces treasury's CustomerBalance directly, which payments/ar_reconcile.go's
// ReconcileCustomerOrders (event-driven) eventually folds into the SAME order's own paid_total via
// a non-on_account "ar_reconciled" payment row. completedReturnsTotal must not ALSO count that
// return's amount — otherwise ComputeSettlement's `total - collected - completedReturns` subtracts
// the same return twice (confirmed live: total 116, one 40 offset_invoice return → amount_due read
// 36 instead of the correct 76 once ar_reconcile had already settled the 40 into paid_total).
func TestCompletedReturnsTotal_ExcludesOffsetInvoiceChannel(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	tenantID := uuid.New()
	order := seedOnAccountOrderWithReturn(t, client, tenantID, 116, 0, 40, posreturn.RefundChannelOffsetInvoice)

	got, err := svc.completedReturnsTotal(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("completedReturnsTotal() error = %v", err)
	}
	if got != 0 {
		t.Errorf("completedReturnsTotal() = %v, want 0 (the offset_invoice return is reflected via ar_reconcile instead)", got)
	}
}

// TestCompletedReturnsTotal_IncludesStoreCreditChannel confirms the fix is channel-scoped, not a
// blanket on-account exclusion: a store_credit-channel return NEVER touches treasury AR (it grants
// a separate drawable balance instead — see treasury's ProcessRefund store_credit branch), so
// ar_reconcile never fires for it and this order's own paid_total will never reflect it any other
// way. completedReturnsTotal must still net it, exactly as before this fix.
func TestCompletedReturnsTotal_IncludesStoreCreditChannel(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	tenantID := uuid.New()
	order := seedOnAccountOrderWithReturn(t, client, tenantID, 116, 0, 40, posreturn.RefundChannelStoreCredit)

	got, err := svc.completedReturnsTotal(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("completedReturnsTotal() error = %v", err)
	}
	if got != 40 {
		t.Errorf("completedReturnsTotal() = %v, want 40 (a store_credit return still needs manual netting)", got)
	}
}
