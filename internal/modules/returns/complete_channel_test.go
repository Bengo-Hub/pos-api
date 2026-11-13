package returns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/payments"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// refundRecorder is a treasury stand-in: it records every /refunds body and answers 404 to the
// eTIMS lookup (the sale was not fiscalised).
type refundRecorder struct {
	mu    sync.Mutex
	calls []treasury.RefundRequest
	// failRefType, when set, answers 503 to refunds of that reference type (not recorded),
	// standing in for a treasury outage on that call.
	failRefType string
}

func (r *refundRecorder) sent() []treasury.RefundRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]treasury.RefundRequest(nil), r.calls...)
}

func newRefundTestService(t *testing.T) (*Service, *ent.Client, *refundRecorder) {
	t.Helper()
	rec := &refundRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/refunds") {
			http.NotFound(w, r)
			return
		}
		var body treasury.RefundRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		fail := rec.failRefType != "" && body.ReferenceType == rec.failRefType
		if !fail {
			rec.calls = append(rec.calls, body)
		}
		rec.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.RefundResponse{ID: uuid.NewString()})
	}))
	t.Cleanup(srv.Close)
	svc, client := newTestService(t)
	svc.treasuryClient = treasury.NewClient(srv.URL, "test-key", 2*time.Second)
	svc.SetOrderService(orders.NewService(client, orders.Config{}, zap.NewNop()))
	svc.SetOrderCompletion(&completionSpy{})
	return svc, client, rec
}

// seedApprovedReturn is an approved return of 250 against the order. channel nil is a return
// created before the original-method default (no channel stored).
func seedApprovedReturn(t *testing.T, client *ent.Client, order *ent.POSOrder, rtype posreturn.ReturnType, channel *posreturn.RefundChannel) *ent.POSReturn {
	t.Helper()
	ctx := context.Background()
	ret := client.POSReturn.Create().
		SetTenantID(order.TenantID).SetOutletID(order.OutletID).SetOrderID(order.ID).
		SetReturnNumber("RET-" + uuid.NewString()[:6]).SetReturnType(rtype).SetStatus(posreturn.StatusApproved).
		SetReason("changed mind").SetRefundAmount(250).SetRequestedBy(uuid.New()).
		SetNillableRefundChannel(channel).
		SaveX(ctx)
	client.POSReturnLine.Create().
		SetReturnID(ret.ID).SetOrderLineID(firstLineID(t, client, order.ID)).SetSku("SKU-1").SetName("Sample Item").
		SetQuantity(1).SetUnitPrice(250).SetTotalPrice(250).
		SaveX(ctx)
	return ret
}

func complete(t *testing.T, svc *Service, ret *ent.POSReturn, req CompleteReturnRequest) *ent.POSReturn {
	t.Helper()
	done, _, err := svc.CompleteReturn(context.Background(), ret.TenantID, "acme", ret.ID, req)
	if err != nil {
		t.Fatalf("CompleteReturn: %v", err)
	}
	return done
}

func onlyRefund(t *testing.T, rec *refundRecorder) treasury.RefundRequest {
	t.Helper()
	got := rec.sent()
	if len(got) != 1 {
		t.Fatalf("%d treasury refunds, want 1: %+v", len(got), got)
	}
	return got[0]
}

// A return created before the original-method default has no channel; completing it refunds
// the way the sale was paid.
func TestCompleteReturnOlderReturnUsesTheOriginalMethod(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	seedPayment(t, client, order.ID, "mtn_momo_manual", 500)
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

	done := complete(t, svc, ret, CompleteReturnRequest{})
	if got := onlyRefund(t, rec).RefundChannel; got != "mtn_momo_manual" {
		t.Errorf("treasury refund_channel = %q, want mtn_momo_manual", got)
	}
	if done.RefundChannel == nil || *done.RefundChannel != posreturn.RefundChannelMtnMomoManual {
		t.Errorf("stored channel = %v, want mtn_momo_manual", done.RefundChannel)
	}
	if _, ok := done.Metadata[MetaCashPaidOut]; ok {
		t.Error("an MTN refund is not cash out of the drawer")
	}
}

