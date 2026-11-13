package returns

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// S2-8 review I-1: pos.return.completed carries the channel the return settled through on every
// return, so treasury's GL backstop credits the right account when it books the return before
// the S2S refund (M-Pesa float, AR for an on-account sale, 2300 for store credit), never Cash by
// default.
func TestReturnCompletedCarriesTheRefundChannel(t *testing.T) {
	for _, tc := range []struct {
		name, paidWith string
		onAccount      bool
		rtype          posreturn.ReturnType
		want           string
	}{
		{name: "cash", paidWith: "cash", rtype: posreturn.ReturnTypeRefund, want: "cash"},
		{name: "mpesa manual", paidWith: "mpesa_manual", rtype: posreturn.ReturnTypeRefund, want: "mpesa_manual"},
		{name: "on account", paidWith: "cash", onAccount: true, rtype: posreturn.ReturnTypeRefund, want: "offset_invoice"},
		{name: "store credit", paidWith: "cash", rtype: posreturn.ReturnTypeStoreCredit, want: "store_credit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, client, rec := newRefundTestService(t)
			order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
			if tc.onAccount {
				client.POSOrder.UpdateOneID(order.ID).SetMetadata(map[string]any{"on_account": true}).ExecX(context.Background())
			}
			seedPayment(t, client, order.ID, tc.paidWith, 500)
			ret := seedApprovedReturn(t, client, order, tc.rtype, nil)

			done := complete(t, svc, ret, CompleteReturnRequest{})
			if got := onlyRefund(t, rec).RefundChannel; got != tc.want {
				t.Errorf("treasury refund_channel = %q, want %q", got, tc.want)
			}
			if p := svc.completedPayload(context.Background(), done, nil, ""); p["refund_channel"] != tc.want {
				t.Errorf("return.completed refund_channel = %v, want %q (the same channel as the S2S refund)", p["refund_channel"], tc.want)
			}
		})
	}
}

// A main refund that fails at completion is kept on the return and sent again by the retry job,
// once, with the same channel and reference; the return then records treasury's reference.
func TestFailedMainRefundIsRetriedOnce(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	ctx := context.Background()
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	seedPayment(t, client, order.ID, "mpesa_manual", 500)
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

	rec.mu.Lock()
	rec.failRefType = "pos_return"
	rec.mu.Unlock()
	done := complete(t, svc, ret, CompleteReturnRequest{})
	if done.Status != posreturn.StatusCompleted || done.TreasuryRefundRef != nil && *done.TreasuryRefundRef != "" {
		t.Fatalf("status %s ref %v, want completed with no treasury ref", done.Status, done.TreasuryRefundRef)
	}
	if done.Metadata[MetaRefundPending] != true || done.Metadata[MetaRefundChannel] != "mpesa_manual" || done.Metadata[MetaRefundAmount] != 250.0 {
		t.Fatalf("pending refund metadata = %v", done.Metadata)
	}
	if p := svc.completedPayload(ctx, done, nil, ""); p["refund_channel"] != "mpesa_manual" {
		t.Errorf("return.completed refund_channel = %v, want mpesa_manual for the GL backstop", p["refund_channel"])
	}

	// Treasury still down: nothing sent, one attempt counted, still pending.
	if n := svc.RetryPendingRefunds(ctx); n != 0 {
		t.Fatalf("retry while treasury is down sent %d, want 0", n)
	}
	if md := client.POSReturn.GetX(ctx, done.ID).Metadata; md[MetaRefundAttempts] != 1.0 || md[MetaRefundPending] != true {
		t.Fatalf("after a failed retry: attempts %v pending %v", md[MetaRefundAttempts], md[MetaRefundPending])
	}

	rec.mu.Lock()
	rec.failRefType = ""
	rec.mu.Unlock()
	for i, want := range []int{1, 0} {
		if n := svc.RetryPendingRefunds(ctx); n != want {
			t.Fatalf("retry %d sent %d, want %d", i, n, want)
		}
	}
	got := onlyRefund(t, rec)
	if got.ReferenceID != done.ID.String() || got.ReferenceType != "pos_return" || got.RefundChannel != "mpesa_manual" || got.Amount != 250 {
		t.Errorf("retried refund = %+v, want 250 mpesa_manual under the return id", got)
	}
	after := client.POSReturn.GetX(ctx, done.ID)
	if after.TreasuryRefundRef == nil || *after.TreasuryRefundRef == "" {
		t.Error("treasury refund ref not recorded after the retry")
	}
	if _, pending := after.Metadata[MetaRefundPending]; pending {
		t.Error("refund still marked pending after it went through")
	}
}

