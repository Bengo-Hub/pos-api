package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// arPaymentRejectStub always returns a 400 (a business-state rejection under treasury's own
// validation, e.g. "exceeds outstanding debit" — never resolves no matter how many times retried).
func arPaymentRejectStub(t *testing.T) (*treasury.Client, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"payment exceeds outstanding debit 0"}`))
	}))
	t.Cleanup(srv.Close)
	return treasury.NewClient(srv.URL, "test-key", 2*time.Second), &calls
}

// arPaymentCapturingStub always succeeds and records the exact request body + URL path segment
// sent for each call — used to assert what creditSettlementKey actually puts on the wire.
func arPaymentCapturingStub(t *testing.T) (*treasury.Client, *[]treasury.ARPaymentRequest, *[]string) {
	t.Helper()
	var bodies []treasury.ARPaymentRequest
	var urlKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body treasury.ARPaymentRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		// Path shape: /api/v1/s2s/{tenant}/ar/customers/{key}/payment
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 2 {
			urlKeys = append(urlKeys, parts[len(parts)-2])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.ARPaymentResponse{ReceiptID: uuid.NewString()})
	}))
	t.Cleanup(srv.Close)
	return treasury.NewClient(srv.URL, "test-key", 2*time.Second), &bodies, &urlKeys
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

// TestCreditSettlementReconciler_BacksOffAfter4xxRejection is the regression test for the live
// incident found 2026-09-14 (boi-enterprises): a single reconciler pass retried 81 candidates that
// treasury permanently rejects under its own current business-state validation ("exceeds
// outstanding debit 0" — the commonest cause is treasury already correctly holding this exact
// payment under a different, pre-automation reference string) and healed zero — every one of those
// 81 was destined to be retried again on the very next 2-minute tick, forever, across every pos-api
// replica independently. A second pass immediately after a 4xx must NOT call treasury again.
func TestCreditSettlementReconciler_BacksOffAfter4xxRejection(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	order := seedReconcilerOrder(t, client, "+254700000603", "Permanently Rejected Settle")

	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(5000).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-10 * time.Minute)).
		SetPaymentData(map[string]any{"method": "cash", "credit_settlement": true}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed permanently-rejected settlement payment: %v", err)
	}

	tc, calls := arPaymentRejectStub(t)
	svc.SetTreasuryClient(tc)

	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background()) // 1st pass: rejected (400), stashes backoff marker
	rec.runOnce(context.Background()) // 2nd pass, immediately after: must be backed off

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected exactly 1 treasury call (2nd pass backed off), got %d", got)
	}
	reloaded, err := client.POSPayment.Get(context.Background(), payment.ID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if reloaded.PaymentData["last_sync_attempt_at"] == nil {
		t.Error("expected last_sync_attempt_at to be stashed after the 4xx rejection")
	}
}

// TestCreditSettlementReconciler_RetriesAfter4xxRejectionOnceBackoffElapses confirms the backoff
// from TestCreditSettlementReconciler_BacksOffAfter4xxRejection is temporary, not permanent — once
// creditSettlementRetryBackoff has elapsed (simulated by backdating the stashed marker), the
// candidate is eligible again, e.g. because a later invoice raised the customer's outstanding debit
// back above zero.
func TestCreditSettlementReconciler_RetriesAfter4xxRejectionOnceBackoffElapses(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	order := seedReconcilerOrder(t, client, "+254700000604", "Eventually Retried Settle")

	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(5000).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-10 * time.Minute)).
		SetPaymentData(map[string]any{
			"method": "cash", "credit_settlement": true,
			"last_sync_attempt_at": time.Now().Add(-creditSettlementRetryBackoff - time.Minute).Format(time.RFC3339),
		}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed previously-rejected settlement payment: %v", err)
	}

	tc, calls := arPaymentStub(t, 0) // succeeds immediately this time
	svc.SetTreasuryClient(tc)
	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background())

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected the candidate to be retried once the backoff elapsed, got %d treasury calls", got)
	}
	reloaded, err := client.POSPayment.Get(context.Background(), payment.ID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if reloaded.PaymentData["treasury_receipt_id"] == nil {
		t.Error("expected treasury_receipt_id to be stashed after the successful retry")
	}
}

// TestCreditSettlementReconciler_SendsPhoneFallbackAlongsideResolvedCrmID is the regression test
// for the live incident found 2026-09-14 (boi-enterprises, KELVIN PORT): creditSettlementKey
// prefers a resolved CRM contact for the URL path, but that crm_contact_id can be one the
// customer's EXISTING treasury balance row never had (the original invoice posted phone-only,
// before any CRM contact got linked for this phone). Without also sending the phone as a fallback
// identifier, treasury's lookup fails outright with "no accounts-receivable balance found" no
// matter how many times it's retried, even though the correct balance genuinely exists. This test
// confirms the outgoing request carries BOTH: the resolved crm_contact_id in the URL path AND the
// order's phone in the body's customer_identifier field.
func TestCreditSettlementReconciler_SendsPhoneFallbackAlongsideResolvedCrmID(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	const phone = "0115897650"
	order := seedReconcilerOrder(t, client, phone, "Kelvin Port")

	crmContactID := uuid.New()
	if _, err := client.LoyaltyAccount.Create().
		SetTenantID(order.TenantID).SetCustomerPhone(phone).SetCustomerName("Kelvin Port").SetCrmContactID(crmContactID).
		Save(context.Background()); err != nil {
		t.Fatalf("seed loyalty account: %v", err)
	}

	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(3830).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-10 * time.Minute)).
		SetPaymentData(map[string]any{"method": "cash", "credit_settlement": true}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed settlement payment: %v", err)
	}

	tc, bodies, urlKeys := arPaymentCapturingStub(t)
	svc.SetTreasuryClient(tc)
	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background())

	if len(*urlKeys) != 1 {
		t.Fatalf("expected exactly 1 treasury call, got %d", len(*urlKeys))
	}
	if (*urlKeys)[0] != crmContactID.String() {
		t.Errorf("URL key = %q, want the resolved crm_contact_id %s", (*urlKeys)[0], crmContactID)
	}
	if (*bodies)[0].CustomerIdentifier != phone {
		t.Errorf("body.customer_identifier = %q, want the order's phone %q — without this fallback, "+
			"a phone-only balance row (the KELVIN PORT shape) can never be found", (*bodies)[0].CustomerIdentifier, phone)
	}
}
