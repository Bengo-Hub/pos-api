package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
)

// Review M-5: a sale edit's extra amount on a room-charged sale goes on the same stay's folio,
// written exactly as the till's Charge to Room writes it, and only once per key.
func TestChargeSaleEditToRoomAddsToTheOpenStay(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	order := seedOpenOrder(t, client, tid, 500)
	guest := seedStay(t, client, tid, true)
	if _, err := chargeRoom(svc, order, guest.RoomID, 0); err != nil {
		t.Fatalf("room charge: %v", err)
	}
	order = client.POSOrder.GetX(ctx, order.ID)

	if stay, err := RoomChargeStayOpen(ctx, client, tid, order.ID); err != nil || stay == nil || stay.ID != guest.ID {
		t.Fatalf("RoomChargeStayOpen = %v, %v; want the open stay", stay, err)
	}

	key := "sale_edit:" + uuid.NewString()
	for i := 0; i < 2; i++ { // the second call is a retry: nothing new is written
		if err := ChargeSaleEditToRoom(ctx, client, order, 150, key, uuid.Nil); err != nil {
			t.Fatalf("charge %d: %v", i, err)
		}
	}

	var lines, editLines int
	for _, it := range client.RoomFolioItem.Query().Where(roomfolioitem.RoomGuestID(guest.ID)).AllX(ctx) {
		lines++
		if it.Amount == 150 {
			editLines++
			if !IsRoomChargeFolioLine(it) || it.Metadata[roomChargeKeyMeta] != key || *it.PosOrderID != order.ID {
				t.Errorf("edit folio line = %+v, want a source-marked till room charge line with the key", it)
			}
		}
	}
	if lines != 2 || editLines != 1 {
		t.Fatalf("folio lines = %d (edit lines %d), want the sale's line plus one edit line", lines, editLines)
	}
	pays := client.POSPayment.Query().Where(pospayment.OrderID(order.ID), pospayment.Amount(150)).AllX(ctx)
	if len(pays) != 1 || pays[0].Status != StatusCompleted || pays[0].PaymentData["method"] != TenderRoomCharge ||
		pays[0].PaymentData["sale_edit_increase"] != true {
		t.Fatalf("edit payments = %+v, want one completed room_charge of 150 marked sale_edit_increase", pays)
	}
	// The checkout split treats the new line as POS value: the folio owes 650 on the receivable.
	owed, err := FolioOutstanding(ctx, client, guest)
	if err != nil || owed != 650 {
		t.Errorf("folio outstanding = %v, %v; want 650", owed, err)
	}
}

// After checkout there is no open stay to charge: the edit is refused with ErrRoomStayClosed and
// nothing is written. A sale never charged to a room is refused the same way.
func TestChargeSaleEditToRoomRefusedWithoutAnOpenStay(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	order := seedOpenOrder(t, client, tid, 500)
	guest := seedStay(t, client, tid, true)
	if _, err := chargeRoom(svc, order, guest.RoomID, 0); err != nil {
		t.Fatalf("room charge: %v", err)
	}
	client.RoomGuest.UpdateOneID(guest.ID).SetStatus(roomguest.StatusCheckedOut).ExecX(ctx)
	order = client.POSOrder.GetX(ctx, order.ID)

	if err := ChargeSaleEditToRoom(ctx, client, order, 150, "k1", uuid.Nil); !errors.Is(err, ErrRoomStayClosed) {
		t.Errorf("charge after checkout = %v, want ErrRoomStayClosed", err)
	}
	if n := client.RoomFolioItem.Query().Where(roomfolioitem.RoomGuestID(guest.ID)).CountX(ctx); n != 1 {
		t.Errorf("%d folio lines after a refused charge, want 1", n)
	}

	cash := seedPaidOrder(t, client, tid, paid{"cash", 100, StatusCompleted})
	if stay, err := RoomChargeStayOpen(ctx, client, tid, cash.ID); err != nil || stay != nil {
		t.Errorf("RoomChargeStayOpen on a cash sale = %v, %v; want nil", stay, err)
	}
	if err := ChargeSaleEditToRoom(ctx, client, cash, 50, "k2", uuid.Nil); !errors.Is(err, ErrRoomStayClosed) {
		t.Errorf("charge on a cash sale = %v, want ErrRoomStayClosed", err)
	}
}

// Review M-1 follow-through: a hotel desk payment taken online stays pending until treasury
// reports it. Success completes the folio payment (so the folio counts it and checkout can go
// ahead); failure marks it failed (so its reserved POS share is released).
func TestTreasuryOutcomeResolvesPendingFolioPayments(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	guest := seedStay(t, client, tid, true)
	pending := func(intent string) uuid.UUID {
		return client.RoomFolioPayment.Create().SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).
			SetAmount(800).SetMethod("mpesa_stk").SetStatus(StatusPending).SetTreasuryIntentID(intent).
			SaveX(ctx).ID
	}
	ok, bad := pending("intent-ok"), pending("intent-bad")

	if err := svc.ConfirmPaymentByIntentID(ctx, tid, "intent-ok", 750, ""); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := svc.FailPaymentByIntentID(ctx, "intent-bad"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if p := client.RoomFolioPayment.GetX(ctx, ok); p.Status != StatusCompleted || p.Amount != 750 {
		t.Errorf("confirmed folio payment = %s %v, want completed at the settled 750", p.Status, p.Amount)
	}
	if p := client.RoomFolioPayment.GetX(ctx, bad); p.Status != StatusFailed {
		t.Errorf("failed folio payment status = %s, want failed", p.Status)
	}
	// An intent nobody holds is still an error, as before.
	if err := svc.ConfirmPaymentByIntentID(ctx, tid, "intent-unknown", 0, ""); err == nil {
		t.Error("confirm of an unknown intent succeeded, want an error")
	}
}
