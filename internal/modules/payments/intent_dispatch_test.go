package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// seedOrderForPayment creates the minimal POSOrder row a POSPayment's required "order" edge needs
// to exist first (mirrors ar_reconcile_test.go's seed pattern) — a bare random outlet id is enough
// when the test doesn't also need TreasuryIntentReconciler.runOnce's Outlet.Get lookup to resolve.
func seedOrderForPayment(t *testing.T, client *ent.Client, orderID uuid.UUID) {
	t.Helper()
	_, err := client.POSOrder.Create().
		SetID(orderID).
		SetTenantID(uuid.New()).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(500).SetTaxTotal(0).SetTotalAmount(500).SetPaidTotal(500).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
}

// seedOrderWithOutletForReconciler creates BOTH the POSOrder and a real Outlet row —
// TreasuryIntentReconciler.runOnce resolves the tenant slug via Outlet.Get(order.OutletID), which
// (unlike POSOrder's OutletID field alone) actually needs a matching row to exist.
func seedOrderWithOutletForReconciler(t *testing.T, client *ent.Client, orderID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenantID := uuid.New()
	if _, err := client.Tenant.Create().
		SetID(tenantID).SetName("Demo Tenant").SetSlug("demo-tenant-" + tenantID.String()[:8]).
		Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	outlet, err := client.Outlet.Create().
		SetTenantID(tenantID).SetTenantSlug("demo-tenant").SetCode("HQ").SetName("HQ Outlet").
		Save(ctx)
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	if _, err := client.POSOrder.Create().
		SetID(orderID).
		SetTenantID(tenantID).SetOutletID(outlet.ID).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(500).SetTaxTotal(0).SetTotalAmount(500).SetPaidTotal(500).
		Save(ctx); err != nil {
		t.Fatalf("seed order: %v", err)
	}
}

// seedCashPayment creates a completed cash POSPayment row with no external_reference yet —
// the exact shape CreatePaymentIntent's cash branch produces before dispatchTreasuryIntent runs.
// The caller must have already seeded a POSOrder with this orderID (POSPayment.order is required).
func seedCashPayment(t *testing.T, svc *Service, orderID uuid.UUID, method string) uuid.UUID {
	t.Helper()
	p, err := svc.client.POSPayment.Create().
		SetOrderID(orderID).
		SetTenderID(uuid.New()).
		SetAmount(500).
		SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{"method": method}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed cash payment: %v", err)
	}
	return p.ID
}

// TestRunTreasuryIntentCreate_BackfillsExternalReferenceOnSuccess verifies the deferred cash-path
// dispatch (2026-08-07 latency fix: CreateIntent moved off CreatePaymentIntent's request path for
// cash tenders) does what the removed synchronous call used to do — stamp the resolved treasury
// intent id onto the payment's external_reference — once it actually runs in the background.
func TestRunTreasuryIntentCreate_BackfillsExternalReferenceOnSuccess(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	orderID := uuid.New()
	seedOrderForPayment(t, client, orderID)
	paymentID := seedCashPayment(t, svc, orderID, "cash")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.IntentResponse{IntentID: "intent-abc123", Status: "pending"})
	}))
	defer server.Close()
	svc.SetTreasuryClient(treasury.NewClient(server.URL, "test-key", 5*time.Second))

	svc.runTreasuryIntentCreate(context.Background(), paymentID, "demo-tenant", orderID, treasury.CreateIntentRequest{
		SourceService: "pos", ReferenceID: "POS-DEMO-ABC", ReferenceType: "pos_order", Amount: 500, Currency: "KES",
	})

	got, err := client.POSPayment.Get(context.Background(), paymentID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if got.ExternalReference != "intent-abc123" {
		t.Fatalf("external_reference = %q, want backfilled intent id", got.ExternalReference)
	}
}

