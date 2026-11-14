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

	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// S2-20 review M-10: a desk folio payment whose treasury intent failed is sent again with its
// original reference id (also the idempotency key). A failure counts an attempt and keeps it
// pending; a success clears it and records the intent id.
func TestRetryPendingFolioIntents(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	_, guest := seedRoomChargedSale(t, client, tid, true)

	var fail atomic.Bool
	fail.Store(true)
	var gotRef, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body treasury.CreateIntentRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotRef, gotKey = body.ReferenceID, r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"intent_id":"intent-1","status":"succeeded"}`))
	}))
	t.Cleanup(srv.Close)
	svc.SetTreasuryClient(treasury.NewClient(srv.URL, "test-key", 2*time.Second))

	req := treasury.CreateIntentRequest{
		SourceService: "pos", ReferenceID: "POS-ref-1", ReferenceType: "hotel_folio", Amount: 500, Currency: "KES",
		PaymentMethod: "cash", OutletID: uuid.NewString(), Metadata: map[string]any{"room_revenue_amount": "500.00"},
	}
	p := client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).SetAmount(500).SetMethod("cash").SetStatus("completed").
		SetMetadata(map[string]any{MetaFolioIntentPending: PendingFolioIntent(req)}).
		SaveX(ctx)
	// A payment that went through first time is never touched.
	client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).SetAmount(100).SetMethod("cash").SetStatus("completed").
		SetTreasuryIntentID("intent-0").SaveX(ctx)

	if n := svc.RetryPendingFolioIntents(ctx); n != 0 {
		t.Fatalf("sent %d while treasury is down, want 0", n)
	}
	got := client.RoomFolioPayment.GetX(ctx, p.ID)
	if _, pending := got.Metadata[MetaFolioIntentPending]; !pending || intAttempts(got.Metadata[MetaFolioIntentAttempts]) != 1 {
		t.Fatalf("after a failure: metadata %v, want pending with 1 attempt", got.Metadata)
	}

	fail.Store(false)
	if n := svc.RetryPendingFolioIntents(ctx); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	got = client.RoomFolioPayment.GetX(ctx, p.ID)
	if got.TreasuryIntentID != "intent-1" {
		t.Errorf("intent id = %v, want intent-1", got.TreasuryIntentID)
	}
	if _, pending := got.Metadata[MetaFolioIntentPending]; pending {
		t.Error("the payment must leave the queue once sent")
	}
	if gotRef != "POS-ref-1" || gotKey != "POS-ref-1" {
		t.Errorf("reference %q, idempotency key %q; want the original reference for both", gotRef, gotKey)
	}
	if n := svc.RetryPendingFolioIntents(ctx); n != 0 {
		t.Errorf("a second pass sent %d, want 0", n)
	}
}

// After maxFolioIntentAttempts failures the payment is marked failed and leaves the queue.
func TestRetryPendingFolioIntentsGivesUp(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	_, guest := seedRoomChargedSale(t, client, tid, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	svc.SetTreasuryClient(treasury.NewClient(srv.URL, "test-key", 2*time.Second))

	req := treasury.CreateIntentRequest{ReferenceID: "POS-ref-2", ReferenceType: "hotel_folio", Amount: 300, PaymentMethod: "cash"}
	p := client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).SetAmount(300).SetMethod("cash").SetStatus("completed").
		SetMetadata(map[string]any{MetaFolioIntentPending: PendingFolioIntent(req), MetaFolioIntentAttempts: maxFolioIntentAttempts - 1}).
		SaveX(ctx)

	svc.RetryPendingFolioIntents(ctx)
	got := client.RoomFolioPayment.GetX(ctx, p.ID)
	if _, pending := got.Metadata[MetaFolioIntentPending]; pending || got.Metadata[MetaFolioIntentFailed] != true {
		t.Errorf("metadata %v, want failed and out of the queue", got.Metadata)
	}
}

func TestFolioIntentFromMetadataRefusesBadRows(t *testing.T) {
	for _, md := range []map[string]any{
		{},
		{MetaFolioIntentPending: "not a map"},
		{MetaFolioIntentPending: map[string]any{"amount": 100.0}},                    // no reference id
		{MetaFolioIntentPending: map[string]any{"reference_id": "x", "amount": 0.0}}, // nothing to book
	} {
		if _, ok := folioIntentFromMetadata(md); ok {
			t.Errorf("folioIntentFromMetadata(%v) accepted a bad row", md)
		}
	}
}
