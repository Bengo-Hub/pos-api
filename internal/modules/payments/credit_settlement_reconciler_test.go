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

// arPaymentRejectStub always returns a 400 with the given body (a business-state rejection under
// treasury's own validation, e.g. "exceeds outstanding debit" or "no accounts-receivable balance
// found").
func arPaymentRejectStub(t *testing.T, body string) (*treasury.Client, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
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
//
// Since 2026-11-13 "exceeds outstanding debit" itself is terminal (see
// TestCreditSettlementReconciler_BlocksExceedsOutstandingDebitRefusal), so this test drives the
// backoff with another 4xx that is still only backed off.
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

	tc, calls := arPaymentRejectStub(t, `{"error":"no accounts-receivable balance found for customer"}`)
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
	if reloaded.PaymentData[treasurySyncBlockedKey] != nil {
		t.Errorf("a non-terminal 4xx must only back off, not block: %v", reloaded.PaymentData)
	}
}

// TestCreditSettlementReconciler_BlocksExceedsOutstandingDebitRefusal is the regression for
// boi-enterprises on 2026-11-13: eight credit settlements (orders 000278, 000304, 000187, 001252,
// 000228, 001976) were refused with 400 "payment N exceeds outstanding debit 0" every 15 minutes
// since August, although treasury already held a matching receipt for every one. That refusal is
// terminal: the row is stamped treasury_sync_blocked {reason, at, http_status} and never posted
// again, even once the old backoff window would have elapsed.
func TestCreditSettlementReconciler_BlocksExceedsOutstandingDebitRefusal(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	order := seedReconcilerOrder(t, client, "+254700000605", "Already Settled In Treasury")

	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(26310).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-10 * time.Minute)).
		SetPaymentData(map[string]any{
			"method": "bank", "credit_settlement": true,
			// An earlier non-terminal 4xx left a backoff marker that has since elapsed.
			"last_sync_attempt_at": time.Now().Add(-creditSettlementRetryBackoff - time.Minute).Format(time.RFC3339),
		}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed settlement payment: %v", err)
	}

	tc, calls := arPaymentRejectStub(t, `{"error":"payment 26310 exceeds outstanding debit 0"}`+"\n")
	svc.SetTreasuryClient(tc)
	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background())

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected exactly 1 treasury call, got %d", got)
	}
	reloaded, err := client.POSPayment.Get(context.Background(), payment.ID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	blocked, ok := reloaded.PaymentData[treasurySyncBlockedKey].(map[string]any)
	if !ok {
		t.Fatalf("expected %s to be stamped as an object, got %v", treasurySyncBlockedKey, reloaded.PaymentData)
	}
	if blocked["reason"] != syncBlockedExceedsOutstanding {
		t.Errorf("reason = %v, want %q", blocked["reason"], syncBlockedExceedsOutstanding)
	}
	if status, _ := blocked["http_status"].(float64); int(status) != http.StatusBadRequest {
		t.Errorf("http_status = %v, want 400", blocked["http_status"])
	}
	if at, _ := blocked["at"].(string); at == "" {
		t.Errorf("expected a blocked-at timestamp, got %v", blocked["at"])
	} else if _, perr := time.Parse(time.RFC3339, at); perr != nil {
		t.Errorf("blocked-at %q is not RFC3339: %v", at, perr)
	}
	if blocked["detail"] != "payment 26310 exceeds outstanding debit 0" {
		t.Errorf("detail = %v, want treasury's own message", blocked["detail"])
	}
	if reloaded.PaymentData["last_sync_attempt_at"] != nil {
		t.Errorf("the backoff marker should be cleared once the row is blocked: %v", reloaded.PaymentData)
	}
	if reloaded.PaymentData[treasurySyncedAtKey] != nil {
		t.Errorf("a blocked row must not be marked synced: %v", reloaded.PaymentData)
	}

	// Further passes must leave treasury alone.
	rec.runOnce(context.Background())
	rec.runOnce(context.Background())
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("expected no further treasury calls once blocked, got %d total", got)
	}
}

// TestCreditSettlementReconciler_ExcludesBlockedRows confirms the candidate query itself skips a
// row that already carries treasury_sync_blocked, with no backoff marker involved, even when
// treasury would now accept the post. Only a person removing the key brings it back.
func TestCreditSettlementReconciler_ExcludesBlockedRows(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	order := seedReconcilerOrder(t, client, "+254700000606", "Blocked By Hand")

	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(17310).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-10 * time.Minute)).
		SetPaymentData(map[string]any{
			"method": "bank", "credit_settlement": true,
			treasurySyncBlockedKey: map[string]any{
				"reason":      syncBlockedExceedsOutstanding,
				"at":          time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
				"http_status": http.StatusBadRequest,
			},
		}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed blocked settlement payment: %v", err)
	}

	tc, calls := arPaymentStub(t, 0)
	svc.SetTreasuryClient(tc)
	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background())

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("expected the blocked row to be excluded from the retry query, got %d treasury calls", got)
	}
}