// A cashier who picks another channel at completion wins.
func TestCompleteReturnOverrideAtCompletionWins(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	seedPayment(t, client, order.ID, "mpesa_manual", 500)
	ch := posreturn.RefundChannelMpesaManual
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, &ch)

	done := complete(t, svc, ret, CompleteReturnRequest{RefundChannel: "cash"})
	if got := onlyRefund(t, rec).RefundChannel; got != "cash" {
		t.Errorf("treasury refund_channel = %q, want cash", got)
	}
	if done.Metadata[MetaCashPaidOut] != 250.0 {
		t.Errorf("cash paid out = %v, want 250", done.Metadata[MetaCashPaidOut])
	}
}

// An on-account sale keeps reducing what the customer owes, whatever the payments say.
func TestCompleteReturnOnAccountSaleOffsets(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	client.POSOrder.UpdateOneID(order.ID).SetMetadata(map[string]any{"on_account": true}).ExecX(context.Background())
	seedPayment(t, client, order.ID, "cash", 100)
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

	complete(t, svc, ret, CompleteReturnRequest{})
	if got := onlyRefund(t, rec).RefundChannel; got != "offset_invoice" {
		t.Errorf("treasury refund_channel = %q, want offset_invoice", got)
	}
}

// An exchange leftover goes back the original way by default, and the cashier can record cash.
func TestCompleteReturnExchangeLeftover(t *testing.T) {
	replacement := []LineInput{{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "Replacement", Quantity: 1, UnitPrice: 100, TotalPrice: 100}}
	for _, tc := range []struct {
		name, override, want string
		cashOut              any
	}{
		{name: "default", want: "airtel_money_manual", cashOut: nil},
		{name: "cashier hands back notes", override: "cash", want: "cash", cashOut: 150.0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, client, rec := newRefundTestService(t)
			order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
			seedPayment(t, client, order.ID, "airtel_money_manual", 500)
			ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeExchange, nil)

			done := complete(t, svc, ret, CompleteReturnRequest{ExchangeLines: replacement, RefundChannel: tc.override})
			got := onlyRefund(t, rec)
			if got.RefundChannel != tc.want || got.ExchangeCredit != 100 {
				t.Errorf("treasury refund = channel %q exchange credit %v, want %q and 100", got.RefundChannel, got.ExchangeCredit, tc.want)
			}
			if done.Metadata[MetaCashPaidOut] != tc.cashOut {
				t.Errorf("cash paid out = %v, want %v", done.Metadata[MetaCashPaidOut], tc.cashOut)
			}
		})
	}
}

// seedRoomChargedOrder is a till sale charged to a room stay (the folio item carries the order).
func seedRoomChargedOrder(t *testing.T, client *ent.Client, stayOpen bool) (*ent.POSOrder, *ent.RoomGuest) {
	t.Helper()
	ctx := context.Background()
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	seedPayment(t, client, order.ID, "room_charge", 500)
	status := roomguest.StatusActive
	if !stayOpen {
		status = roomguest.StatusCheckedOut
	}
	// The stay and its folio line both hold a foreign key to a real room.
	room := client.Room.Create().
		SetTenantID(order.TenantID).SetOutletID(uuid.New()).SetRoomNumber("R" + uuid.NewString()[:6]).SetName("Room").
		SetRatePerNight(1000).
		SaveX(ctx)
	guest := client.RoomGuest.Create().
		SetTenantID(order.TenantID).SetRoomID(room.ID).SetGuestName("Jane").SetPhone("+254712345678").SetIDNumber("ID-1").
		SetCheckInDate(time.Now()).SetNights(1).SetCheckOutDate(time.Now().AddDate(0, 0, 1)).
		SetTotalRoomCharge(1000).SetCheckedInBy(uuid.New()).SetStatus(status).
		SaveX(ctx)
	client.RoomFolioItem.Create().
		SetTenantID(order.TenantID).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).SetDescription("Restaurant bill").
		SetAmount(500).SetChargeType(roomfolioitem.ChargeTypeOther).SetCreatedBy(uuid.New()).SetPosOrderID(order.ID).
		SetMetadata(map[string]any{"source": "pos_room_charge"}). // written by the till's room charge
		SaveX(ctx)
	return order, guest
}

