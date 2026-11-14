package handlers

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

	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

func checkOutRoom(t *testing.T, h *HotelHandler, tid, roomID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.CheckOut(rec, hotelRoomRequest(t, http.MethodPost, tid, roomID, map[string]any{"checked_out_by": uuid.New().String()}))
	return rec
}

// Review R-5 (owner decision 2026-11-14): no checkout while the folio owes anything. CheckOut
// answers 409 with the amount owed and leaves the guest checked in; once the folio is paid it
// checks the guest out.
func TestCheckOutRefusedWhileTheFolioOwes(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	ctx := context.Background()
	room := seedOccupiedRoom(t, client, tid) // room charge 1000 on the folio

	rec := checkOutRoom(t, h, tid, room.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("checkout with 1000 owed: status %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "outstanding_balance" || body["balance"] != 1000.0 ||
		!strings.Contains(body["message"].(string), "still owes KES 1000.00") {
		t.Errorf("409 body = %v, want outstanding_balance, balance 1000 and the owed message", body)
	}
	guest := client.RoomGuest.Query().Where(entroomguest.RoomID(room.ID)).OnlyX(ctx)
	if guest.Status != entroomguest.StatusActive {
		t.Fatalf("guest status %s after a refused checkout, want active", guest.Status)
	}

	if rec := settle(t, h, tid, room.ID, map[string]any{"amount": 1000, "method": "cash"}); rec.Code != http.StatusOK {
		t.Fatalf("settle: status %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := checkOutRoom(t, h, tid, room.ID); rec.Code != http.StatusOK {
		t.Fatalf("checkout after paying: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if g := client.RoomGuest.GetX(ctx, guest.ID); g.Status != entroomguest.StatusCheckedOut {
		t.Errorf("guest status %s, want checked_out", g.Status)
	}
}

// The re-check under the stay lock is what closes the race: a charge that lands after the
// summary was read is still counted (checkOutStay reads the balance again in its transaction).
func TestCheckOutStayReadsTheBalanceUnderTheLock(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	ctx := context.Background()
	room := seedOccupiedRoom(t, client, tid)
	guest := client.RoomGuest.Query().Where(entroomguest.RoomID(room.ID)).OnlyX(ctx)
	client.RoomFolioPayment.Create().SetTenantID(tid).SetRoomID(room.ID).SetRoomGuestID(guest.ID).
		SetAmount(1000).SetMethod("cash").SetStatus("completed").SaveX(ctx)
	// A till room charge committed after the desk loaded the bill.
	client.RoomFolioItem.Create().SetTenantID(tid).SetRoomID(room.ID).SetRoomGuestID(guest.ID).
		SetChargeType(entroomfolioitem.ChargeTypeRestaurant).SetDescription("POS bill").SetAmount(250).
		SetCreatedBy(uuid.New()).SetPosOrderID(uuid.New()).SetMetadata(map[string]any{"source": "pos_room_charge"}).
		SaveX(ctx)

	owed, done, err := h.checkOutStay(ctx, tid, guest.ID, uuid.New(), time.Now())
	if err != nil || done || owed != 250 {
		t.Fatalf("checkOutStay = owed %v done %v err %v, want owed 250 and no checkout", owed, done, err)
	}
	client.RoomFolioPayment.Create().SetTenantID(tid).SetRoomID(room.ID).SetRoomGuestID(guest.ID).
		SetAmount(250).SetMethod("cash").SetStatus("completed").SaveX(ctx)
	if owed, done, err := h.checkOutStay(ctx, tid, guest.ID, uuid.New(), time.Now()); err != nil || !done || owed != 0 {
		t.Fatalf("checkOutStay after paying = owed %v done %v err %v, want checked out", owed, done, err)
	}
	// A second checkout finds no active stay: done false, nothing owed, no error.
	if owed, done, err := h.checkOutStay(ctx, tid, guest.ID, uuid.New(), time.Now()); err != nil || done || owed != 0 {
		t.Errorf("second checkOutStay = owed %v done %v err %v, want a no-op", owed, done, err)
	}
}

// Settle and check out still works: the payment is written first, then the checkout runs in the
// same transaction and sees it. A part payment with checkout set keeps the guest checked in.
func TestSettleAndCheckOutUnderTheSameLock(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	ctx := context.Background()
	room := seedOccupiedRoom(t, client, tid)
	guest := client.RoomGuest.Query().Where(entroomguest.RoomID(room.ID)).OnlyX(ctx)

	rec := settle(t, h, tid, room.ID, map[string]any{"amount": 400, "method": "cash", "checkout": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("part settle: status %d (%s)", rec.Code, rec.Body.String())
	}
	var part struct {
		CheckedOut bool                  `json:"checked_out"`
		Summary    *folioSummaryResponse `json:"summary"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &part)
	if part.CheckedOut || part.Summary == nil || part.Summary.Balance != 600 {
		t.Fatalf("part settle with checkout = checked_out %v summary %+v, want still in with 600 owed", part.CheckedOut, part.Summary)
	}

	rec = settle(t, h, tid, room.ID, map[string]any{"amount": 600, "method": "cash", "checkout": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("final settle: status %d (%s)", rec.Code, rec.Body.String())
	}
	var full struct {
		CheckedOut bool                  `json:"checked_out"`
		Summary    *folioSummaryResponse `json:"summary"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &full)
	if !full.CheckedOut || full.Summary == nil || full.Summary.Balance != 0 || full.Summary.ChargesTotal != 1000 {
		t.Fatalf("settle and check out = checked_out %v summary %+v, want checked out with the bill in the response", full.CheckedOut, full.Summary)
	}
	if g := client.RoomGuest.GetX(ctx, guest.ID); g.Status != entroomguest.StatusCheckedOut {
		t.Errorf("guest status %s, want checked_out", g.Status)
	}
}

// Review M-1: an online payment for the whole folio is still pending when the desk takes cash for
// the whole folio. The pending payment reserved the POS share, so the cash payment claims none of
// it and treasury never credits the guest receivable twice.
func TestSettleFolioCountsPendingPaymentsPOSShareAsReserved(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(treasury.IntentResponse{IntentID: uuid.NewString(), Status: "pending"})
	}))
	t.Cleanup(srv.Close)
	h.SetTreasuryClient(treasury.NewClient(srv.URL, "test-key", 5*time.Second))

	room := seedOccupiedRoom(t, client, tid) // room 1000
	guest := client.RoomGuest.Query().Where(entroomguest.RoomID(room.ID)).OnlyX(ctx)
	client.RoomFolioItem.Create().
		SetTenantID(tid).SetRoomID(room.ID).SetRoomGuestID(guest.ID).
		SetChargeType(entroomfolioitem.ChargeTypeRestaurant).SetDescription("POS bill").SetAmount(500).SetCreatedBy(uuid.New()).
		SetPosOrderID(uuid.New()).SetMetadata(map[string]any{"source": "pos_room_charge"}).
		SaveX(ctx)

	// The STK push for the whole 1500 claims the POS share and stays pending.
	if rec := settle(t, h, tid, room.ID, map[string]any{"amount": 1500, "method": "mpesa_stk"}); rec.Code != http.StatusOK {
		t.Fatalf("stk settle: status %d (%s)", rec.Code, rec.Body.String())
	}
	// Cash for the whole 1500 while it is pending.
	if rec := settle(t, h, tid, room.ID, map[string]any{"amount": 1500, "method": "cash"}); rec.Code != http.StatusOK {
		t.Fatalf("cash settle: status %d (%s)", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("%d treasury intents, want 2", len(sent))
	}
	if sent[0].Metadata["pos_charge_amount"] != "500.00" {
		t.Errorf("stk intent pos_charge_amount = %v, want 500.00", sent[0].Metadata["pos_charge_amount"])
	}
	if v, ok := sent[1].Metadata["pos_charge_amount"]; ok {
		t.Errorf("cash intent pos_charge_amount = %v, want none (reserved by the pending payment)", v)
	}
	pays := client.RoomFolioPayment.Query().Where(roomfoliopayment.RoomGuestID(guest.ID)).AllX(ctx)
	claimed := 0.0
	for _, p := range pays {
		if v, ok := p.Metadata[folioPOSChargeKey].(float64); ok {
			claimed += v
		}
	}
	if claimed != 500 {
		t.Errorf("POS share claimed across folio payments = %v, want 500 once", claimed)
	}
}
