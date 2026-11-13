package reversals

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entposreversal "github.com/bengobox/pos-service/internal/ent/posreversal"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/payments"
)

// pay is one completed payment on a seeded sale; minutesAgo orders them (stepPOSTotals nets the
// newest first).
type pay struct {
	method     string
	amount     float64
	minutesAgo int
}

// seedSaleWithLine is a completed sale of one line (qty x unit) paid by pays.
func seedSaleWithLine(t *testing.T, client *ent.Client, tid uuid.UUID, qty, unit float64, pays ...pay) (*ent.POSOrder, *ent.POSOrderLine) {
	t.Helper()
	ctx := context.Background()
	total := qty * unit
	order := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(total).SetTaxTotal(0).SetTotalAmount(total).SetPaidTotal(total).
		SaveX(ctx)
	line := client.POSOrderLine.Create().
		SetOrderID(order.ID).SetCatalogItemID(uuid.New()).SetSku("MEAL").SetName("Meal").
		SetQuantity(qty).SetUnitPrice(unit).SetTotalPrice(total).
		SaveX(ctx)
	for _, p := range pays {
		client.POSPayment.Create().
			SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(p.amount).SetCurrency("KES").SetStatus("completed").
			SetPaymentData(map[string]any{"method": p.method}).
			SetOccurredAt(time.Now().Add(-time.Duration(p.minutesAgo) * time.Minute)).
			SaveX(ctx)
	}
	return order, line
}

// seedRoomPart charges amount of the order to an open stay that also owes nights of room charge,
// the way the till's room charge writes the folio line.
func seedRoomPart(t *testing.T, client *ent.Client, order *ent.POSOrder, amount, nights float64) *ent.RoomGuest {
	t.Helper()
	ctx := context.Background()
	room := client.Room.Create().
		SetTenantID(order.TenantID).SetOutletID(uuid.New()).SetRoomNumber("R" + uuid.NewString()[:6]).SetName("Room").
		SetRatePerNight(1000).
		SaveX(ctx)
	guest := client.RoomGuest.Create().
		SetTenantID(order.TenantID).SetRoomID(room.ID).SetGuestName("Jane").SetPhone("+254712345678").SetIDNumber("ID-1").
		SetCheckInDate(time.Now()).SetNights(1).SetCheckOutDate(time.Now().AddDate(0, 0, 1)).
		SetTotalRoomCharge(nights).SetCheckedInBy(uuid.New()).
		SaveX(ctx)
	if nights > 0 {
		client.RoomFolioItem.Create().
			SetTenantID(order.TenantID).SetRoomID(room.ID).SetRoomGuestID(guest.ID).SetDescription("Room").
			SetAmount(nights).SetChargeType(roomfolioitem.ChargeTypeRoomCharge).SetCreatedBy(uuid.New()).
			SaveX(ctx)
	}
	client.RoomFolioItem.Create().
		SetTenantID(order.TenantID).SetRoomID(room.ID).SetRoomGuestID(guest.ID).SetDescription("POS bill").
		SetAmount(amount).SetChargeType(roomfolioitem.ChargeTypeRestaurant).SetCreatedBy(uuid.New()).SetPosOrderID(order.ID).
		SetMetadata(map[string]any{"source": "pos_room_charge"}).
		SaveX(ctx)
	return guest
}

func folioCredited(t *testing.T, client *ent.Client) float64 {
	t.Helper()
	sum := 0.0
	for _, c := range client.RoomFolioPayment.Query().Where(roomfoliopayment.Method(payments.FolioReturnCreditMethod)).AllX(context.Background()) {
		sum += c.Amount
	}
	return sum
}

type wantCall struct {
	channel, refType string
	amount           float64
}

