package reversals

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/payments"
)

// channelFor is resolveRefundChannel for a test that expects the lookups to work.
func channelFor(t *testing.T, svc *Service, tid, orderID uuid.UUID, requested string) string {
	t.Helper()
	ch, err := svc.resolveRefundChannel(context.Background(), tid, orderID, requested)
	if err != nil {
		t.Fatalf("resolveRefundChannel: %v", err)
	}
	return ch
}

// A failed stay lookup must not read as "stay closed": the channel would fall to cash for a
// room-charged sale. The reversal is refused instead, so it can be tried again.
func TestResolveRefundChannelFailsOnAStayLookupError(t *testing.T) {
	svc, client := newTestReversalsService(t, nil)
	ctx := context.Background()
	tid := uuid.New()
	order, _ := seedSaleWithLine(t, client, tid, 3, 100, pay{"room_charge", 300, 0})
	seedRoomStayFor(t, client, order, true)

	client.RoomGuest.Intercept(ent.InterceptFunc(func(ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(context.Context, ent.Query) (ent.Value, error) {
			return nil, errors.New("room stays unavailable")
		})
	}))
	if ch, err := svc.resolveRefundChannel(ctx, tid, order.ID, ""); err == nil {
		t.Fatalf("channel = %q with no error, want an error (never cash on a failed lookup)", ch)
	}
}

// A sale void or edit with no channel chosen refunds the way the sale was paid; an explicit
// choice and the on-account offset keep working as before.
func TestResolveRefundChannelUsesTheOriginalMethod(t *testing.T) {
	svc, client := newTestReversalsService(t, nil)
	ctx := context.Background()
	tid := uuid.New()

	order := setupOrderWithPayment(t, client, tid, 100)
	p := client.POSPayment.Query().OnlyX(ctx)
	client.POSPayment.UpdateOne(p).SetPaymentData(map[string]any{"method": "mtn_momo_manual"}).ExecX(ctx)

	if got := channelFor(t, svc, tid, order.ID, ""); got != "mtn_momo_manual" {
		t.Errorf("MTN sale reverses via %q, want mtn_momo_manual", got)
	}
	if got := channelFor(t, svc, tid, order.ID, "cash"); got != "cash" {
		t.Errorf("explicit cash became %q", got)
	}

	cash := setupOrderWithPayment(t, client, tid, 50)
	if got := channelFor(t, svc, tid, cash.ID, ""); got != "cash" {
		t.Errorf("cash sale reverses via %q, want cash", got)
	}
}

