package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// sqlite3Driver/init() (registers the "sqlite3" driver name) already live in held_items_test.go —
// reused here, not redeclared.

func seedOrderWithCompletedReturn(t *testing.T, client *ent.Client, tenantID uuid.UUID, refundAmount float64, channel posreturn.RefundChannel) *ent.POSOrder {
	t.Helper()
	ctx := context.Background()
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(100).SetTaxTotal(0).SetTotalAmount(100).SetPaidTotal(0).
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

// TestReturnsRollupFor_ExcludesOffsetInvoiceFromCompletedTotal is the regression test for the live
// bug found 2026-08-06: returnsRollupFor's completedTotal feeds directly into
// orders.ComputeSettlement's completedReturns parameter across every list/detail/report call site.
// An offset_invoice-channel completed return is already folded into the order's own paid_total by
// payments/ar_reconcile.go once its event lands, so completedTotal must exclude it — otherwise the
// same return gets subtracted twice (order total 116, one 40 return → amount_due read 36 instead
// of the correct 76, confirmed live). The unfiltered display fields (total/count/status) must stay
// unaffected — they never feed the owed-amount math.
func TestReturnsRollupFor_ExcludesOffsetInvoiceFromCompletedTotal(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:handlers_returnsrollup_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	tenantID := uuid.New()
	order := seedOrderWithCompletedReturn(t, client, tenantID, 40, posreturn.RefundChannelOffsetInvoice)

	agg := returnsRollupFor(context.Background(), client, zap.NewNop(), tenantID, []uuid.UUID{order.ID})[order.ID]
	if agg == nil {
		t.Fatal("expected a return aggregate for the order")
	}
	if agg.completedTotal != 0 {
		t.Errorf("completedTotal = %v, want 0 (offset_invoice is reflected via ar_reconcile/paid_total instead)", agg.completedTotal)
	}
	if agg.total != 40 || agg.count != 1 {
		t.Errorf("total/count = %v/%v, want 40/1 (display fields must stay unfiltered)", agg.total, agg.count)
	}
}

// TestReturnsRollupFor_IncludesStoreCreditInCompletedTotal confirms the fix is channel-scoped: a
// store_credit-channel return never reaches treasury AR, so it never gets reflected in paid_total
// any other way and completedTotal must still net it, exactly as before this fix.
func TestReturnsRollupFor_IncludesStoreCreditInCompletedTotal(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:handlers_returnsrollup_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	tenantID := uuid.New()
	order := seedOrderWithCompletedReturn(t, client, tenantID, 40, posreturn.RefundChannelStoreCredit)

	agg := returnsRollupFor(context.Background(), client, zap.NewNop(), tenantID, []uuid.UUID{order.ID})[order.ID]
	if agg == nil {
		t.Fatal("expected a return aggregate for the order")
	}
	if agg.completedTotal != 40 {
		t.Errorf("completedTotal = %v, want 40 (a store_credit return still needs manual netting)", agg.completedTotal)
	}
}