// A room-charged sale returned while the stay is open is credited to the folio: no cash leaves
// the drawer, and treasury reverses the sale against the guest receivable the till booked it to
// (S2-20: refund channel room_folio, not cash).
func TestCompleteReturnRoomChargeOpenStayCreditsTheFolio(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	order, guest := seedRoomChargedOrder(t, client, true)
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

	done := complete(t, svc, ret, CompleteReturnRequest{RefundChannel: "mpesa"})
	if done.RefundChannel == nil || *done.RefundChannel != posreturn.RefundChannelRoomFolio {
		t.Errorf("stored channel = %v, want room_folio (an open stay always takes the credit)", done.RefundChannel)
	}
	if _, ok := done.Metadata[MetaCashPaidOut]; ok {
		t.Error("a folio credit is not cash out of the drawer")
	}
	if got := onlyRefund(t, rec).RefundChannel; got != "room_folio" {
		t.Errorf("treasury refund_channel = %q, want room_folio (reduces the guest receivable)", got)
	}
	if p := svc.completedPayload(context.Background(), done, nil, ""); p["refund_channel"] != "room_folio" {
		t.Errorf("return.completed refund_channel = %v, want room_folio so the GL backstop credits the receivable", p["refund_channel"])
	}
	credits := client.RoomFolioPayment.Query().AllX(context.Background())
	if len(credits) != 1 || credits[0].Method != payments.FolioReturnCreditMethod || credits[0].Amount != 250 ||
		credits[0].RoomGuestID != guest.ID || credits[0].Reference != ret.ReturnNumber {
		t.Fatalf("folio credits = %+v, want one return credit of 250 on the stay", credits)
	}
}

// Once the stay is checked out, the refund goes through the chosen channel (cash by default).
func TestCompleteReturnRoomChargeClosedStayUsesTheChosenChannel(t *testing.T) {
	for _, tc := range []struct{ override, want string }{{"", "cash"}, {"mpesa", "mpesa"}} {
		svc, client, rec := newRefundTestService(t)
		order, _ := seedRoomChargedOrder(t, client, false)
		ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

		done := complete(t, svc, ret, CompleteReturnRequest{RefundChannel: tc.override})
		if got := onlyRefund(t, rec).RefundChannel; got != tc.want {
			t.Errorf("override %q: treasury refund_channel = %q, want %q", tc.override, got, tc.want)
		}
		if done.RefundChannel == nil || string(*done.RefundChannel) != tc.want {
			t.Errorf("override %q: stored channel = %v, want %q", tc.override, done.RefundChannel, tc.want)
		}
		if n := client.RoomFolioPayment.Query().CountX(context.Background()); n != 0 {
			t.Errorf("override %q: %d folio credits on a closed stay, want 0", tc.override, n)
		}
	}
}

