package reversals

import (
	"context"
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

// A sale void or edit with no channel chosen refunds the way the sale was paid; an explicit
// choice and the on-account offset keep working as before.
func TestResolveRefundChannelUsesTheOriginalMethod(t *testing.T) {
	svc, client := newTestReversalsService(t, nil)
	ctx := context.Background()
	tid := uuid.New()

	order := setupOrderWithPayment(t, client, tid, 100)
	p := client.POSPayment.Query().OnlyX(ctx)
	client.POSPayment.UpdateOne(p).SetPaymentData(map[string]any{"method": "mtn_momo_manual"}).ExecX(ctx)

	if got := svc.resolveRefundChannel(ctx, tid, order.ID, ""); got != "mtn_momo_manual" {
		t.Errorf("MTN sale reverses via %q, want mtn_momo_manual", got)
	}
	if got := svc.resolveRefundChannel(ctx, tid, order.ID, "cash"); got != "cash" {
		t.Errorf("explicit cash became %q", got)
	}

	cash := setupOrderWithPayment(t, client, tid, 50)
	if got := svc.resolveRefundChannel(ctx, tid, cash.ID, ""); got != "cash" {
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
		SaveX(ctx)
	return guest
}

// A voided or edited room-charged sale is credited to the open stay's folio, and treasury
// reverses it as cash; once the stay is closed the channel resolves to cash.
func TestRoomChargedSaleReversalCreditsTheOpenStay(t *testing.T) {
	rec, tc := newRecordingTreasuryServer(t)
	svc, client := newTestReversalsService(t, tc)
	ctx := context.Background()
	tid := uuid.New()

	order := setupOrderWithPayment(t, client, tid, 300)
	p := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).OnlyX(ctx)
	client.POSPayment.UpdateOne(p).SetPaymentData(map[string]any{"method": "room_charge", "reversal": map[string]any{
		"reversal_number": "REV-R1", "netted_from": 300.0, "netted_to": 100.0,
	}}).ExecX(ctx)
	guest := seedRoomStayFor(t, client, order, true)

	if got := svc.resolveRefundChannel(ctx, tid, order.ID, ""); got != payments.RefundChannelRoomFolio {
		t.Fatalf("open stay: channel = %q, want room_folio", got)
	}
	rev := client.POSReversal.Create().
		SetTenantID(tid).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-R1").SetScope("partial").SetStatus("pending").SetReason("wrong item").
		SetRefundChannel(payments.RefundChannelRoomFolio).SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(200).SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		SaveX(ctx)

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
		if c.RefundChannel != "cash" {
			t.Errorf("treasury refund_channel = %q, want cash", c.RefundChannel)
		}
	}
	rec.mu.Unlock()

	closed := setupOrderWithPayment(t, client, tid, 50)
	cp := client.POSPayment.Query().Where(entpospayment.OrderID(closed.ID)).OnlyX(ctx)
	client.POSPayment.UpdateOne(cp).SetPaymentData(map[string]any{"method": "room_charge"}).ExecX(ctx)
	seedRoomStayFor(t, client, closed, false)
	if got := svc.resolveRefundChannel(ctx, tid, closed.ID, ""); got != "cash" {
		t.Errorf("closed stay: channel = %q, want cash", got)
	}
}

// A folio already settled (or a stay that closed after the reversal was created) gets no
// credit: the refund goes out in cash and the reversal records cash, not room_folio. A
// part-settled folio is credited only what it still owes.
func TestRoomChargedSaleReversalCapsTheFolioCredit(t *testing.T) {
	for _, tc := range []struct {
		name          string
		paid          float64 // already paid on the 300 folio
		open          bool
		credit        float64
		storedChannel string
	}{
		{"settled folio", 300, true, 0, "cash"},
		{"part-settled folio", 250, true, 50, payments.RefundChannelRoomFolio},
		{"stay closed since", 0, false, 0, "cash"},
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
			if len(rec.calls) != 1 || rec.calls[0].RefundChannel != "cash" || rec.calls[0].Amount != 200 {
				t.Errorf("treasury calls = %+v, want one cash refund of 200", rec.calls)
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