// TestRunTreasuryIntentCreate_NeverClobbersExistingReference guards the ExternalReferenceIsNil()
// backfill filter: a cashier-entered ref (card terminal code / M-Pesa manual code) must never be
// overwritten by a later treasury intent id, and a racing reconciler retry over an
// already-backfilled row must be a harmless no-op.
func TestRunTreasuryIntentCreate_NeverClobbersExistingReference(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	orderID := uuid.New()
	seedOrderForPayment(t, client, orderID)
	paymentID := seedCashPayment(t, svc, orderID, "card_manual")
	const cashierRef = "APPROVAL-998877"
	if err := client.POSPayment.UpdateOneID(paymentID).SetExternalReference(cashierRef).Exec(context.Background()); err != nil {
		t.Fatalf("stamp cashier ref: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.IntentResponse{IntentID: "intent-should-not-land", Status: "pending"})
	}))
	defer server.Close()
	svc.SetTreasuryClient(treasury.NewClient(server.URL, "test-key", 5*time.Second))

	svc.runTreasuryIntentCreate(context.Background(), paymentID, "demo-tenant", orderID, treasury.CreateIntentRequest{
		SourceService: "pos", ReferenceID: "POS-DEMO-DEF", ReferenceType: "pos_order", Amount: 500, Currency: "KES",
	})

	got, err := client.POSPayment.Get(context.Background(), paymentID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if got.ExternalReference != cashierRef {
		t.Fatalf("external_reference = %q, want unchanged cashier ref %q", got.ExternalReference, cashierRef)
	}
}

// TestRunTreasuryIntentCreate_TreasuryErrorThenReconcilerRetries simulates a treasury outage: the
// payment must be left exactly as CreatePaymentIntent's cash branch created it (no
// external_reference), matching TreasuryIntentReconciler.runOnce's scan predicate, and the call
// must not panic or return an error to a caller (there is none — this always runs detached). Then
// confirms the reconciler actually picks the row up and completes the backfill once treasury
// recovers.
func TestRunTreasuryIntentCreate_TreasuryErrorThenReconcilerRetries(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	orderID := uuid.New()
	seedOrderWithOutletForReconciler(t, client, orderID)
	paymentID := seedCashPayment(t, svc, orderID, "cash")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	svc.SetTreasuryClient(treasury.NewClient(server.URL, "test-key", 5*time.Second))

	svc.runTreasuryIntentCreate(context.Background(), paymentID, "demo-tenant", orderID, treasury.CreateIntentRequest{
		SourceService: "pos", ReferenceID: "POS-DEMO-GHI", ReferenceType: "pos_order", Amount: 500, Currency: "KES",
	})

	got, err := client.POSPayment.Get(context.Background(), paymentID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if got.ExternalReference != "" {
		t.Fatalf("external_reference = %q, want empty after a treasury failure", got.ExternalReference)
	}

	// Confirm the reconciler's scan predicate would actually pick this row up: back-date it past
	// the 2-minute floor and run one reconcile pass — it should call treasury again (this time
	// succeeding) and backfill.
	if err := client.POSPayment.UpdateOneID(paymentID).SetOccurredAt(time.Now().Add(-10 * time.Minute)).Exec(context.Background()); err != nil {
		t.Fatalf("back-date payment: %v", err)
	}

	calls := 0
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.IntentResponse{IntentID: "intent-from-reconciler", Status: "pending"})
	}))
	defer server2.Close()
	svc.SetTreasuryClient(treasury.NewClient(server2.URL, "test-key", 5*time.Second))

	reconciler := NewTreasuryIntentReconciler(svc, zap.NewNop())
	if err := reconciler.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected reconciler to call treasury exactly once, got %d", calls)
	}

	got2, err := client.POSPayment.Get(context.Background(), paymentID)
	if err != nil {
		t.Fatalf("reload payment after reconcile: %v", err)
	}
	if got2.ExternalReference != "intent-from-reconciler" {
		t.Fatalf("external_reference = %q, want reconciler-backfilled id", got2.ExternalReference)
	}
}

// TestTreasuryIntentReconciler_SkipsNonCashMethods guards against the reconciler ever firing a
// CreateIntent call for a tender that never wants one (on_account/complimentary/customer_credit —
// these always stamp external_reference at creation, so this is a defensive belt-and-braces check,
// not expected to trigger in practice).
func TestTreasuryIntentReconciler_SkipsNonCashMethods(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	orderID := uuid.New()
	seedOrderWithOutletForReconciler(t, client, orderID)
	paymentID := seedCashPayment(t, svc, orderID, TenderOnAccount)
	if err := client.POSPayment.UpdateOneID(paymentID).SetOccurredAt(time.Now().Add(-10 * time.Minute)).Exec(context.Background()); err != nil {
		t.Fatalf("back-date payment: %v", err)
	}

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.IntentResponse{IntentID: "should-not-be-called"})
	}))
	defer server.Close()
	svc.SetTreasuryClient(treasury.NewClient(server.URL, "test-key", 5*time.Second))

	reconciler := NewTreasuryIntentReconciler(svc, zap.NewNop())
	if err := reconciler.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected reconciler to skip on_account payment, treasury was called %d times", calls)
	}
}