// TestCreditSettlementReconciler_BlocksOrderWithNoCustomerKey is the regression for the
// boi-enterprises orders 000147, 000516, 003903, 002131, 002234, 002255 and 002358, which logged
// "no customer key on order" every 2 minutes with no backoff. With no customer phone and no staff
// party there is nothing to post against, so the row is blocked on the first pass, treasury is
// never called, and later passes skip it.
func TestCreditSettlementReconciler_BlocksOrderWithNoCustomerKey(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	order := seedReconcilerOrder(t, client, "", "")

	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(4000).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-10 * time.Minute)).
		SetPaymentData(map[string]any{"method": "cash", "credit_settlement": true}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed settlement payment: %v", err)
	}

	tc, calls := arPaymentStub(t, 0)
	svc.SetTreasuryClient(tc)
	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background())
	rec.runOnce(context.Background())

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("expected no treasury call for an order with no customer key, got %d", got)
	}
	reloaded, err := client.POSPayment.Get(context.Background(), payment.ID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	blocked, ok := reloaded.PaymentData[treasurySyncBlockedKey].(map[string]any)
	if !ok {
		t.Fatalf("expected %s to be stamped, got %v", treasurySyncBlockedKey, reloaded.PaymentData)
	}
	if blocked["reason"] != syncBlockedNoCustomerKey {
		t.Errorf("reason = %v, want %q", blocked["reason"], syncBlockedNoCustomerKey)
	}
	if _, has := blocked["http_status"]; has {
		t.Errorf("no HTTP call was made, http_status should be absent: %v", blocked)
	}
}

func TestHasCreditSettlementCustomer(t *testing.T) {
	phone, blank := "+254700000607", "   "
	cases := []struct {
		name  string
		order *ent.POSOrder
		want  bool
	}{
		{"phone", &ent.POSOrder{CustomerPhone: &phone}, true},
		{"blank phone", &ent.POSOrder{CustomerPhone: &blank}, false},
		{"no phone, no party", &ent.POSOrder{}, false},
		{"staff party", &ent.POSOrder{Metadata: map[string]any{"staff_member_id": uuid.NewString()}}, true},
		{"bad staff id", &ent.POSOrder{Metadata: map[string]any{"staff_member_id": "not-a-uuid"}}, false},
	}
	for _, tc := range cases {
		if got := hasCreditSettlementCustomer(tc.order); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestExceedsOutstandingDebitRefusal(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantOK     bool
		wantDetail string
	}{
		{"400 exceeds", &treasury.HTTPError{StatusCode: 400, Body: `{"error":"payment 20300 exceeds outstanding debit 0"}` + "\n"}, true, "payment 20300 exceeds outstanding debit 0"},
		{"400 plain text", &treasury.HTTPError{StatusCode: 400, Body: "payment 5 Exceeds Outstanding Debit 0"}, true, "payment 5 Exceeds Outstanding Debit 0"},
		{"400 other", &treasury.HTTPError{StatusCode: 400, Body: `{"error":"no accounts-receivable balance found"}`}, false, ""},
		{"409 exceeds", &treasury.HTTPError{StatusCode: 409, Body: `{"error":"payment 5 exceeds outstanding debit 0"}`}, false, ""},
		{"500 exceeds", &treasury.HTTPError{StatusCode: 500, Body: `{"error":"payment 5 exceeds outstanding debit 0"}`}, false, ""},
		{"transport", context.DeadlineExceeded, false, ""},
		{"nil", nil, false, ""},
	}
	for _, tc := range cases {
		detail, ok := exceedsOutstandingDebitRefusal(tc.err)
		if ok != tc.wantOK || detail != tc.wantDetail {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, detail, ok, tc.wantDetail, tc.wantOK)
		}
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

// TestCreditSettlementReconciler_StopsAfterReceiptlessSuccess is the regression for urban-loft order
// 000439 (2026-10-03): treasury's idempotent replay answers success with no receipt_id, the row was
// never marked done, and the reconciler re-posted it every 2 minutes forever ("retry succeeded"
// logged each tick). A success without a receipt id must still stop the retries.
func TestCreditSettlementReconciler_StopsAfterReceiptlessSuccess(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	order := seedReconcilerOrder(t, client, "+254700000599", "Receiptless Replay")
	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(2500).SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetOccurredAt(time.Now().Add(-10 * time.Minute)).
		SetPaymentData(map[string]any{"method": "cash", "credit_settlement": true}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed settlement payment: %v", err)
	}

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"bal","balance_due":"0","settled_amount":"2500.00"}`))
	}))
	t.Cleanup(srv.Close)
	svc.SetTreasuryClient(treasury.NewClient(srv.URL, "test-key", 2*time.Second))

	rec := NewCreditSettlementSyncReconciler(svc, zap.NewNop())
	rec.runOnce(context.Background())
	rec.runOnce(context.Background())
	rec.runOnce(context.Background())

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("treasury called %d times, want 1 (the replay must stop the retries)", got)
	}
	reloaded, err := client.POSPayment.Get(context.Background(), payment.ID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if reloaded.PaymentData[treasurySyncedAtKey] == nil {
		t.Errorf("expected %s on the synced payment, got %v", treasurySyncedAtKey, reloaded.PaymentData)
	}
	if reloaded.PaymentData["treasury_receipt_id"] != nil {
		t.Errorf("no receipt id was returned, none should be stored: %v", reloaded.PaymentData)
	}
}
