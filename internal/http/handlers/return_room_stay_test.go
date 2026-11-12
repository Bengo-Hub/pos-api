package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
)

// The return detail tells pos-ui when the sale was charged to a room stay that is still open,
// so the return screen can say the refund is credited to the guest's room bill.
func TestReturnDetailRoomStayOpen(t *testing.T) {
	client := openSQLiteTestClient(t, "return_room_stay")
	h := NewReturnHandler(zap.NewNop(), client, nil)
	ctx := context.Background()
	tid, outlet := uuid.New(), uuid.New()

	seed := func(stay *roomguest.Status) *ent.POSReturn {
		order := client.POSOrder.Create().
			SetTenantID(tid).SetOutletID(outlet).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
			SetOrderNumber("ORD-" + uuid.NewString()[:6]).SetStatus("completed").
			SetSubtotal(100).SetTaxTotal(0).SetTotalAmount(100).SetPaidTotal(100).
			SaveX(ctx)
		if stay != nil {
			// The stay and its folio line both hold a foreign key to a real room.
			room := client.Room.Create().
				SetTenantID(tid).SetOutletID(outlet).SetRoomNumber("R" + uuid.NewString()[:6]).SetName("Room").
				SetRatePerNight(1000).
				SaveX(ctx)
			guest := client.RoomGuest.Create().
				SetTenantID(tid).SetRoomID(room.ID).SetGuestName("Jane").SetPhone("+254712345678").SetIDNumber("ID-1").
				SetCheckInDate(time.Now()).SetNights(1).SetCheckOutDate(time.Now().AddDate(0, 0, 1)).
				SetTotalRoomCharge(1000).SetCheckedInBy(uuid.New()).SetStatus(*stay).
				SaveX(ctx)
			client.RoomFolioItem.Create().
				SetTenantID(tid).SetRoomID(guest.RoomID).SetRoomGuestID(guest.ID).SetDescription("Restaurant bill").
				SetAmount(100).SetChargeType(roomfolioitem.ChargeTypeOther).SetCreatedBy(uuid.New()).SetPosOrderID(order.ID).
				SaveX(ctx)
		}
		return client.POSReturn.Create().
			SetTenantID(tid).SetOutletID(outlet).SetOrderID(order.ID).SetReturnNumber("RET-" + uuid.NewString()[:6]).
			SetReturnType(posreturn.ReturnTypeRefund).SetStatus(posreturn.StatusPending).SetReason("x").
			SetRefundAmount(100).SetRequestedBy(uuid.New()).
			SaveX(ctx)
	}
	open, closed := roomguest.StatusActive, roomguest.StatusCheckedOut

	if !h.withOrderNumber(ctx, tid, seed(&open)).RoomStayOpen {
		t.Error("a sale charged to an open stay: room_stay_open = false, want true")
	}
	if h.withOrderNumber(ctx, tid, seed(&closed)).RoomStayOpen {
		t.Error("a closed stay: room_stay_open = true, want false")
	}
	if h.withOrderNumber(ctx, tid, seed(nil)).RoomStayOpen {
		t.Error("a sale never charged to a room: room_stay_open = true, want false")
	}
}