// seedRoomStayFor charges the order to a room stay (the folio item carries the order id).
func seedRoomStayFor(t *testing.T, client *ent.Client, order *ent.POSOrder, open bool) *ent.RoomGuest {
	t.Helper()
	ctx := context.Background()
	status := roomguest.StatusActive
	if !open {
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
		SetAmount(order.TotalAmount).SetChargeType(roomfolioitem.ChargeTypeOther).SetCreatedBy(uuid.New()).SetPosOrderID(order.ID).
		SetMetadata(map[string]any{"source": "pos_room_charge"}). // written by the till's room charge
		SaveX(ctx)
	return guest
}

// A voided or edited room-charged sale is credited to the open stay's folio, and treasury
// reverses it against the guest receivable (refund channel room_folio, S2-20); once the stay is
// closed the channel resolves to cash.
func TestRoomChargedSaleReversalCreditsTheOpenStay(t *testing.T) {
	rec, tc := newRecordingTreasuryServer(t)
	svc, client := newTestReversalsService(t, tc)
	ctx := context.Background()
	tid := uuid.New()

	// A 300 sale (3 x 100) wholly charged to the room; the edit removes 2 of the 3.
	order, line := seedSaleWithLine(t, client, tid, 3, 100, pay{"room_charge", 300, 0})
	guest := seedRoomStayFor(t, client, order, true)

	if got := channelFor(t, svc, tid, order.ID, ""); got != payments.RefundChannelRoomFolio {
		t.Fatalf("open stay: channel = %q, want room_folio", got)
	}
	rev := client.POSReversal.Create().
		SetTenantID(tid).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-R1").SetScope("partial").SetStatus("pending").SetReason("wrong item").
		SetRefundChannel(payments.RefundChannelRoomFolio).
		SetLines([]entschema.ReversalLineJSON{{LineID: line.ID, SKU: line.Sku, Name: line.Name, Quantity: 2, OfQuantity: 3, Amount: 200}}).
		SetAmount(200).SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		SaveX(ctx)

	// The real order: stepPOSTotals nets the room_charge payment down to 100 first, then
	// stepTreasuryGL credits the folio (re-review R-1: the limit must not shrink with it).
	if _, _, _, err := svc.stepPOSTotals(ctx, rev); err != nil {
		t.Fatalf("stepPOSTotals: %v", err)
	}
	if p := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).OnlyX(ctx); p.Amount != 100 {
		t.Fatalf("room_charge payment after stepPOSTotals = %v, want netted to 100", p.Amount)
	}
	for i := 0; i < 2; i++ { // a retried step never credits twice
		if _, _, skip, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug"); err != nil || skip {
			t.Fatalf("stepTreasuryGL() error=%v skip=%v", err, skip)
		}
	}
	credits := client.RoomFolioPayment.Query().AllX(ctx)
	if len(credits) != 1 || credits[0].Amount != 200 || credits[0].RoomGuestID != guest.ID || credits[0].Method != payments.FolioReturnCreditMethod {
		t.Fatalf("folio credits = %+v, want one credit of 200 on the stay", credits)
	}
	rec.mu.Lock()
	for _, c := range rec.calls {
		if c.RefundChannel != payments.RefundChannelRoomFolio || c.Amount != 200 {
			t.Errorf("treasury refund = %v %q, want 200 room_folio", c.Amount, c.RefundChannel)
		}
	}
	rec.mu.Unlock()

	closed := setupOrderWithPayment(t, client, tid, 50)
	cp := client.POSPayment.Query().Where(entpospayment.OrderID(closed.ID)).OnlyX(ctx)
	client.POSPayment.UpdateOne(cp).SetPaymentData(map[string]any{"method": "room_charge"}).ExecX(ctx)
	seedRoomStayFor(t, client, closed, false)
	if got := channelFor(t, svc, tid, closed.ID, ""); got != "cash" {
		t.Errorf("closed stay: channel = %q, want cash", got)
	}
}

