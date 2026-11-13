package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
	"github.com/bengobox/pos-service/internal/modules/payments"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

func folioLine(amount float64, chargeType entroomfolioitem.ChargeType, tillRoomCharge bool) *ent.RoomFolioItem {
	it := &ent.RoomFolioItem{Amount: amount, ChargeType: chargeType, Metadata: map[string]any{}}
	if tillRoomCharge {
		oid := uuid.New()
		it.PosOrderID = &oid
		it.Metadata["source"] = "pos_room_charge"
	}
	return it
}

func folioPay(amount float64, method string, posCharge float64) *ent.RoomFolioPayment {
	p := &ent.RoomFolioPayment{Amount: amount, Method: method, Status: "completed", Metadata: map[string]any{}}
	if posCharge > 0 {
		p.Metadata[folioPOSChargeKey] = posCharge
	}
	return p
}

// The folio payment's POS share is what the folio still owes on till room charges, in
// proportion to the balance; the rest splits room vs other the way the room split always did.
// Worked example: room 1000, a till room charge of 500, minibar 100 (folio 1600).
func TestFolioPaymentSplit(t *testing.T) {
	items := []*ent.RoomFolioItem{
		folioLine(1000, entroomfolioitem.ChargeTypeRoomCharge, false),
		folioLine(500, entroomfolioitem.ChargeTypeRestaurant, true),
		folioLine(100, entroomfolioitem.ChargeTypeMinibar, false),
	}
	for _, tc := range []struct {
		name            string
		pays            []*ent.RoomFolioPayment
		amount          float64
		wantPOS, wantRm float64
	}{
		// The whole balance clears the whole POS receivable, and room revenue is the room nights.
		{"one payment of the whole folio", nil, 1600, 500, 1000},
		// A part payment goes to the room nights and minibar first: it clears no POS value
		// (review I-2), and the room share is its room part of the non-POS lines.
		{"first half", nil, 800, 0, 727.27},
		// The second half reaches the POS lines and clears all of them.
		{"second half", []*ent.RoomFolioPayment{folioPay(800, "cash", 0)}, 800, 500, 272.73},
		// A return credited to the folio already reduced the receivable by 200 (treasury booked it
		// there), so the guest's payment clears only the 300 left.
		{"after a 200 folio return credit", []*ent.RoomFolioPayment{folioPay(200, payments.FolioReturnCreditMethod, 0)}, 1400, 300, 1000},
		// Nothing left on the POS sales: the payment is all room/other.
		{"pos already cleared", []*ent.RoomFolioPayment{folioPay(500, "cash", 500)}, 1100, 0, 1000},
		// A pending payment does not count as cleared.
		{"pending ignored", []*ent.RoomFolioPayment{{Amount: 800, Method: "mpesa", Status: "pending", Metadata: map[string]any{folioPOSChargeKey: 250.0}}}, 1600, 500, 1000},
	} {
		pos, room := folioPaymentSplit(items, tc.pays, tc.amount)
		if pos != tc.wantPOS || room != tc.wantRm {
			t.Errorf("%s: pos %v room %v, want %v %v", tc.name, pos, room, tc.wantPOS, tc.wantRm)
		}
	}

	// Review E2: room 1,000 and a till room charge of 116. A mid-stay payment of 558 clears no POS
	// value; the bill is then returned and credited to the folio (116, which treasury books against
	// the receivable); checkout of the last 442 is all room. Room revenue ends at 1,000 and the
	// receivable at zero.
	e2 := []*ent.RoomFolioItem{
		folioLine(1000, entroomfolioitem.ChargeTypeRoomCharge, false),
		folioLine(116, entroomfolioitem.ChargeTypeRestaurant, true),
	}
	if pos, room := folioPaymentSplit(e2, nil, 558); pos != 0 || room != 558 {
		t.Errorf("E2 mid-stay payment: pos %v room %v, want 0 558", pos, room)
	}
	afterReturn := []*ent.RoomFolioPayment{folioPay(558, "cash", 0), folioPay(116, payments.FolioReturnCreditMethod, 0)}
	if pos, room := folioPaymentSplit(e2, afterReturn, 442); pos != 0 || room != 442 {
		t.Errorf("E2 checkout: pos %v room %v, want 0 442", pos, room)
	}
	// Without the return, the checkout of 558 clears the whole 116 on the receivable.
	if pos, room := folioPaymentSplit(e2, []*ent.RoomFolioPayment{folioPay(558, "cash", 0)}, 558); pos != 116 || room != 442 {
		t.Errorf("E2 checkout without the return: pos %v room %v, want 116 442", pos, room)
	}

	// A line posted by hand with an order id was never booked to the receivable: it stays revenue.
	oid := uuid.New()
	hand := &ent.RoomFolioItem{Amount: 500, ChargeType: entroomfolioitem.ChargeTypeRestaurant, PosOrderID: &oid, Metadata: map[string]any{}}
	if pos, room := folioPaymentSplit([]*ent.RoomFolioItem{items[0], hand}, nil, 1500); pos != 0 || room != 1000 {
		t.Errorf("hand-posted order line: pos %v room %v, want 0 1000", pos, room)
	}
	// No folio lines from the till at all: the split is the old room split exactly.
	if pos, room := folioPaymentSplit([]*ent.RoomFolioItem{items[0], items[2]}, nil, 1100); pos != 0 || room != 1000 {
		t.Errorf("no till room charge: pos %v room %v, want 0 1000", pos, room)
	}
}

// SettleFolio sends the POS share to treasury as pos_charge_amount (so the ledger credits the
// guest receivable for it, not revenue) and keeps it on the folio payment for the next split.
func TestSettleFolioSendsThePOSShare(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	ctx := context.Background()

	var mu sync.Mutex
	var sent []treasury.CreateIntentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req treasury.CreateIntentRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		sent = append(sent, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.IntentResponse{IntentID: uuid.NewString(), Status: "succeeded"})
	}))
	t.Cleanup(srv.Close)
	h.SetTreasuryClient(treasury.NewClient(srv.URL, "test-key", 5*time.Second))

	room := seedOccupiedRoom(t, client, tid) // room charge 1000 on the folio
	guest := client.RoomGuest.Query().Where(entroomguest.RoomID(room.ID)).OnlyX(ctx)
	orderID := uuid.New()
	client.RoomFolioItem.Create().
		SetTenantID(tid).SetRoomID(room.ID).SetRoomGuestID(guest.ID).
		SetChargeType(entroomfolioitem.ChargeTypeRestaurant).SetDescription("POS bill").SetAmount(500).SetCreatedBy(uuid.New()).
		SetPosOrderID(orderID).SetMetadata(map[string]any{"source": "pos_room_charge"}).
		SaveX(ctx)

	rec := settle(t, h, tid, room.ID, map[string]any{"amount": 1500, "method": "cash"})
	if rec.Code != http.StatusOK {
		t.Fatalf("settle status = %d, body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("%d treasury intents, want 1", len(sent))
	}
	md := sent[0].Metadata
	if md["pos_charge_amount"] != "500.00" || md["room_revenue_amount"] != "1000.00" || sent[0].ReferenceType != "hotel_folio" {
		t.Errorf("intent %q metadata = %v, want pos_charge_amount 500.00 and room_revenue_amount 1000.00", sent[0].ReferenceType, md)
	}
	p := client.RoomFolioPayment.Query().Where(roomfoliopayment.RoomGuestID(guest.ID)).OnlyX(ctx)
	if p.Metadata[folioPOSChargeKey] != 500.0 {
		t.Errorf("folio payment metadata = %v, want pos_charge_amount 500", p.Metadata)
	}
}