// S2-8 N-1: an exchange whose refund fails is kept pending with its exchange credit, and the retry
// resends the whole split (treasury's GL backstop skips exchanges, so only this books clearing).
// N-3: every refund carries the return's outlet, fiscalised or not.
func TestFailedExchangeRefundIsRetriedWithItsExchangeCredit(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	ctx := context.Background()
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	seedPayment(t, client, order.ID, "mpesa_manual", 500)
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeExchange, nil)
	replacement := []LineInput{{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "Replacement", Quantity: 1, UnitPrice: 100, TotalPrice: 100}}

	rec.mu.Lock()
	rec.failRefType = "pos_return"
	rec.mu.Unlock()
	done, _, err := svc.CompleteReturn(ctx, ret.TenantID, "acme", ret.ID, CompleteReturnRequest{ExchangeLines: replacement})
	if err != nil {
		t.Fatalf("CompleteReturn: %v", err)
	}
	if done.Metadata[MetaRefundPending] != true || done.Metadata[MetaRefundExchangeCredit] != 100.0 {
		t.Fatalf("pending exchange refund metadata = %v, want pending with exchange credit 100", done.Metadata)
	}

	rec.mu.Lock()
	rec.failRefType = ""
	rec.mu.Unlock()
	for i, want := range []int{1, 0} {
		if n := svc.RetryPendingRefunds(ctx); n != want {
			t.Fatalf("retry %d sent %d, want %d", i, n, want)
		}
	}
	got := onlyRefund(t, rec)
	if got.ExchangeCredit != 100 || got.Amount != 250 || got.RefundChannel != "mpesa_manual" || got.ReferenceID != done.ID.String() {
		t.Errorf("retried exchange refund = %+v, want 250 with exchange credit 100 on mpesa_manual", got)
	}
	if got.OutletID != order.OutletID.String() {
		t.Errorf("refund outlet = %q, want the return's outlet %s", got.OutletID, order.OutletID)
	}
}

// S2-8 N-3: a non-fiscalised return's refund still carries its outlet.
func TestCompleteReturnSendsTheOutlet(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	seedPayment(t, client, order.ID, "mpesa_manual", 500)
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)
	complete(t, svc, ret, CompleteReturnRequest{})
	if got := onlyRefund(t, rec).OutletID; got != order.OutletID.String() {
		t.Errorf("refund outlet = %q, want %s", got, order.OutletID)
	}
}

// A refund treasury keeps refusing stops after the cap and is marked failed, so it never starves
// newer ones.
func TestRetryPendingRefundsStopsAfterTheCap(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	ctx := context.Background()
	rec.mu.Lock()
	rec.failRefType = "pos_return"
	rec.mu.Unlock()
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	md := map[string]any{}
	mainRefund{Amount: 250, Channel: "cash", Currency: "KES"}.toMetadata(md)
	stuck := client.POSReturn.Create().
		SetTenantID(order.TenantID).SetOutletID(order.OutletID).SetOrderID(order.ID).
		SetReturnNumber("RET-" + uuid.NewString()[:6]).SetReturnType(posreturn.ReturnTypeRefund).
		SetStatus(posreturn.StatusCompleted).SetReason("x").SetRefundAmount(250).SetRequestedBy(uuid.New()).
		SetCreatedAt(time.Now().Add(-time.Hour)).SetMetadata(md).
		SaveX(ctx)
	for i := 0; i < maxRefundAttempts; i++ {
		svc.RetryPendingRefunds(ctx)
	}
	got := client.POSReturn.GetX(ctx, stuck.ID).Metadata
	if _, pending := got[MetaRefundPending]; pending || got[MetaRefundFailed] != true {
		t.Fatalf("after %d attempts: pending %v failed %v, want not pending and failed", maxRefundAttempts, got[MetaRefundPending], got[MetaRefundFailed])
	}
}

// The stay lookup itself failing must not read as "no open stay" (which refunds the room-charged
// sale through the chosen channel, cash by default): the completion fails and nothing goes out.
func TestCompleteReturnStayLookupErrorRefundsNothing(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	ctx := context.Background()
	order, _ := seedRoomChargedOrder(t, client, true)
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

	client.RoomGuest.Intercept(ent.InterceptFunc(func(ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(context.Context, ent.Query) (ent.Value, error) {
			return nil, errors.New("room stays unavailable")
		})
	}))
	if _, _, err := svc.CompleteReturn(ctx, ret.TenantID, "acme", ret.ID, CompleteReturnRequest{}); err == nil {
		t.Fatal("CompleteReturn succeeded on a failed stay lookup, want an error so it is retried")
	}
	if got := rec.sent(); len(got) != 0 {
		t.Fatalf("treasury refunds = %+v, want none (no cash refund)", got)
	}
	after := client.POSReturn.GetX(ctx, ret.ID)
	if after.Status != posreturn.StatusApproved {
		t.Errorf("status = %s, want approved", after.Status)
	}
	if _, ok := after.Metadata[MetaCashPaidOut]; ok {
		t.Error("cash recorded as paid out on a failed completion")
	}
}

// S2-8 review: a folio lookup that fails must fail the completion, never read as "the folio owes
// nothing", which would refund the room-charged sale as cash while the folio still bills the guest.
func TestCompleteReturnFolioLookupErrorRefundsNothing(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	ctx := context.Background()
	order, _ := seedRoomChargedOrder(t, client, true)
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

	// Every folio payment read fails from here on (a dropped connection, a lock timeout).
	client.RoomFolioPayment.Intercept(ent.InterceptFunc(func(ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(context.Context, ent.Query) (ent.Value, error) {
			return nil, errors.New("folio payments unavailable")
		})
	}))
	if _, _, err := svc.CompleteReturn(ctx, ret.TenantID, "acme", ret.ID, CompleteReturnRequest{}); err == nil {
		t.Fatal("CompleteReturn succeeded on a failed folio lookup, want an error so it is retried")
	}
	if got := rec.sent(); len(got) != 0 {
		t.Fatalf("treasury refunds = %+v, want none", got)
	}
	after := client.POSReturn.GetX(ctx, ret.ID)
	if after.Status != posreturn.StatusApproved {
		t.Errorf("status = %s, want approved (completion can be tried again)", after.Status)
	}
	if _, ok := after.Metadata[MetaCashPaidOut]; ok {
		t.Error("cash recorded as paid out on a failed completion")
	}
}
