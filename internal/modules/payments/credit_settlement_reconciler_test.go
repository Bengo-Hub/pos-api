package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// arPaymentStub serves POST /ar/customers/{key}/payment, failing the first N calls (simulating a
// transient outage) then succeeding — enough to prove the reconciler actually retries rather than
// giving up after logging once.
func arPaymentStub(t *testing.T, failFirstN int32) (*treasury.Client, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= failFirstN {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.ARPaymentResponse{ReceiptID: uuid.NewString()})
	}))
	t.Cleanup(srv.Close)
	return treasury.NewClient(srv.URL, "test-key", 2*time.Second), &calls
}

// TestCreditSettlementReconciler_RetriesFailedSyncUntilItSucceeds is the regression test for the
// live 2026-09-11 boi-enterprises incident (MRS MERCY BUSIA): a real, collected credit-settlement
// payment whose treasury sync failed transiently sat permanently unreflected with only a log line
// — nothing ever retried it. The reconciler must find that exact payment shape (credit_settlement
// true, no treasury_receipt_id, outside the in-flight window) and keep retrying until it lands,
// then stop (stash treasury_receipt_id so it's never retried again).
func TestCreditSettlementReconciler_RetriesFailedSyncUntilItSucceeds(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	tenantID := uuid.New()
	if _, err := client.Tenant.Create().
		SetID(tenantID).SetName("Test Tenant").SetSlug("test-tenant-" + tenantID.String()[:8]).
		Save(context.Background()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	outlet, err := client.Outlet.Create().
		SetTenantID(tenantID).SetName("Main").SetCode("MAIN").SetTenantSlug("test-tenant").
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(outlet.ID).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(5000).SetTaxTotal(0).SetTotalAmount(5000).SetPaidTotal(5000).
		SetCustomerPhone("+254700000501").SetCustomerName("Jane Settle").
		SetMetadata(map[string]any{"on_account": true}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// The failed settlement payment — occurred 10 minutes ago (outside the 2-minute in-flight
	// window), no treasury_receipt_id (the original attempt failed).
	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(5000).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-10 * time.Minute)).
		SetPaymentData(map[string]any{"method": "cash", "credit_settlement": true}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed failed settlement payment: %v", err)
	}

	tc, calls := arPaymentStub(t, 1) // fail once, succeed on retry
	svc.SetTreasuryClient(tc)

	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background()) // 1st pass: fails (n=1), row still unsynced
	rec.runOnce(context.Background()) // 2nd pass: succeeds (n=2)

	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("expected exactly 2 treasury calls (1 failure + 1 success), got %d", got)
	}
	reloaded, err := client.POSPayment.Get(context.Background(), payment.ID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if reloaded.PaymentData["treasury_receipt_id"] == nil {
		t.Error("expected treasury_receipt_id to be stashed after the successful retry")
	}

	// A third pass must NOT call treasury again — the row is done.
	rec.runOnce(context.Background())
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("expected no further treasury calls once synced, got %d total calls", got)
	}
}

// TestCreditSettlementReconciler_SkipsPaymentsStillInFlight confirms a settlement from moments
// ago (still inside the normal request-time sync window) is left alone — the reconciler must
// never race a legitimate in-flight attempt.
func TestCreditSettlementReconciler_SkipsPaymentsStillInFlight(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	tenantID := uuid.New()
	if _, err := client.Tenant.Create().
		SetID(tenantID).SetName("Test Tenant").SetSlug("test-tenant-" + tenantID.String()[:8]).
		Save(context.Background()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	outlet, err := client.Outlet.Create().
		SetTenantID(tenantID).SetName("Main").SetCode("MAIN").SetTenantSlug("test-tenant").
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(outlet.ID).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(5000).SetTaxTotal(0).SetTotalAmount(5000).SetPaidTotal(5000).
		SetCustomerPhone("+254700000502").SetCustomerName("Recent Settle").
		SetMetadata(map[string]any{"on_account": true}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(5000).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-30 * time.Second)). // well inside the 2-min floor
		SetPaymentData(map[string]any{"method": "cash", "credit_settlement": true}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed just-now settlement payment: %v", err)
	}

	tc, calls := arPaymentStub(t, 0)
	svc.SetTreasuryClient(tc)
	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background())

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("expected the in-flight-window payment to be skipped, got %d treasury calls", got)
	}
}

