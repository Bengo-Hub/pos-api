package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// seedOpenOrder is an unpaid till sale of total, still open.
func seedOpenOrder(t *testing.T, client *ent.Client, tid uuid.UUID, total float64) *ent.POSOrder {
	t.Helper()
	return client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus(orders.StatusOpen).
		SetSubtotal(total).SetTaxTotal(0).SetTotalAmount(total).SetPaidTotal(0).
		SaveX(context.Background())
}

// seedStay is a room with a guest in it (active) or checked out.
func seedStay(t *testing.T, client *ent.Client, tid uuid.UUID, active bool) *ent.RoomGuest {
	t.Helper()
	ctx := context.Background()
	status := roomguest.StatusActive
	if !active {
		status = roomguest.StatusCheckedOut
	}
	room := client.Room.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetRoomNumber("R" + uuid.NewString()[:6]).SetName("Room").
		SetRatePerNight(1000).
		SaveX(ctx)
	return client.RoomGuest.Create().
		SetTenantID(tid).SetRoomID(room.ID).SetGuestName("Jane").SetPhone("+254712345678").SetIDNumber("ID-1").
		SetCheckInDate(time.Now()).SetNights(1).SetCheckOutDate(time.Now().AddDate(0, 0, 1)).
		SetTotalRoomCharge(1000).SetCheckedInBy(uuid.New()).SetStatus(status).
		SaveX(ctx)
}

func chargeRoom(svc *Service, order *ent.POSOrder, roomID uuid.UUID, amount float64) (*CreateIntentResult, error) {
	return svc.CreatePaymentIntent(context.Background(), RecordPaymentRequest{
		TenantID: order.TenantID, TenantSlug: "hotel", OrderID: order.ID, TenderID: uuid.New(),
		TenderMethod: "Room_Charge", Amount: amount, RoomID: roomID,
	})
}

// Charge to Room writes the folio line and a completed room_charge payment together and
// completes the order (the old till flow left it open), with no treasury client needed: a room
// charge takes no money at the till, so no intent is created.
func TestRoomChargePostsTheFolioLineAndCompletesTheOrder(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	order := seedOpenOrder(t, client, tid, 500)
	guest := seedStay(t, client, tid, true)

	res, err := chargeRoom(svc, order, guest.RoomID, 0)
	if err != nil || res == nil || !res.IsCash {
		t.Fatalf("room charge = %+v, %v; want settled", res, err)
	}

	items := client.RoomFolioItem.Query().Where(roomfolioitem.TenantID(tid)).AllX(ctx)
	if len(items) != 1 {
		t.Fatalf("%d folio lines, want 1", len(items))
	}
	it := items[0]
	if it.PosOrderID == nil || *it.PosOrderID != order.ID || it.RoomGuestID != guest.ID || it.Amount != 500 ||
		it.ChargeType != roomfolioitem.ChargeTypeRestaurant || !IsRoomChargeFolioLine(it) {
		t.Errorf("folio line = %+v, want 500 restaurant on the stay, linked to the order and marked as a till room charge", it)
	}

	pays := client.POSPayment.Query().Where(pospayment.OrderID(order.ID)).AllX(ctx)
	if len(pays) != 1 || pays[0].Status != StatusCompleted || pays[0].Amount != 500 ||
		pays[0].PaymentData["method"] != TenderRoomCharge || pays[0].PaymentData["folio_item_id"] != it.ID.String() {
		t.Fatalf("payments = %+v, want one completed room_charge of 500 pointing at the folio line", pays)
	}
	// Set at creation, so the treasury intent reconciler never treats it as a cash payment
	// still waiting for an intent.
	if pays[0].ExternalReference != order.OrderNumber {
		t.Errorf("external_reference = %q, want the order number", pays[0].ExternalReference)
	}
	if isCashMethod(TenderRoomCharge) {
		t.Error("a room charge is not an immediate cash tender")
	}

	if got := client.POSOrder.Query().Where(posorder.ID(order.ID)).OnlyX(ctx); got.Status != orders.StatusCompleted {
		t.Errorf("order status = %q, want completed", got.Status)
	}

	scheme, onAccount, _, _, tenders := svc.resolveSaleSettlement(ctx, client.POSOrder.GetX(ctx, order.ID))
	if scheme != "room_charge" || onAccount != 0 || roomChargeTotal(tenders) != 500 {
		t.Errorf("settlement = %q on_account %v room %v, want room_charge 0 500", scheme, onAccount, roomChargeTotal(tenders))
	}
}