// A mixed sale (cash 200, room 300) returned in two parts while the stay is open: the folio is
// credited at most the 300 that was charged to the room (S2-20 review I-1), so the second return
// credits only the 50 left and refunds the other 200 in cash as a pos_return_rest portion. The
// guest receivable is never credited more than the till debited it.
func TestCompleteReturnFolioCreditCappedAtTheRoomCharge(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	ctx := context.Background()
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	seedPayment(t, client, order.ID, "cash", 200)
	seedPayment(t, client, order.ID, "room_charge", 300)
	room := client.Room.Create().
		SetTenantID(order.TenantID).SetOutletID(uuid.New()).SetRoomNumber("R" + uuid.NewString()[:6]).SetName("Room").
		SetRatePerNight(1000).
		SaveX(ctx)
	guest := client.RoomGuest.Create().
		SetTenantID(order.TenantID).SetRoomID(room.ID).SetGuestName("Jane").SetPhone("+254712345678").SetIDNumber("ID-1").
		SetCheckInDate(time.Now()).SetNights(2).SetCheckOutDate(time.Now().AddDate(0, 0, 2)).
		SetTotalRoomCharge(2000).SetCheckedInBy(uuid.New()).SetStatus(roomguest.StatusActive).
		SaveX(ctx)
	client.RoomFolioItem.Create().
		SetTenantID(order.TenantID).SetRoomID(room.ID).SetRoomGuestID(guest.ID).SetDescription("Room").
		SetAmount(2000).SetChargeType(roomfolioitem.ChargeTypeRoomCharge).SetCreatedBy(uuid.New()).
		SaveX(ctx)
	client.RoomFolioItem.Create().
		SetTenantID(order.TenantID).SetRoomID(room.ID).SetRoomGuestID(guest.ID).SetDescription("POS bill").
		SetAmount(300).SetChargeType(roomfolioitem.ChargeTypeRestaurant).SetCreatedBy(uuid.New()).SetPosOrderID(order.ID).
		SetMetadata(map[string]any{"source": "pos_room_charge"}).
		SaveX(ctx)

	first := complete(t, svc, seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil), CompleteReturnRequest{})
	second := complete(t, svc, seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil), CompleteReturnRequest{})

	var credited float64
	for _, c := range client.RoomFolioPayment.Query().Where(roomfoliopayment.Method(payments.FolioReturnCreditMethod)).AllX(ctx) {
		credited += c.Amount
	}
	if credited != 300 {
		t.Errorf("folio credits = %v, want 300 (250 then the 50 left of the room charge)", credited)
	}
	if first.Metadata[MetaFolioCredit] != 250.0 || second.Metadata[MetaFolioCredit] != 50.0 {
		t.Errorf("folio credit per return = %v, %v; want 250, 50", first.Metadata[MetaFolioCredit], second.Metadata[MetaFolioCredit])
	}
	calls := rec.sent()
	if len(calls) != 3 {
		t.Fatalf("%d treasury refunds, want 3: %+v", len(calls), calls)
	}
	if calls[0].Amount != 250 || calls[0].RefundChannel != "room_folio" ||
		calls[1].Amount != 50 || calls[1].RefundChannel != "room_folio" ||
		calls[2].Amount != 200 || calls[2].RefundChannel != "cash" || calls[2].ReferenceType != RestRefundReferenceType {
		t.Errorf("treasury refunds = %+v, want 250 room_folio, 50 room_folio, then 200 cash as %s", calls, RestRefundReferenceType)
	}
}

func payFolio(t *testing.T, client *ent.Client, guest *ent.RoomGuest, amount float64) {
	t.Helper()
	client.RoomFolioPayment.Create().
		SetTenantID(guest.TenantID).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).
		SetAmount(amount).SetMethod("cash").SetStatus("completed").
		SaveX(context.Background())
}

// A folio the guest already settled gets no credit: the whole refund goes through the
// cashier's channel (cash by default), and only that counts as cash out of the drawer.
func TestCompleteReturnRoomChargeSettledFolioRefundsByChannel(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	order, guest := seedRoomChargedOrder(t, client, true)
	payFolio(t, client, guest, 500)
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

	done := complete(t, svc, ret, CompleteReturnRequest{})
	if got := onlyRefund(t, rec); got.RefundChannel != "cash" || got.Amount != 250 {
		t.Errorf("treasury refund = %q %v, want cash 250", got.RefundChannel, got.Amount)
	}
	if done.RefundChannel == nil || *done.RefundChannel != posreturn.RefundChannelCash {
		t.Errorf("stored channel = %v, want cash", done.RefundChannel)
	}
	if done.Metadata[MetaCashPaidOut] != 250.0 {
		t.Errorf("cash paid out = %v, want 250", done.Metadata[MetaCashPaidOut])
	}
	if n := client.RoomFolioPayment.Query().Where(roomfoliopayment.Method(payments.FolioReturnCreditMethod)).CountX(context.Background()); n != 0 {
		t.Errorf("%d folio credits on a settled folio, want 0", n)
	}
}

