package reversals

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// flakyTreasury records refunds and can refuse the first N of them with a 503.
type flakyTreasury struct {
	mu       sync.Mutex
	calls    []treasury.RefundRequest
	failNext int
}

func newFlakyTreasury(t *testing.T) (*flakyTreasury, *treasury.Client) {
	t.Helper()
	ft := &flakyTreasury{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req treasury.RefundRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		ft.mu.Lock()
		if ft.failNext > 0 {
			ft.failNext--
			ft.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ft.calls = append(ft.calls, req)
		ft.mu.Unlock()
		_ = json.NewEncoder(w).Encode(treasury.RefundResponse{ID: uuid.NewString(), Status: "succeeded"})
	}))
	t.Cleanup(srv.Close)
	return ft, treasury.NewClient(srv.URL, "test-key", 0)
}

// A reversal split into a cash portion and an AR write-off raises one credit-note document: the
// second portion goes as ReversalRestReferenceType, which treasury does not raise a document for.
// It is sent only after the first portion went through; a failed step is retried by the step
// ledger and both portions keep their deterministic ids, so the retry books each once.
func TestSplitReversalSecondPortionRaisesNoCreditNote(t *testing.T) {
	ft, tc := newFlakyTreasury(t)
	svc, client := newTestReversalsService(t, tc)
	ctx := context.Background()
	tenantID := uuid.New()
	order := setupOrderWithPayment(t, client, tenantID, 100)
	rev := client.POSReversal.Create().
		SetTenantID(tenantID).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-S1").SetScope("partial").SetStatus("pending").SetReason("remove edit-added line").
		SetRefundChannel("cash").SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(1400).SetTaxAmount(140).SetCostAmount(700).
		SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		SaveX(ctx)
	p := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).OnlyX(ctx)
	client.POSPayment.UpdateOne(p).SetPaymentData(map[string]any{"method": "cash", "reversal": map[string]any{
		"reversal_number": "REV-S1", "netted_from": 100.0, "netted_to": 0.0,
	}}).ExecX(ctx)

	// The first portion fails: nothing else is sent and the step reports the error for a retry.
	ft.failNext = 1
	if _, _, _, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug"); err == nil {
		t.Fatal("want an error when the first portion fails")
	}
	ft.mu.Lock()
	if len(ft.calls) != 0 {
		t.Fatalf("%d refunds booked after the first portion failed, want 0", len(ft.calls))
	}
	ft.mu.Unlock()

	// The retry books both portions once.
	if _, _, _, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.calls) != 2 {
		t.Fatalf("%d refunds, want 2: %+v", len(ft.calls), ft.calls)
	}
	first, second := ft.calls[0], ft.calls[1]
	if first.RefundChannel != "cash" || first.ReferenceType != "pos_return" {
		t.Errorf("first portion = %q %q, want cash under pos_return", first.RefundChannel, first.ReferenceType)
	}
	if second.RefundChannel != "offset_invoice" || second.ReferenceType != ReversalRestReferenceType {
		t.Errorf("second portion = %q %q, want offset_invoice under %s", second.RefundChannel, second.ReferenceType, ReversalRestReferenceType)
	}
	if first.ReferenceID != uuid.NewSHA1(rev.ID, []byte("cash")).String() || second.ReferenceID != uuid.NewSHA1(rev.ID, []byte("ar")).String() {
		t.Errorf("portion ids changed: %s %s", first.ReferenceID, second.ReferenceID)
	}
}

// A single-portion reversal still raises its credit note under pos_return and the reversal id.
func TestSingleReversalKeepsPosReturn(t *testing.T) {
	ft, tc := newFlakyTreasury(t)
	svc, client := newTestReversalsService(t, tc)
	ctx := context.Background()
	tenantID := uuid.New()
	order := setupOrderWithPayment(t, client, tenantID, 300)
	rev := client.POSReversal.Create().
		SetTenantID(tenantID).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-S2").SetScope("partial").SetStatus("pending").SetReason("test").
		SetRefundChannel("cash").SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(200).SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		SaveX(ctx)
	p := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).OnlyX(ctx)
	client.POSPayment.UpdateOne(p).SetPaymentData(map[string]any{"method": "cash", "reversal": map[string]any{
		"reversal_number": "REV-S2", "netted_from": 300.0, "netted_to": 100.0,
	}}).ExecX(ctx)

	if _, _, _, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug"); err != nil {
		t.Fatal(err)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.calls) != 1 || ft.calls[0].ReferenceType != "pos_return" || ft.calls[0].ReferenceID != rev.ID.String() {
		t.Errorf("calls = %+v, want one pos_return refund under the reversal id", ft.calls)
	}
}