// A retried room charge (double tap, lost response) finds the folio line and changes nothing,
// even though the first call already completed the order.
func TestRoomChargeRetryIsIdempotent(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	order := seedOpenOrder(t, client, tid, 300)
	guest := seedStay(t, client, tid, true)

	for i := 0; i < 3; i++ {
		if res, err := chargeRoom(svc, order, guest.RoomID, 300); err != nil || !res.IsCash {
			t.Fatalf("attempt %d: %+v, %v", i, res, err)
		}
	}
	if n := client.RoomFolioItem.Query().Where(roomfolioitem.PosOrderID(order.ID)).CountX(ctx); n != 1 {
		t.Errorf("%d folio lines after retries, want 1", n)
	}
	if n := client.POSPayment.Query().Where(pospayment.OrderID(order.ID)).CountX(ctx); n != 1 {
		t.Errorf("%d payments after retries, want 1", n)
	}
}

// Only a checked-in guest can be charged, and the room must be named; nothing is written
// otherwise.
func TestRoomChargeNeedsACheckedInGuest(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	order := seedOpenOrder(t, client, tid, 200)

	gone := seedStay(t, client, tid, false)
	if _, err := chargeRoom(svc, order, gone.RoomID, 200); !errors.Is(err, ErrRoomChargeNoGuest) {
		t.Errorf("checked-out room: err = %v, want ErrRoomChargeNoGuest", err)
	}
	other := seedStay(t, client, uuid.New(), true) // another tenant's guest
	if _, err := chargeRoom(svc, order, other.RoomID, 200); !errors.Is(err, ErrRoomChargeNoGuest) {
		t.Errorf("another tenant's room: err = %v, want ErrRoomChargeNoGuest", err)
	}
	if _, err := chargeRoom(svc, order, uuid.Nil, 200); err == nil {
		t.Error("no room: want an error")
	}
	if n := client.RoomFolioItem.Query().CountX(ctx); n != 0 {
		t.Errorf("%d folio lines written, want 0", n)
	}
	if n := client.POSPayment.Query().CountX(ctx); n != 0 {
		t.Errorf("%d payments written, want 0", n)
	}
	if got := client.POSOrder.GetX(ctx, order.ID); got.Status != orders.StatusOpen {
		t.Errorf("order status = %q, want still open", got.Status)
	}
}

// Part cash, rest charged to the room: the room charge takes only the outstanding balance and
// the sale reaches treasury as "mixed" with room_charge_amount.
func TestRoomChargeForTheRestOfASplitSale(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	order := seedOpenOrder(t, client, tid, 500)
	client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(200).SetCurrency("KES").
		SetStatus(StatusCompleted).SetPaymentData(map[string]any{"method": "cash"}).SetExternalReference("intent-1").
		SaveX(ctx)
	guest := seedStay(t, client, tid, true)

	if _, err := chargeRoom(svc, order, guest.RoomID, 500); err != nil {
		t.Fatal(err)
	}
	item := client.RoomFolioItem.Query().Where(roomfolioitem.PosOrderID(order.ID)).OnlyX(ctx)
	if item.Amount != 300 {
		t.Errorf("folio line = %v, want the 300 outstanding", item.Amount)
	}
	scheme, _, _, _, tenders := svc.resolveSaleSettlement(ctx, client.POSOrder.GetX(ctx, order.ID))
	if scheme != "mixed" || roomChargeTotal(tenders) != 300 {
		t.Errorf("settlement = %q room %v, want mixed 300", scheme, roomChargeTotal(tenders))
	}
	if got := client.POSOrder.GetX(ctx, order.ID); got.Status != orders.StatusCompleted {
		t.Errorf("order status = %q, want completed", got.Status)
	}
}