// A part-settled folio is credited what it still owes; the rest goes through the cashier's
// channel as a second treasury portion.
func TestCompleteReturnRoomChargePartSettledFolioSplits(t *testing.T) {
	for _, tc := range []struct {
		override, restChannel string
		cashOut               any
	}{
		{"", "cash", 150.0},
		{"mpesa", "mpesa", nil},
	} {
		svc, client, rec := newRefundTestService(t)
		order, guest := seedRoomChargedOrder(t, client, true) // folio charge 500
		payFolio(t, client, guest, 400)                       // owes 100
		ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

		done := complete(t, svc, ret, CompleteReturnRequest{RefundChannel: tc.override})
		credits := client.RoomFolioPayment.Query().Where(roomfoliopayment.Method(payments.FolioReturnCreditMethod)).AllX(context.Background())
		if len(credits) != 1 || credits[0].Amount != 100 {
			t.Fatalf("override %q: folio credits = %+v, want one of 100", tc.override, credits)
		}
		calls := rec.sent()
		if len(calls) != 2 {
			t.Fatalf("override %q: %d treasury refunds, want 2: %+v", tc.override, len(calls), calls)
		}
		if calls[0].Amount != 100 || calls[0].RefundChannel != "room_folio" || calls[1].Amount != 150 || calls[1].RefundChannel != tc.restChannel {
			t.Errorf("override %q: portions = %v %q + %v %q, want 100 room_folio + 150 %q",
				tc.override, calls[0].Amount, calls[0].RefundChannel, calls[1].Amount, calls[1].RefundChannel, tc.restChannel)
		}
		if calls[0].ReferenceID == calls[1].ReferenceID {
			t.Errorf("override %q: the two portions need distinct reference ids", tc.override)
		}
		// The rest must not raise a second credit note (treasury raises one per pos_return
		// refund) and keeps the outlet for its GL attribution.
		if calls[0].ReferenceType != "pos_return" || calls[1].ReferenceType != RestRefundReferenceType {
			t.Errorf("override %q: reference types = %q, %q; want pos_return, %s", tc.override, calls[0].ReferenceType, calls[1].ReferenceType, RestRefundReferenceType)
		}
		if calls[1].OutletID != order.OutletID.String() || len(calls[1].Lines) != 0 {
			t.Errorf("override %q: rest outlet %q lines %d, want the sale's outlet and no lines", tc.override, calls[1].OutletID, len(calls[1].Lines))
		}
		if _, pending := done.Metadata[MetaFolioRestPending]; pending {
			t.Errorf("override %q: rest sent, so it must not be marked pending", tc.override)
		}
		if done.Metadata[MetaCashPaidOut] != tc.cashOut {
			t.Errorf("override %q: cash paid out = %v, want %v", tc.override, done.Metadata[MetaCashPaidOut], tc.cashOut)
		}
	}
}

// When the main refund fails, the rest is not sent: it is marked pending, and the retry job
// sends it once (idempotent on its deterministic id). return.completed tells treasury's GL
// backstop to book only the part under the return id, so the rest is never reversed twice.
func TestCompleteReturnFolioRestWaitsForTheMainRefund(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	order, guest := seedRoomChargedOrder(t, client, true)
	payFolio(t, client, guest, 400) // owes 100 of the 250 return
	ret := seedApprovedReturn(t, client, order, posreturn.ReturnTypeRefund, nil)

	rec.mu.Lock()
	rec.failRefType = "pos_return"
	rec.mu.Unlock()
	done := complete(t, svc, ret, CompleteReturnRequest{})
	if n := len(rec.sent()); n != 0 {
		t.Fatalf("%d refunds reached treasury, want none (the main one failed, the rest waits)", n)
	}
	if done.Metadata[MetaFolioRestPending] != true {
		t.Fatalf("rest pending = %v, want true", done.Metadata[MetaFolioRestPending])
	}
	if p := svc.completedPayload(context.Background(), done, nil, ""); p["ledger_refund_amount"] != 100.0 || p["refund_amount"] != 250.0 {
		t.Errorf("payload refund_amount %v ledger_refund_amount %v, want 250 and 100", p["refund_amount"], p["ledger_refund_amount"])
	}

	for i := 0; i < 2; i++ {
		if n := svc.RetryPendingFolioRests(context.Background()); n != map[int]int{0: 1, 1: 0}[i] {
			t.Errorf("retry %d settled %d rests, want %d", i, n, map[int]int{0: 1, 1: 0}[i])
		}
	}
	calls := rec.sent()
	if len(calls) != 1 || calls[0].ReferenceType != RestRefundReferenceType || calls[0].Amount != 150 || calls[0].RefundChannel != "cash" {
		t.Fatalf("treasury refunds after retry = %+v, want one rest of 150 cash", calls)
	}
	if calls[0].ReferenceID != restRefundID(done.ID).String() {
		t.Errorf("rest reference id = %s, want the deterministic one", calls[0].ReferenceID)
	}
	if md := client.POSReturn.GetX(context.Background(), done.ID).Metadata; md[MetaFolioRestPending] != nil {
		t.Errorf("rest still marked pending after it was sent: %v", md[MetaFolioRestPending])
	}
}

