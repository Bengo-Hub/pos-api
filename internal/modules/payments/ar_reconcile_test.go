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

// TestReconcileCustomerOrders_VoidedReceipt_UnreconcilesMatchingPhantomPayment is the regression
// test for a live gap: a treasury-side AR receipt VOID (arpa.VoidARReceipt) reinstates the
// customer's debt and republishes customer.balance_updated with reference "VOID-<original
// reference>", but ReconcileCustomerOrders' reduce-only design used to no-op on ANY balance
// increase — including one caused by voiding a receipt THIS SAME mechanism had previously
// phantom-paid an order down with (applyReconcileSettlement, stamped ar_reconciled=true,
// treasury_reference=<the receipt's reference>). Left unfixed, voiding a duplicate payment in
// treasury left the matching POS order's own balance due completely unchanged — the exact
// "balance due did not change on POS" symptom reported live. This asserts the mirror-image path:
// the phantom marker payment is precisely undone (matched by its own treasury_reference, never a
// blind guess) and the order's paid_total/amount_due correctly reflect the reinstated debt.
func TestReconcileCustomerOrders_VoidedReceipt_UnreconcilesMatchingPhantomPayment(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	phone := "+254700000099"

	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(1000).SetTaxTotal(0).SetTotalAmount(1000).SetPaidTotal(1000).
		SetCustomerPhone(phone).
		SetMetadata(map[string]any{"on_account": true, "credit_settled_at": "2026-09-19T00:00:00Z"}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// A real cash payment (500) + a PRIOR reconcile pass' phantom marker (500, tied to a treasury
	// receipt referenced "REF-DUP") — together they made the order look fully paid.
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(500).SetCurrency("KES").
		SetStatus(StatusCompleted).SetPaymentData(map[string]any{"method": "cash"}).
		Save(ctx); err != nil {
		t.Fatalf("seed real cash payment: %v", err)
	}
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(500).SetCurrency("KES").
		SetStatus(StatusCompleted).SetPaymentData(map[string]any{
		"method": "ar_receipt", "ar_reconciled": true, "treasury_reference": "REF-DUP",
		"reconcile_source": "treasury_balance_updated",
	}).
		Save(ctx); err != nil {
		t.Fatalf("seed phantom reconcile payment: %v", err)
	}

	// Treasury's outstanding just went back up to 500 (the duplicate receipt "REF-DUP" was voided)
	// — POS's own open total is currently 0 (order looks fully paid), so this hits the increase
	// branch, not the reduce-only one.
	report, err := svc.ReconcileCustomerOrders(ctx, ReconcileParams{
		TenantID: tenantID, CustomerIdentifier: phone, TargetOutstanding: 500, Reference: "VOID-REF-DUP",
	})
	if err != nil {
		t.Fatalf("ReconcileCustomerOrders() error = %v", err)
	}
	if len(report.OrdersTouched) != 1 {
		t.Fatalf("expected 1 order touched, got %d (%+v)", len(report.OrdersTouched), report.OrdersTouched)
	}

	reloaded, err := client.POSOrder.Get(ctx, order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloaded.PaidTotal != 500 {
		t.Errorf("order PaidTotal = %.2f, want 500 (the real cash payment only, phantom undone)", reloaded.PaidTotal)
	}
	if due := orders.ComputeSettlement(reloaded, 0).AmountDue; due != 500 {
		t.Errorf("AmountDue = %.2f, want 500 (the reinstated debt)", due)
	}
	if _, done := reloaded.Metadata["credit_settled_at"]; done {
		t.Errorf("credit_settled_at should have been cleared — the order is no longer fully paid")
	}
}

// TestReconcileCustomerOrders_GenericBalanceIncrease_NeverGuesses confirms the reduce-only
// no-op is preserved for a balance increase that ISN'T a recognizable voided-receipt reference
// (e.g. a genuine new invoice/opening-balance bump) — unreconcilePhantomPayments must never touch
// an order's payments without a precise "VOID-<reference>" match to anchor on.
func TestReconcileCustomerOrders_GenericBalanceIncrease_NeverGuesses(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	phone := "+254700000098"

	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(1000).SetTaxTotal(0).SetTotalAmount(1000).SetPaidTotal(1000).
		SetCustomerPhone(phone).
		SetMetadata(map[string]any{"on_account": true}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(1000).SetCurrency("KES").
		SetStatus(StatusCompleted).SetPaymentData(map[string]any{
		"method": "ar_receipt", "ar_reconciled": true, "treasury_reference": "REF-REAL",
	}).
		Save(ctx); err != nil {
		t.Fatalf("seed phantom reconcile payment: %v", err)
	}

	// No "VOID-" reference at all — a fresh invoice, not a void — must leave the phantom untouched.
	report, err := svc.ReconcileCustomerOrders(ctx, ReconcileParams{
		TenantID: tenantID, CustomerIdentifier: phone, TargetOutstanding: 300, Reference: "INV-NEW-001",
	})
	if err != nil {
		t.Fatalf("ReconcileCustomerOrders() error = %v", err)
	}
	if len(report.OrdersTouched) != 0 {
		t.Fatalf("expected 0 orders touched (no precise voided-receipt match), got %d", len(report.OrdersTouched))
	}
	reloaded, err := client.POSOrder.Get(ctx, order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloaded.PaidTotal != 1000 {
		t.Errorf("order PaidTotal = %.2f, want unchanged 1000 (a generic surplus is non-POS AR, never touched)", reloaded.PaidTotal)
	}
}
