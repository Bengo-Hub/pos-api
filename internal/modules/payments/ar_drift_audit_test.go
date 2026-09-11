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
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/syncfailure"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

func seedOnAccountOrderWithCustomer(t *testing.T, client *ent.Client, tenantID uuid.UUID, phone, name string, total, paidTotal float64) *ent.POSOrder {
	t.Helper()
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(total).SetTaxTotal(0).SetTotalAmount(total).SetPaidTotal(paidTotal).
		SetCustomerPhone(phone).SetCustomerName(name).
		SetMetadata(map[string]any{"on_account": true}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return order
}

// treasuryStub serves a fixed OutstandingDebit for GetCreditTerms — enough for the drift-audit
// tests, which only exercise that one call.
func treasuryStub(t *testing.T, outstandingDebit string) *treasury.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.CreditTermsResponse{OutstandingDebit: outstandingDebit})
	}))
	t.Cleanup(srv.Close)
	return treasury.NewClient(srv.URL, "test-key", 2*time.Second)
}

// TestARDriftAudit_SafeDirection_AutoHeals confirms the scheduler self-heals via the SAME
// reduce-only ReconcileCustomerOrders the event-driven path uses when POS shows MORE owed than
// treasury — the direction that's always safe to auto-correct. This is the backstop for a lost/
// never-fired balance_updated event, not a new mechanism.
func TestARDriftAudit_SafeDirection_AutoHeals(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	tenantID := uuid.New()
	order := seedOnAccountOrderWithCustomer(t, client, tenantID, "+254700000111", "Jane Doe", 1000, 0)
	svc.SetTreasuryClient(treasuryStub(t, "400")) // treasury says only 400 is really owed

	sched := NewARDriftAuditScheduler(zap.NewNop(), svc)
	sched.run(context.Background())

	reloaded, err := client.POSOrder.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloaded.PaidTotal != 600 {
		t.Errorf("PaidTotal = %v, want 600 (reduce-only settle down to treasury's 400 outstanding on a 1000 order)", reloaded.PaidTotal)
	}
	hasFlag, _ := client.SyncFailure.Query().
		Where(syncfailure.TenantID(tenantID), syncfailure.EntityType(arDriftEntityType)).Exist(context.Background())
	if hasFlag {
		t.Error("expected no drift flag written for the safe (auto-healable) direction")
	}
}

// TestARDriftAudit_UnsafeDirection_RecordsFlagWithoutMutating is the regression test for the live
// bug class found 2026-09-10 (boi-enterprises: Mercy Busia's silently-failed settle-credit sync,
// Sammon Malaba / Solomon Ng'ethe's cross-customer misattribution) — when POS shows LESS owed than
// treasury, nothing may be auto-corrected (the surplus could be legitimate non-POS AR, or, as
// confirmed live, a real mistake needing a human decision). The order must be left untouched and a
// durable, queryable flag recorded instead.
func TestARDriftAudit_UnsafeDirection_RecordsFlagWithoutMutating(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	tenantID := uuid.New()
	order := seedOnAccountOrderWithCustomer(t, client, tenantID, "+254700000222", "John Roe", 1000, 0)
	svc.SetTreasuryClient(treasuryStub(t, "1500")) // treasury shows MORE owed than POS's own open total

	sched := NewARDriftAuditScheduler(zap.NewNop(), svc)
	sched.run(context.Background())

	reloaded, err := client.POSOrder.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloaded.PaidTotal != 0 {
		t.Errorf("PaidTotal = %v, want unchanged 0 — the unsafe direction must never mutate the order", reloaded.PaidTotal)
	}
	flag, ferr := client.SyncFailure.Query().
		Where(syncfailure.TenantID(tenantID), syncfailure.EntityType(arDriftEntityType), syncfailure.ExternalID("+254700000222")).
		Only(context.Background())
	if ferr != nil {
		t.Fatalf("expected a drift flag to be recorded: %v", ferr)
	}
	if flag.IsResolved {
		t.Error("expected the new flag to be unresolved")
	}
	payload := flag.Payload
	if payload["pos_open"] != 1000.0 || payload["treasury_balance"] != 1500.0 {
		t.Errorf("flag payload = %+v, want pos_open=1000 treasury_balance=1500", payload)
	}
}

// TestARDriftAudit_PaginatesAcrossMultipleBatches is the regression test for exactly the class of
// bug the user flagged 2026-09-11: an unbounded/off-by-one pagination loop is a ticking time bomb
// that only shows symptoms once real data volume exceeds one batch — by which point it's a
// production incident, not a code review comment. Seeds more open on-account orders than
// arDriftBatchSize (shrunk here so the test doesn't need hundreds of rows) split across several
// distinct customers, and confirms EVERY customer's orders are found and summed — not just
// whichever fit in the first page — and that the loop actually terminates.
func TestARDriftAudit_PaginatesAcrossMultipleBatches(t *testing.T) {
	old := arDriftBatchSize
	arDriftBatchSize = 2
	t.Cleanup(func() { arDriftBatchSize = old })

	svc, client := newTestPaymentsService(t)
	tenantID := uuid.New()
	// 5 customers, 1 order each (200 apiece) — batch size 2 forces 3 pages (2+2+1).
	phones := []string{"+254700000401", "+254700000402", "+254700000403", "+254700000404", "+254700000405"}
	for _, phone := range phones {
		seedOnAccountOrderWithCustomer(t, client, tenantID, phone, "Customer "+phone, 200, 0)
	}
	svc.SetTreasuryClient(treasuryStub(t, "0")) // every customer fully settled per treasury -> safe-direction heal

	sched := NewARDriftAuditScheduler(zap.NewNop(), svc)
	done := make(chan struct{})
	go func() {
		sched.run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run() did not terminate — likely an infinite pagination loop")
	}

	for _, phone := range phones {
		order, err := client.POSOrder.Query().Where(posorder.CustomerPhone(phone)).Only(context.Background())
		if err != nil {
			t.Fatalf("reload order for %s: %v", phone, err)
		}
		if order.PaidTotal != 200 {
			t.Errorf("customer %s: PaidTotal = %v, want 200 (every customer across every page must be processed, not just the first batch)", phone, order.PaidTotal)
		}
	}
}

// TestARDriftAudit_AutoResolvesOnceAgreementReturns confirms a stale flag from a drift that has
// since resolved itself (e.g. a human fixed it, or a later real payment healed it) gets closed out
// automatically, so the review queue only ever reflects currently-open problems.
func TestARDriftAudit_AutoResolvesOnceAgreementReturns(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	tenantID := uuid.New()
	_ = seedOnAccountOrderWithCustomer(t, client, tenantID, "+254700000333", "Mary Poe", 500, 0)
	stub := treasuryStub(t, "900")
	svc.SetTreasuryClient(stub)
	sched := NewARDriftAuditScheduler(zap.NewNop(), svc)
	sched.run(context.Background())

	flagExists := func() bool {
		ok, _ := client.SyncFailure.Query().
			Where(syncfailure.TenantID(tenantID), syncfailure.EntityType(arDriftEntityType), syncfailure.IsResolved(false)).
			Exist(context.Background())
		return ok
	}
	if !flagExists() {
		t.Fatal("expected a drift flag after the first pass")
	}

	// Now treasury agrees with POS (500) — simulate resolution and re-run.
	svc.SetTreasuryClient(treasuryStub(t, "500"))
	sched2 := NewARDriftAuditScheduler(zap.NewNop(), svc)
	sched2.run(context.Background())

	if flagExists() {
		t.Error("expected the stale flag to be auto-resolved once POS and treasury agree again")
	}
}