// seedPendingRest is a completed return whose folio rest is still waiting for treasury.
func seedPendingRest(t *testing.T, client *ent.Client, createdAt time.Time) *ent.POSReturn {
	t.Helper()
	order := seedOrder(t, client, uuid.New(), uuid.New(), "completed")
	return client.POSReturn.Create().
		SetTenantID(order.TenantID).SetOutletID(order.OutletID).SetOrderID(order.ID).
		SetReturnNumber("RET-" + uuid.NewString()[:6]).SetReturnType(posreturn.ReturnTypeRefund).
		SetStatus(posreturn.StatusCompleted).SetReason("x").SetRefundAmount(250).SetRequestedBy(uuid.New()).
		SetRefundChannel(posreturn.RefundChannelRoomFolio).SetCreatedAt(createdAt).
		SetMetadata(map[string]any{
			MetaFolioCredit: 100.0, MetaFolioRestAmount: 150.0, MetaFolioRestChannel: "cash",
			MetaFolioRestPending: true,
		}).
		SaveX(context.Background())
}

// Pending rests are retried oldest first in bounded batches.
func TestRetryPendingFolioRestsOldestFirst(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	newer := seedPendingRest(t, client, time.Now().Add(-time.Minute))
	older := seedPendingRest(t, client, time.Now().Add(-time.Hour))
	defer func(b int) { folioRestRetryBatch = b }(folioRestRetryBatch)
	folioRestRetryBatch = 1

	if n := svc.RetryPendingFolioRests(context.Background()); n != 1 {
		t.Fatalf("first pass sent %d, want 1", n)
	}
	if got := rec.sent(); len(got) != 1 || got[0].ReferenceID != restRefundID(older.ID).String() {
		t.Fatalf("first pass sent %+v, want the older return's rest", got)
	}
	if n := svc.RetryPendingFolioRests(context.Background()); n != 1 {
		t.Fatalf("second pass sent %d, want 1", n)
	}
	if got := rec.sent(); len(got) != 2 || got[1].ReferenceID != restRefundID(newer.ID).String() {
		t.Fatalf("second pass sent %+v, want the newer return's rest", got)
	}
}

// A rest treasury keeps refusing is retried a bounded number of times, then marked failed for
// staff to settle by hand, so it never starves newer ones.
func TestRetryPendingFolioRestsStopsAfterTheCap(t *testing.T) {
	svc, client, rec := newRefundTestService(t)
	rec.mu.Lock()
	rec.failRefType = RestRefundReferenceType
	rec.mu.Unlock()
	stuck := seedPendingRest(t, client, time.Now().Add(-2*time.Hour))
	ctx := context.Background()

	for i := 1; i <= maxFolioRestAttempts; i++ {
		svc.RetryPendingFolioRests(ctx)
		md := client.POSReturn.GetX(ctx, stuck.ID).Metadata
		if i < maxFolioRestAttempts {
			if md[MetaFolioRestAttempts] != float64(i) || md[MetaFolioRestPending] != true {
				t.Fatalf("after attempt %d: attempts %v pending %v", i, md[MetaFolioRestAttempts], md[MetaFolioRestPending])
			}
		}
	}
	md := client.POSReturn.GetX(ctx, stuck.ID).Metadata
	if _, pending := md[MetaFolioRestPending]; pending || md[MetaFolioRestFailed] != true {
		t.Fatalf("after %d attempts: pending %v failed %v, want not pending and failed", maxFolioRestAttempts, md[MetaFolioRestPending], md[MetaFolioRestFailed])
	}

	// A newer rest still goes through once treasury accepts rests again; the failed one is left alone.
	rec.mu.Lock()
	rec.failRefType = ""
	rec.mu.Unlock()
	fresh := seedPendingRest(t, client, time.Now())
	if n := svc.RetryPendingFolioRests(ctx); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	if got := rec.sent(); len(got) != 1 || got[0].ReferenceID != restRefundID(fresh.ID).String() {
		t.Fatalf("sent %+v, want only the fresh rest", got)
	}
}
