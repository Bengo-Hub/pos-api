package saleedit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
	"github.com/bengobox/pos-service/internal/modules/payments"
)

// seedRoomChargedOrder is a completed sale of 500 charged wholly to a guest's room, as the till's
// Charge to Room leaves it: a source-marked folio line on the stay and a completed room_charge
// payment.
func seedRoomChargedOrder(t *testing.T, client *ent.Client, tid uuid.UUID) (*ent.POSOrder, *ent.RoomGuest) {
	t.Helper()
	ctx := context.Background()
	order := seedOrchestratorOrder(t, client, tid, uuid.New(), 5, 100)
	room := client.Room.Create().
		SetTenantID(tid).SetOutletID(order.OutletID).SetRoomNumber("R" + uuid.NewString()[:4]).SetName("Room").
		SetRatePerNight(1000).SaveX(ctx)
	guest := client.RoomGuest.Create().
		SetTenantID(tid).SetRoomID(room.ID).SetGuestName("Jane").SetPhone("+254712345678").SetIDNumber("ID-1").
		SetCheckInDate(time.Now()).SetNights(1).SetCheckOutDate(time.Now().AddDate(0, 0, 1)).
		SetTotalRoomCharge(1000).SetCheckedInBy(uuid.New()).SaveX(ctx)
	item := client.RoomFolioItem.Create().
		SetTenantID(tid).SetRoomID(room.ID).SetRoomGuestID(guest.ID).SetDescription("POS bill").
		SetAmount(500).SetChargeType(entroomfolioitem.ChargeTypeRestaurant).SetCreatedBy(uuid.New()).
		SetPosOrderID(order.ID).SetMetadata(map[string]any{"source": "pos_room_charge"}).SaveX(ctx)
	client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.Nil).SetAmount(500).SetCurrency("KES").SetStatus("completed").
		SetPaymentData(map[string]any{"method": payments.TenderRoomCharge, "room_guest_id": guest.ID.String(), "folio_item_id": item.ID.String()}).
		SaveX(ctx)
	return order, guest
}

func addOneItem(order *ent.POSOrder, line *ent.POSOrderLine, settlement string) EditSaleRequest {
	return EditSaleRequest{
		OrderID: order.ID, Reason: "guest ordered one more", RequestedBy: uuid.New(), IncreaseSettlement: settlement,
		Lines: []EditLine{
			{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 5, UnitPrice: 100},
			{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "New Item", Quantity: 1, UnitPrice: 50},
		},
	}
}

// Review M-5 (owner decision 2026-11-14): while the stay is open, an increase on a room-charged
// sale can be charged to the room. The extra goes on the same folio as a till room charge line,
// a completed room_charge payment settles it on the order, and nothing is billed on credit.
func TestEdit_IncreaseOnRoomChargedSale_ChargesTheRoom(t *testing.T) {
	svc, client := newOrchestratorTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	order, guest := seedRoomChargedOrder(t, client, tid)

	if _, err := svc.Edit(ctx, tid, addOneItem(order, onlyLine(t, client, order.ID), payments.TenderRoomCharge)); err != nil {
		t.Fatalf("room charge increase: %v", err)
	}

	lines := client.RoomFolioItem.Query().Where(entroomfolioitem.RoomGuestID(guest.ID), entroomfolioitem.Amount(50)).AllX(ctx)
	if len(lines) != 1 || !payments.IsRoomChargeFolioLine(lines[0]) || *lines[0].PosOrderID != order.ID {
		t.Fatalf("folio lines of 50 = %+v, want one source-marked till room charge line for the order", lines)
	}
	pays := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID), entpospayment.Amount(50)).AllX(ctx)
	if len(pays) != 1 || pays[0].PaymentData["method"] != payments.TenderRoomCharge || pays[0].Status != "completed" {
		t.Fatalf("payments of 50 = %+v, want one completed room_charge", pays)
	}
	reloaded := client.POSOrder.GetX(ctx, order.ID)
	if reloaded.TotalAmount != 550 || reloaded.PaidTotal != 550 {
		t.Errorf("order total %v paid %v, want 550 and 550", reloaded.TotalAmount, reloaded.PaidTotal)
	}
	if on, _ := reloaded.Metadata["on_account"].(bool); on {
		t.Error("metadata.on_account set: a room charge increase must not bill credit")
	}
}

// After checkout the option is refused with ErrRoomStayClosed (HTTP 409), and the sale is left as
// it was: no line added, nothing on the folio. Collect now still works.
func TestEdit_IncreaseRoomChargeAfterCheckout_Refused(t *testing.T) {
	svc, client := newOrchestratorTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	order, guest := seedRoomChargedOrder(t, client, tid)
	client.RoomGuest.UpdateOneID(guest.ID).SetStatus(entroomguest.StatusCheckedOut).ExecX(ctx)
	line := onlyLine(t, client, order.ID)

	_, err := svc.Edit(ctx, tid, addOneItem(order, line, payments.TenderRoomCharge))
	if !errors.Is(err, payments.ErrRoomStayClosed) {
		t.Fatalf("room charge after checkout = %v, want ErrRoomStayClosed", err)
	}
	if n := client.POSOrderLine.Query().Where(entposorderline.OrderID(order.ID)).CountX(ctx); n != 1 {
		t.Errorf("%d order lines after the refusal, want 1 (nothing applied)", n)
	}
	if n := client.RoomFolioItem.Query().Where(entroomfolioitem.RoomGuestID(guest.ID)).CountX(ctx); n != 1 {
		t.Errorf("%d folio lines after the refusal, want 1", n)
	}

	if _, err := client.Tender.Create().SetTenantID(tid).SetName("Cash").SetType("cash").SetIsActive(true).Save(ctx); err != nil {
		t.Fatalf("seed cash tender: %v", err)
	}
	if _, err := svc.Edit(ctx, tid, addOneItem(order, line, "cash")); err != nil {
		t.Fatalf("collect now after the refusal: %v", err)
	}
}