// seedReconcilerOrder is the shared tenant/outlet/order scaffold every reconciler test needs.
func seedReconcilerOrder(t *testing.T, client *ent.Client, phone, name string) *ent.POSOrder {
	t.Helper()
	tenantID := uuid.New()
	if _, err := client.Tenant.Create().
		SetID(tenantID).SetName("Test Tenant").SetSlug("test-tenant-" + tenantID.String()[:8]).
		Save(context.Background()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	outlet, err := client.Outlet.Create().
		SetTenantID(tenantID).SetName("Main").SetCode("MAIN").SetTenantSlug("test-tenant").
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(outlet.ID).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(5000).SetTaxTotal(0).SetTotalAmount(5000).SetPaidTotal(5000).
		SetCustomerPhone(phone).SetCustomerName(name).
		SetMetadata(map[string]any{"on_account": true}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return order
}

// TestCreditSettlementReconciler_RetriesBackdatedPaymentDespiteOldOccurredAt is the regression
// test for the live bug found 2026-09-14 (boi-enterprises order 000803, MR ALBERT INLAW MALABA):
// a 10,610 payment was recorded TODAY but backdated to occurred_at 12 days earlier (money
// physically received on that date — a first-class, heavily-used feature, see
// credit_settlement.go's OccurredAt/backdated handling). The reconciler's original window filtered
// candidates by occurred_at >= now-7days, so ANY payment backdated more than a week — regardless
// of how recently it was actually written — silently fell outside the query and was NEVER
// retried, even once. Must now find and retry it based on when the row was actually WRITTEN
// (payment_data.recorded_at), not its business date.
func TestCreditSettlementReconciler_RetriesBackdatedPaymentDespiteOldOccurredAt(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	order := seedReconcilerOrder(t, client, "+254700000601", "Backdated Settle")

	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(5000).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-12 * 24 * time.Hour)). // backdated business date, 12 days ago
		SetPaymentData(map[string]any{
			"method": "bank", "credit_settlement": true, "backdated": true,
			"recorded_at": time.Now().Add(-3 * time.Minute).Format(time.RFC3339), // actually written 3 min ago
		}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed backdated settlement payment: %v", err)
	}

	tc, calls := arPaymentStub(t, 0)
	svc.SetTreasuryClient(tc)
	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background())

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected the backdated-but-recently-written payment to be retried despite its old occurred_at, got %d treasury calls", got)
	}
	reloaded, err := client.POSPayment.Get(context.Background(), payment.ID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if reloaded.PaymentData["treasury_receipt_id"] == nil {
		t.Error("expected treasury_receipt_id to be stashed after the retry")
	}
}

// TestCreditSettlementReconciler_SkipsRecentlyWrittenBackdatedPayment confirms the in-flight
// guard uses the same recorded_at logic — a backdated payment written 30 SECONDS ago (well inside
// the 2-minute floor) must be skipped even though its occurred_at looks old, so the reconciler
// never races the request-time sync attempt regardless of how far back the business date is.
func TestCreditSettlementReconciler_SkipsRecentlyWrittenBackdatedPayment(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	order := seedReconcilerOrder(t, client, "+254700000602", "Just-Now Backdated Settle")

	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(5000).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-12 * 24 * time.Hour)).
		SetPaymentData(map[string]any{
			"method": "bank", "credit_settlement": true, "backdated": true,
			"recorded_at": time.Now().Add(-30 * time.Second).Format(time.RFC3339),
		}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed just-written backdated payment: %v", err)
	}

	tc, calls := arPaymentStub(t, 0)
	svc.SetTreasuryClient(tc)
	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background())

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("expected the just-written backdated payment to be skipped as in-flight, got %d treasury calls", got)
	}
}