// Re-review R-1: run the reversal steps in their real order (stepPOSTotals, which nets the
// payments for a partial reversal, then stepTreasuryGL) and check what reaches the folio and
// treasury. The folio credit is capped by the folio lines the till wrote, never by payment rows
// the same reversal just netted down.
func TestRoomChargedReversalAfterPOSTotals(t *testing.T) {
	for _, tc := range []struct {
		name         string
		qty, unit    float64
		pays         []pay
		roomPart     float64
		scope        string
		revQty, amt  float64
		wantCredited float64
		wantCalls    []wantCall
	}{
		{
			name: "full void of a 300 room-charged sale", qty: 3, unit: 100,
			pays: []pay{{"room_charge", 300, 0}}, roomPart: 300,
			scope: "full", revQty: 3, amt: 300,
			wantCredited: 300, wantCalls: []wantCall{{payments.RefundChannelRoomFolio, "pos_return", 300}},
		},
		{
			name: "edit removing 200 of a 300 room-charged sale", qty: 3, unit: 100,
			pays: []pay{{"room_charge", 300, 0}}, roomPart: 300,
			scope: "partial", revQty: 2, amt: 200,
			wantCredited: 200, wantCalls: []wantCall{{payments.RefundChannelRoomFolio, "pos_return", 200}},
		},
		{
			name: "full void of a mixed sale (cash 500, room 660)", qty: 2, unit: 580,
			pays: []pay{{"cash", 500, 10}, {"room_charge", 660, 0}}, roomPart: 660,
			scope: "full", revQty: 2, amt: 1160,
			wantCredited: 660, wantCalls: []wantCall{{payments.RefundChannelRoomFolio, "pos_return", 660}, {"cash", folioRestReferenceType, 500}},
		},
		{
			name: "all lines of a mixed sale edited away (cash 500, room 660)", qty: 2, unit: 580,
			pays: []pay{{"cash", 500, 10}, {"room_charge", 660, 0}}, roomPart: 660,
			scope: "partial", revQty: 2, amt: 1160,
			wantCredited: 660, wantCalls: []wantCall{{payments.RefundChannelRoomFolio, "pos_return", 660}, {"cash", folioRestReferenceType, 500}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, trc := newRecordingTreasuryServer(t)
			svc, client := newTestReversalsService(t, trc)
			ctx := context.Background()
			tid := uuid.New()
			order, line := seedSaleWithLine(t, client, tid, tc.qty, tc.unit, tc.pays...)
			seedRoomPart(t, client, order, tc.roomPart, 2000)

			channel := channelFor(t, svc, tid, order.ID, "")
			if channel != payments.RefundChannelRoomFolio {
				t.Fatalf("channel = %q, want room_folio", channel)
			}
			rev := client.POSReversal.Create().
				SetTenantID(tid).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
				SetReversalNumber("REV-" + uuid.NewString()[:6]).SetScope(entposreversal.Scope(tc.scope)).SetStatus("pending").SetReason("wrong item").
				SetRefundChannel(channel).
				SetLines([]entschema.ReversalLineJSON{{LineID: line.ID, SKU: line.Sku, Name: line.Name, Quantity: tc.revQty, OfQuantity: tc.qty, Amount: tc.amt}}).
				SetAmount(tc.amt).SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
				SaveX(ctx)

			if _, _, _, err := svc.stepPOSTotals(ctx, rev); err != nil {
				t.Fatalf("stepPOSTotals: %v", err)
			}
			if _, _, skip, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug"); err != nil || skip {
				t.Fatalf("stepTreasuryGL: err=%v skip=%v", err, skip)
			}

			if got := folioCredited(t, client); got != tc.wantCredited {
				t.Errorf("folio credited %v, want %v", got, tc.wantCredited)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.calls) != len(tc.wantCalls) {
				t.Fatalf("treasury calls = %+v, want %+v", rec.calls, tc.wantCalls)
			}
			for i, w := range tc.wantCalls {
				got := rec.calls[i]
				if got.RefundChannel != w.channel || got.ReferenceType != w.refType || got.Amount != w.amount {
					t.Errorf("call %d = %v %q %q, want %v %q %q", i, got.Amount, got.RefundChannel, got.ReferenceType, w.amount, w.channel, w.refType)
				}
			}
			if got := client.POSReversal.GetX(ctx, rev.ID).RefundChannel; got != payments.RefundChannelRoomFolio {
				t.Errorf("stored channel = %q, want room_folio", got)
			}
		})
	}
}