// A folio line is a till room charge only when the room charge wrote it: a line posted by hand
// with a pos_order_id is still revenue at checkout.
func TestIsRoomChargeFolioLine(t *testing.T) {
	oid := uuid.New()
	for _, tc := range []struct {
		name string
		it   *ent.RoomFolioItem
		want bool
	}{
		{"till room charge", &ent.RoomFolioItem{PosOrderID: &oid, Metadata: map[string]any{"source": "pos_room_charge"}}, true},
		{"hand-posted with an order id", &ent.RoomFolioItem{PosOrderID: &oid, Metadata: map[string]any{}}, false},
		{"minibar", &ent.RoomFolioItem{Metadata: map[string]any{"source": "pos_room_charge"}}, false},
		{"nil", nil, false},
	} {
		if got := IsRoomChargeFolioLine(tc.it); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A room charge refunds to the folio, and treasury now gets room_folio for it, not cash: the
// till booked the charge to the guest receivable.
func TestRoomFolioRefundGoesToTreasuryAsRoomFolio(t *testing.T) {
	if got := TreasuryRefundChannel(RefundChannelRoomFolio); got != RefundChannelRoomFolio {
		t.Errorf("TreasuryRefundChannel(room_folio) = %q, want room_folio", got)
	}
	if got := TreasuryRefundChannel("mpesa"); got != "mpesa" {
		t.Errorf("TreasuryRefundChannel(mpesa) = %q, want mpesa", got)
	}
}

func chargeRoomKeyed(svc *Service, order *ent.POSOrder, roomID uuid.UUID, amount float64, key string) (*CreateIntentResult, error) {
	return svc.CreatePaymentIntent(context.Background(), RecordPaymentRequest{
		TenantID: order.TenantID, TenantSlug: "hotel", OrderID: order.ID, TenderID: uuid.New(),
		TenderMethod: TenderRoomCharge, Amount: amount, RoomID: roomID, RoomChargeKey: key,
	})
}

// A bill split across two rooms posts both charges (review I-5): the idempotency key is the
// request, not the order. A retry of the first charge with its key still returns the first
// result and writes nothing, even after the second charge completed the order.
func TestRoomChargeSplitAcrossTwoRooms(t *testing.T) {
	for _, keyed := range []bool{true, false} {
		svc, client := newTestPaymentsService(t)
		ctx := context.Background()
		tid := uuid.New()
		order := seedOpenOrder(t, client, tid, 1000)
		a, b := seedStay(t, client, tid, true), seedStay(t, client, tid, true)
		k1, k2 := "", ""
		if keyed {
			k1, k2 = "charge-1", "charge-2"
		}

		if _, err := chargeRoomKeyed(svc, order, a.RoomID, 600, k1); err != nil {
			t.Fatalf("keyed=%v first room: %v", keyed, err)
		}
		if got := client.POSOrder.GetX(ctx, order.ID); got.Status != orders.StatusOpen {
			t.Errorf("keyed=%v: order %q after a part charge, want still open", keyed, got.Status)
		}
		if _, err := chargeRoomKeyed(svc, order, b.RoomID, 400, k2); err != nil {
			t.Fatalf("keyed=%v second room: %v", keyed, err)
		}
		if _, err := chargeRoomKeyed(svc, order, a.RoomID, 600, k1); err != nil {
			t.Fatalf("keyed=%v retry of the first charge: %v", keyed, err)
		}

		lines := client.RoomFolioItem.Query().Where(roomfolioitem.PosOrderID(order.ID)).AllX(ctx)
		byGuest := map[uuid.UUID]float64{}
		for _, l := range lines {
			byGuest[l.RoomGuestID] += l.Amount
		}
		if len(lines) != 2 || byGuest[a.ID] != 600 || byGuest[b.ID] != 400 {
			t.Errorf("keyed=%v: folio lines %v, want 600 on the first stay and 400 on the second", keyed, byGuest)
		}
		if n := client.POSPayment.Query().Where(pospayment.OrderID(order.ID)).CountX(ctx); n != 2 {
			t.Errorf("keyed=%v: %d payments, want 2", keyed, n)
		}
		if got := client.POSOrder.GetX(ctx, order.ID); got.Status != orders.StatusCompleted {
			t.Errorf("keyed=%v: order %q, want completed", keyed, got.Status)
		}
	}
}

// Two charges of the same amount to the same room are told apart by their keys.
func TestRoomChargeSameRoomTwiceWithKeys(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	order := seedOpenOrder(t, client, tid, 1000)
	g := seedStay(t, client, tid, true)
	for _, k := range []string{"half-1", "half-2", "half-1"} {
		if _, err := chargeRoomKeyed(svc, order, g.RoomID, 500, k); err != nil {
			t.Fatalf("key %s: %v", k, err)
		}
	}
	if n := client.RoomFolioItem.Query().Where(roomfolioitem.PosOrderID(order.ID)).CountX(ctx); n != 2 {
		t.Errorf("%d folio lines, want 2", n)
	}
	if got := client.POSOrder.GetX(ctx, order.ID); got.Status != orders.StatusCompleted {
		t.Errorf("order %q, want completed", got.Status)
	}
}

// A folio line posted by hand with the order id (no till source marker) never blocks a room charge.
func TestRoomChargeIgnoresHandPostedLines(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	order := seedOpenOrder(t, client, tid, 300)
	g := seedStay(t, client, tid, true)
	client.RoomFolioItem.Create().
		SetTenantID(tid).SetRoomID(g.RoomID).SetRoomGuestID(g.ID).SetDescription("Restaurant bill").
		SetAmount(300).SetChargeType(roomfolioitem.ChargeTypeRestaurant).SetCreatedBy(uuid.New()).SetPosOrderID(order.ID).
		SaveX(ctx)

	if _, err := chargeRoom(svc, order, g.RoomID, 300); err != nil {
		t.Fatal(err)
	}
	if n := client.POSPayment.Query().Where(pospayment.OrderID(order.ID)).CountX(ctx); n != 1 {
		t.Errorf("%d payments, want 1 (the hand-posted line must not count as the charge)", n)
	}
	if got := client.POSOrder.GetX(ctx, order.ID); got.Status != orders.StatusCompleted {
		t.Errorf("order %q, want completed", got.Status)
	}
}

// Complimentary and room charge never mix on one sale (review I-3), whichever comes first.
func TestRoomChargeAndComplimentaryRefuseEachOther(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	g := seedStay(t, client, tid, true)

	comped := seedOpenOrder(t, client, tid, 1000)
	client.POSPayment.Create().
		SetOrderID(comped.ID).SetTenderID(uuid.New()).SetAmount(400).SetCurrency("KES").
		SetStatus(StatusCompleted).SetPaymentData(map[string]any{"method": TenderComplimentary, "reason": "goodwill"}).
		SaveX(ctx)
	if _, err := chargeRoom(svc, comped, g.RoomID, 600); !errors.Is(err, ErrRoomChargeWithComplimentary) {
		t.Errorf("room charge after a complimentary part: err = %v, want ErrRoomChargeWithComplimentary", err)
	}
	if n := client.RoomFolioItem.Query().CountX(ctx); n != 0 {
		t.Errorf("%d folio lines written, want 0", n)
	}

	roomed := seedOpenOrder(t, client, tid, 1000)
	if _, err := chargeRoom(svc, roomed, g.RoomID, 600); err != nil {
		t.Fatal(err)
	}
	fresh := client.POSOrder.GetX(ctx, roomed.ID)
	_, err := svc.recordComplimentarySale(ctx, fresh, RecordPaymentRequest{
		TenantID: tid, OrderID: roomed.ID, TenderID: uuid.New(), TenderMethod: TenderComplimentary, Amount: 400, ComplimentaryReason: "goodwill",
	}, "KES")
	if !errors.Is(err, ErrRoomChargeWithComplimentary) {
		t.Errorf("complimentary after a room charge: err = %v, want ErrRoomChargeWithComplimentary", err)
	}
	// The cashier is told how to do it instead (owner decision 2026-11-13): comp the items as a
	// discount and charge the net to the room; whole-bill comps keep Complimentary.
	msg := ErrRoomChargeWithComplimentary.Error()
	for _, want := range []string{"comp them as a discount", "manager approval", "charge the rest to the room", "whole bill"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message %q does not say %q", msg, want)
		}
	}
}

// A folio credit for a mixed cash and room sale is capped at the room-charged part, less the
// credits already made for the sale's other returns (review I-1); its own earlier credit does not
// count against a retry.
func TestFolioCreditLimitCapsAtTheRoomCharge(t *testing.T) {
	_, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	g := seedStay(t, client, tid, true)
	order := seedOpenOrder(t, client, tid, 1160)
	for _, p := range []struct {
		method string
		amount float64
	}{{"cash", 500}, {TenderRoomCharge, 660}} {
		client.POSPayment.Create().
			SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(p.amount).SetCurrency("KES").
			SetStatus(StatusCompleted).SetPaymentData(map[string]any{"method": p.method}).
			SaveX(ctx)
	}
	for _, l := range []struct {
		amount float64
		ct     roomfolioitem.ChargeType
		meta   map[string]any
	}{
		{2000, roomfolioitem.ChargeTypeRoomCharge, map[string]any{}},
		{660, roomfolioitem.ChargeTypeRestaurant, map[string]any{"source": "pos_room_charge"}},
	} {
		c := client.RoomFolioItem.Create().
			SetTenantID(tid).SetRoomID(g.RoomID).SetRoomGuestID(g.ID).SetDescription("line").
			SetAmount(l.amount).SetChargeType(l.ct).SetCreatedBy(uuid.New()).SetMetadata(l.meta)
		if l.ct == roomfolioitem.ChargeTypeRestaurant {
			c = c.SetPosOrderID(order.ID)
		}
		c.SaveX(ctx)
	}

	if got := FolioCreditLimit(ctx, client, g, order.ID, "RET-1"); got != 660 {
		t.Errorf("limit for a full return = %v, want 660 (the room part, not the 1,160 sale)", got)
	}
	ret := client.POSReturn.Create().
		SetTenantID(tid).SetOrderID(order.ID).SetOutletID(order.OutletID).
		SetReturnNumber("RET-1").SetStatus("completed").SetReturnType("refund").SetReason("test").
		SetRefundAmount(400).SetRequestedBy(uuid.New()).
		SaveX(ctx)
	if _, err := CreditRoomFolio(ctx, client, g, 400, "RET-1", ret.ID, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if got := FolioCreditLimit(ctx, client, g, order.ID, "RET-2"); got != 260 {
		t.Errorf("limit after a 400 credit = %v, want 260", got)
	}
	if got := FolioCreditLimit(ctx, client, g, order.ID, "RET-1"); got != 660 {
		t.Errorf("limit for RET-1's own retry = %v, want 660", got)
	}
}