// A folio already settled (or a stay that closed after the reversal was created) gets no
// credit: the refund goes out in cash and the reversal records cash, not room_folio. A
// part-settled folio is credited only what it still owes: that part reaches treasury as
// room_folio (it reduces the guest receivable) and the rest as its own cash portion.
func TestRoomChargedSaleReversalCapsTheFolioCredit(t *testing.T) {
	type call struct {
		channel, refType string
		amount           float64
	}
	for _, tc := range []struct {
		name          string
		paid          float64 // already paid on the 300 folio
		open          bool
		credit        float64
		storedChannel string
		calls         []call
	}{
		{"settled folio", 300, true, 0, "cash", []call{{"cash", "pos_return", 200}}},
		{"part-settled folio", 250, true, 50, payments.RefundChannelRoomFolio,
			[]call{{payments.RefundChannelRoomFolio, "pos_return", 50}, {"cash", folioRestReferenceType, 150}}},
		{"stay closed since", 0, false, 0, "cash", []call{{"cash", "pos_return", 200}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, trc := newRecordingTreasuryServer(t)
			svc, client := newTestReversalsService(t, trc)
			ctx := context.Background()
			tid := uuid.New()
			order := setupOrderWithPayment(t, client, tid, 300)
			p := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).OnlyX(ctx)
			client.POSPayment.UpdateOne(p).SetPaymentData(map[string]any{"method": "room_charge", "reversal": map[string]any{
				"reversal_number": "REV-C1", "netted_from": 300.0, "netted_to": 100.0,
			}}).ExecX(ctx)
			guest := seedRoomStayFor(t, client, order, tc.open)
			if tc.paid > 0 {
				client.RoomFolioPayment.Create().
					SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).
					SetAmount(tc.paid).SetMethod("cash").SetStatus("completed").
					SaveX(ctx)
			}
			rev := client.POSReversal.Create().
				SetTenantID(tid).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
				SetReversalNumber("REV-C1").SetScope("partial").SetStatus("pending").SetReason("wrong item").
				SetRefundChannel(payments.RefundChannelRoomFolio).SetLines([]entschema.ReversalLineJSON{}).
				SetAmount(200).SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
				SaveX(ctx)

			if _, _, skip, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug"); err != nil || skip {
				t.Fatalf("stepTreasuryGL() error=%v skip=%v", err, skip)
			}
			var credited float64
			for _, c := range client.RoomFolioPayment.Query().AllX(ctx) {
				if c.Method == payments.FolioReturnCreditMethod {
					credited += c.Amount
				}
			}
			if credited != tc.credit {
				t.Errorf("folio credit = %v, want %v", credited, tc.credit)
			}
			if got := client.POSReversal.GetX(ctx, rev.ID).RefundChannel; got != tc.storedChannel {
				t.Errorf("stored channel = %q, want %q", got, tc.storedChannel)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.calls) != len(tc.calls) {
				t.Fatalf("treasury calls = %+v, want %+v", rec.calls, tc.calls)
			}
			for i, want := range tc.calls {
				got := rec.calls[i]
				if got.RefundChannel != want.channel || got.ReferenceType != want.refType || got.Amount != want.amount {
					t.Errorf("call %d = %v %q %q, want %v %q %q", i, got.Amount, got.RefundChannel, got.ReferenceType, want.amount, want.channel, want.refType)
				}
			}
			if len(rec.calls) == 2 && rec.calls[0].ReferenceID == rec.calls[1].ReferenceID {
				t.Error("the folio part and the cash rest need distinct reference ids")
			}
		})
	}
}

// A retried step after the stay closed finds the credit the first attempt made and keeps
// room_folio; it never re-records the reversal as a cash refund.
func TestRoomChargedSaleReversalRetryAfterStayClosed(t *testing.T) {
	_, trc := newRecordingTreasuryServer(t)
	svc, client := newTestReversalsService(t, trc)
	ctx := context.Background()
	tid := uuid.New()
	order := setupOrderWithPayment(t, client, tid, 300)
	p := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).OnlyX(ctx)
	client.POSPayment.UpdateOne(p).SetPaymentData(map[string]any{"method": "room_charge", "reversal": map[string]any{
		"reversal_number": "REV-R2", "netted_from": 300.0, "netted_to": 100.0,
	}}).ExecX(ctx)
	guest := seedRoomStayFor(t, client, order, true)
	rev := client.POSReversal.Create().
		SetTenantID(tid).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-R2").SetScope("partial").SetStatus("pending").SetReason("wrong item").
		SetRefundChannel(payments.RefundChannelRoomFolio).SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(200).SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		SaveX(ctx)

	if _, _, _, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug"); err != nil {
		t.Fatal(err)
	}
	client.RoomGuest.UpdateOne(guest).SetStatus(roomguest.StatusCheckedOut).ExecX(ctx)
	_, detail, _, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug")
	if err != nil {
		t.Fatal(err)
	}
	if got := client.POSReversal.GetX(ctx, rev.ID).RefundChannel; got != payments.RefundChannelRoomFolio {
		t.Errorf("stored channel after the retry = %q, want room_folio", got)
	}
	if n := client.RoomFolioPayment.Query().CountX(ctx); n != 1 {
		t.Errorf("%d folio credits, want 1", n)
	}
	if !strings.Contains(detail, "credited to the room folio") {
		t.Errorf("retry detail %q does not report the folio credit", detail)
	}
}
