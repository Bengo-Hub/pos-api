package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
)

// seedRoomChargedSale is a till sale charged to a room stay: the folio carries the order id.
func seedRoomChargedSale(t *testing.T, client *ent.Client, tid uuid.UUID, stayActive bool) (*ent.POSOrder, *ent.RoomGuest) {
	t.Helper()
	ctx := context.Background()
	order := seedPaidOrder(t, client, tid, paid{"room_charge", 100, StatusCompleted})
	status := roomguest.StatusActive
	if !stayActive {
		status = roomguest.StatusCheckedOut
	}
	// The stay and its folio line both hold a foreign key to a real room.
	room := client.Room.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetRoomNumber("R" + uuid.NewString()[:6]).SetName("Room").
		SetRatePerNight(1000).
		SaveX(ctx)
	guest := client.RoomGuest.Create().
		SetTenantID(tid).SetRoomID(room.ID).SetGuestName("Jane").SetPhone("+254712345678").SetIDNumber("ID-1").
		SetCheckInDate(time.Now()).SetNights(1).SetCheckOutDate(time.Now().AddDate(0, 0, 1)).
		SetTotalRoomCharge(1000).SetCheckedInBy(uuid.New()).SetStatus(status).
		SaveX(ctx)
	client.RoomFolioItem.Create().
		SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).SetDescription("Restaurant bill").
		SetAmount(100).SetChargeType(roomfolioitem.ChargeTypeOther).SetCreatedBy(uuid.New()).SetPosOrderID(order.ID).
		SaveX(ctx)
	return order, guest
}

// A room-charged sale refunds to the room folio, whatever money tender came second.
func TestRoomChargeRefundsToTheFolio(t *testing.T) {
	if got := RefundChannelForMethod("room_charge"); got != RefundChannelRoomFolio {
		t.Errorf("room_charge refunds via %q, want %q", got, RefundChannelRoomFolio)
	}
	if nonMoneyTenders["room_charge"] {
		t.Error("room_charge is a real charge the guest pays at checkout; it must not be skipped")
	}
}

// The open stay a sale was charged to is found only while it is open, and only in its tenant.
func TestOpenRoomStayForOrder(t *testing.T) {
	_, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()

	open, guest := seedRoomChargedSale(t, client, tid, true)
	if got := OpenRoomStayForOrder(ctx, client, tid, open.ID); got == nil || got.ID != guest.ID {
		t.Errorf("open stay = %v, want %s", got, guest.ID)
	}
	if got := OpenRoomStayForOrder(ctx, client, uuid.New(), open.ID); got != nil {
		t.Error("another tenant must not find the stay")
	}
	closed, _ := seedRoomChargedSale(t, client, tid, false)
	if got := OpenRoomStayForOrder(ctx, client, tid, closed.ID); got != nil {
		t.Error("a checked-out stay is not open")
	}
	plain := seedPaidOrder(t, client, tid, paid{"cash", 100, StatusCompleted})
	if got := OpenRoomStayForOrder(ctx, client, tid, plain.ID); got != nil {
		t.Error("a sale never charged to a room has no stay")
	}
}

// The credit is a completed folio payment that lowers the balance, recorded once per reference.
func TestCreditRoomFolioIsIdempotent(t *testing.T) {
	_, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	_, guest := seedRoomChargedSale(t, client, tid, true)
	src := uuid.New()

	for i := 0; i < 2; i++ {
		created, err := CreditRoomFolio(ctx, client, guest, 60, "RET-9", src, uuid.New())
		if err != nil {
			t.Fatalf("credit %d: %v", i, err)
		}
		if created != (i == 0) {
			t.Errorf("credit %d: created = %v, want %v", i, created, i == 0)
		}
	}
	rows := client.RoomFolioPayment.Query().AllX(ctx)
	if len(rows) != 1 {
		t.Fatalf("%d folio rows, want 1", len(rows))
	}
	p := rows[0]
	if p.Method != FolioReturnCreditMethod || p.Amount != 60 || p.Status != "completed" || p.Reference != "RET-9" ||
		p.RoomGuestID != guest.ID || p.TenantID != tid || p.Metadata["source_id"] != src.String() {
		t.Errorf("credit row = %+v", p)
	}
	if _, err := CreditRoomFolio(ctx, client, guest, 0, "RET-10", uuid.New(), uuid.Nil); err == nil {
		t.Error("a zero credit must be refused")
	}
}

// The folio still owes its charges less its completed payments, never below zero. Pending and
// failed payments do not count.
func TestFolioOutstanding(t *testing.T) {
	_, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	_, guest := seedRoomChargedSale(t, client, tid, true) // one charge of 100

	if got := FolioOutstanding(ctx, client, guest); got != 100 {
		t.Errorf("unpaid folio owes %v, want 100", got)
	}
	pay := func(amount float64, status string) {
		client.RoomFolioPayment.Create().
			SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).
			SetAmount(amount).SetMethod("cash").SetStatus(status).
			SaveX(ctx)
	}
	pay(30, "completed")
	pay(50, "pending")
	if got := FolioOutstanding(ctx, client, guest); got != 70 {
		t.Errorf("part-paid folio owes %v, want 70", got)
	}
	pay(90, "completed")
	if got := FolioOutstanding(ctx, client, guest); got != 0 {
		t.Errorf("overpaid folio owes %v, want 0", got)
	}
}

// A concurrent second insert of the same credit hits the unique index and is reported as
// already recorded, not as an error.
func TestCreditRoomFolioUniqueIndexBacksIdempotency(t *testing.T) {
	_, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	_, guest := seedRoomChargedSale(t, client, tid, true)

	// Simulate the other writer: the row exists but this caller's existence check missed it.
	client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).
		SetAmount(40).SetMethod(FolioReturnCreditMethod).SetReference("RET-7").SetStatus("completed").
		SaveX(ctx)
	if _, err := client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).
		SetAmount(40).SetMethod(FolioReturnCreditMethod).SetReference("RET-7").SetStatus("completed").
		Save(ctx); err == nil {
		t.Fatal("a second return credit with the same reference must hit the unique index")
	}
	// The same reference on an ordinary payment is not covered by the partial index.
	client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).
		SetAmount(10).SetMethod("cash").SetReference("RET-7").SetStatus("completed").
		SaveX(ctx)
	if created, err := CreditRoomFolio(ctx, client, guest, 40, "RET-7", uuid.New(), uuid.Nil); err != nil || created {
		t.Errorf("CreditRoomFolio = created %v err %v, want an idempotent no-op", created, err)
	}
}

func TestSplitFolioCredit(t *testing.T) {
	for _, c := range []struct{ amount, owed, folio, rest float64 }{
		{250, 1000, 250, 0},  // the folio owes more than the refund
		{250, 100, 100, 150}, // part-settled folio
		{250, 0, 0, 250},     // settled folio: all by the cashier's channel
		{0, 100, 0, 0},
	} {
		f, r := SplitFolioCredit(c.amount, c.owed)
		if f != c.folio || r != c.rest {
			t.Errorf("SplitFolioCredit(%v, %v) = %v, %v; want %v, %v", c.amount, c.owed, f, r, c.folio, c.rest)
		}
	}
}
