package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
)

// seedOccupiedRoom is a room with an active guest owing 1000.
func seedOccupiedRoom(t *testing.T, client *ent.Client, tid uuid.UUID) *ent.Room {
	t.Helper()
	ctx := context.Background()
	room := client.Room.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetRoomNumber("D" + uuid.NewString()[:4]).SetName("Desk").
		SetRatePerNight(1000).SetStatus(entroom.StatusOccupied).
		SaveX(ctx)
	guest := client.RoomGuest.Create().
		SetTenantID(tid).SetRoomID(room.ID).SetGuestName("Jane Wanjiku").SetPhone("+254712345678").
		SetIDNumber("ID-1").SetCheckInDate(time.Now()).SetNights(1).SetCheckOutDate(time.Now().AddDate(0, 0, 1)).
		SetTotalRoomCharge(1000).SetCheckedInBy(uuid.New()).
		SaveX(ctx)
	client.RoomFolioItem.Create().
		SetTenantID(tid).SetRoomID(room.ID).SetRoomGuestID(guest.ID).
		SetChargeType(entroomfolioitem.ChargeTypeRoomCharge).SetDescription("Room").SetAmount(1000).SetCreatedBy(uuid.New()).
		SaveX(ctx)
	return room
}

func settle(t *testing.T, h *HotelHandler, tid, roomID uuid.UUID, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.SettleFolio(rec, hotelRoomRequest(t, http.MethodPost, tid, roomID, body))
	return rec
}

// The desk takes MTN and Airtel codes in the till's format, uppercased, settled at once.
func TestSettleFolioTakesMobileMoneyCodes(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	ctx := context.Background()

	room := seedOccupiedRoom(t, client, tid)
	rec := settle(t, h, tid, room.ID, map[string]any{"amount": 400, "method": "mtn_momo_manual", "reference": " mtn1234567 "})
	if rec.Code != http.StatusOK {
		t.Fatalf("MTN settle status = %d, body %s", rec.Code, rec.Body.String())
	}
	p := client.RoomFolioPayment.Query().Where(roomfoliopayment.RoomID(room.ID)).OnlyX(ctx)
	if p.Method != "mtn_momo_manual" || p.Reference != "MTN1234567" || p.Status != "completed" {
		t.Errorf("stored payment = method %q ref %q status %q", p.Method, p.Reference, p.Status)
	}

	rec = settle(t, h, tid, room.ID, map[string]any{"amount": 100, "method": "airtel_money_manual", "reference": "AB12"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Airtel Money transaction ID") {
		t.Errorf("short Airtel code: status %d body %s, want 400 with the till's message", rec.Code, rec.Body.String())
	}

	rec = settle(t, h, tid, room.ID, map[string]any{"amount": 100, "method": "mpesa", "reference": "  "})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("M-Pesa without a code: status %d, want 400", rec.Code)
	}
}

// A code that already settled a till sale or another folio is refused, per network.
func TestSettleFolioRefusesAReusedCode(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	ctx := context.Background()

	order := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-1").SetStatus("completed").SetSubtotal(100).SetTaxTotal(0).SetTotalAmount(100).SetPaidTotal(100).
		SaveX(ctx)
	client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(100).SetCurrency("KES").SetStatus("completed").
		SetExternalReference("QB234ABCDE").SetPaymentData(map[string]any{"method": "mpesa_manual"}).
		SaveX(ctx)

	room := seedOccupiedRoom(t, client, tid)
	rec := settle(t, h, tid, room.ID, map[string]any{"amount": 100, "method": "mpesa", "reference": "qb234abcde"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already been used") {
		t.Errorf("till M-Pesa code at the desk: status %d body %s, want 409", rec.Code, rec.Body.String())
	}

	if rec := settle(t, h, tid, room.ID, map[string]any{"amount": 100, "method": "airtel_money_manual", "reference": "AIR1234567"}); rec.Code != http.StatusOK {
		t.Fatalf("first Airtel code: status %d body %s", rec.Code, rec.Body.String())
	}
	other := seedOccupiedRoom(t, client, tid)
	if rec := settle(t, h, tid, other.ID, map[string]any{"amount": 100, "method": "airtel_money_manual", "reference": "air1234567"}); rec.Code != http.StatusConflict {
		t.Errorf("Airtel code reused on another folio: status %d, want 409", rec.Code)
	}
	if rec := settle(t, h, tid, other.ID, map[string]any{"amount": 100, "method": "mtn_momo_manual", "reference": "AIR1234567"}); rec.Code != http.StatusOK {
		t.Errorf("same digits on MTN: status %d body %s, want 200 (checked per network)", rec.Code, rec.Body.String())
	}
	if n := client.RoomFolioPayment.Query().CountX(ctx); n != 2 {
		t.Errorf("%d folio payments stored, want 2 (refused codes store nothing)", n)
	}
}

// Other needs the label saying how the guest paid; the label is kept with the payment.
func TestSettleFolioOtherNeedsALabel(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	room := seedOccupiedRoom(t, client, tid)

	if rec := settle(t, h, tid, room.ID, map[string]any{"amount": 100, "method": "other_manual"}); rec.Code != http.StatusBadRequest {
		t.Errorf("Other without a label: status %d, want 400", rec.Code)
	}
	rec := settle(t, h, tid, room.ID, map[string]any{"amount": 100, "method": "other_manual", "label": " Company voucher ", "reference": "V-77"})
	if rec.Code != http.StatusOK {
		t.Fatalf("Other with a label: status %d body %s", rec.Code, rec.Body.String())
	}
	p := client.RoomFolioPayment.Query().OnlyX(context.Background())
	if p.Method != "other_manual" || p.Metadata["payment_label"] != "Company voucher" || p.Reference != "V-77" {
		t.Errorf("stored Other payment = method %q label %v ref %q", p.Method, p.Metadata["payment_label"], p.Reference)
	}
}

// Check-in under pay upfront refuses a bad code before the guest is checked in.
func TestCheckInDeskTenderCheckedBeforeCheckIn(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	if _, status, err := h.checkDeskTender(context.Background(), tid, "mtn_momo_manual", "AB1", ""); err == nil || status != http.StatusBadRequest {
		t.Errorf("bad MTN code: status %d err %v, want 400", status, err)
	}
	got, status, err := h.checkDeskTender(context.Background(), tid, "cash", " ", "label")
	if err != nil || status != 0 || got.Method != "cash" || got.Label != "" {
		t.Errorf("cash: got %+v status %d err %v, want it passed through without a label", got, status, err)
	}
	_ = client
}

// seedPayUpfrontRoom is an available room whose outlet takes payment at check-in.
func seedPayUpfrontRoom(t *testing.T, client *ent.Client, tid uuid.UUID) *ent.Room {
	t.Helper()
	ctx := context.Background()
	// The outlet setting holds a foreign key to a real outlet, which needs its tenant.
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "U"+uuid.NewString()[:4]).ID
	client.OutletSetting.Create().SetOutletID(outlet).
		SetMetadata(map[string]any{"booking_policy": map[string]any{"payment_timing": "pay_upfront"}}).
		SaveX(ctx)
	return client.Room.Create().
		SetTenantID(tid).SetOutletID(outlet).SetRoomNumber("U" + uuid.NewString()[:4]).SetName("Upfront").
		SetRatePerNight(800).
		SaveX(ctx)
}

func payUpfrontBody(method, ref string) map[string]any {
	return map[string]any{
		"guest_name": "Jane Wanjiku", "phone": "0712345678", "id_number": "AK1234567", "nights": 1,
		"payment_method": method, "payment_reference": ref,
	}
}

// A refused code at a pay upfront check-in leaves nothing behind: no guest, no folio charge,
// no folio payment, and the room stays available.
func TestCheckInPayUpfrontRefusedCodeWritesNothing(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	ctx := context.Background()

	// The code already settled a folio payment in this tenant.
	client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(uuid.New()).SetRoomGuestID(uuid.New()).
		SetAmount(100).SetMethod("mtn_momo_manual").SetReference("MTN1234567").SetStatus("completed").
		SaveX(ctx)
	room := seedPayUpfrontRoom(t, client, tid)

	for _, tc := range []struct {
		name, method, ref string
		want              int
	}{
		{"reused MTN code", "mtn_momo_manual", "mtn1234567", http.StatusConflict},
		{"bad Airtel code", "airtel_money_manual", "AB1", http.StatusBadRequest},
		{"M-Pesa without a code", "mpesa", "", http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		h.CheckIn(rec, hotelRoomRequest(t, http.MethodPost, tid, room.ID, payUpfrontBody(tc.method, tc.ref)))
		if rec.Code != tc.want {
			t.Errorf("%s: status %d body %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	if n := client.RoomGuest.Query().CountX(ctx); n != 0 {
		t.Errorf("%d guests, want 0", n)
	}
	if n := client.RoomFolioItem.Query().CountX(ctx); n != 0 {
		t.Errorf("%d folio items, want 0", n)
	}
	if n := client.RoomFolioPayment.Query().CountX(ctx); n != 1 {
		t.Errorf("%d folio payments, want only the seeded one", n)
	}
	if got := client.Room.GetX(ctx, room.ID).Status; got != entroom.StatusAvailable {
		t.Errorf("room status = %q, want available", got)
	}
}

// A good code at a pay upfront check-in records the payment with the code uppercased.
func TestCheckInPayUpfrontTakesAnAirtelCode(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	room := seedPayUpfrontRoom(t, client, tid)

	rec := httptest.NewRecorder()
	h.CheckIn(rec, hotelRoomRequest(t, http.MethodPost, tid, room.ID, payUpfrontBody("airtel_money_manual", "ci251111.1530.a12345")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	p := client.RoomFolioPayment.Query().OnlyX(context.Background())
	if p.Method != "airtel_money_manual" || p.Reference != "CI251111.1530.A12345" || p.Status != "completed" {
		t.Errorf("payment = method %q ref %q status %q", p.Method, p.Reference, p.Status)
	}
}

// The duplicate check runs again right before the write, so a code taken elsewhere after the
// first check is still refused.
func TestRecordFolioPaymentRechecksTheCode(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid := uuid.New()
	ctx := context.Background()
	room := seedOccupiedRoom(t, client, tid)

	tender, code, err := h.checkDeskTender(ctx, tid, "mtn_momo_manual", "MTN7654321", "")
	if err != nil || code != 0 {
		t.Fatalf("first check: code %d err %v", code, err)
	}
	// Another desk takes the same code in between.
	client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(uuid.New()).SetRoomGuestID(uuid.New()).
		SetAmount(100).SetMethod("mtn_momo_manual").SetReference("MTN7654321").SetStatus("completed").
		SaveX(ctx)

	req := hotelRoomRequest(t, http.MethodPost, tid, room.ID, nil)
	_, _, _, perr := h.recordFolioPayment(req, tid, "acme", room, uuid.New(), "Jane", 100, tender, 0, uuid.Nil)
	if !errors.Is(perr, errDeskCodeUsed) {
		t.Fatalf("recordFolioPayment error = %v, want the code-used error", perr)
	}
	if n := client.RoomFolioPayment.Query().CountX(ctx); n != 1 {
		t.Errorf("%d folio payments, want only the other desk's", n)
	}
}

// A pay upfront payment refused at write time (the code was taken after the first check) tells
// the desk exactly that; any other failure keeps the general message.
func TestUpfrontPaymentFailureMessage(t *testing.T) {
	used := upfrontPaymentFailureMessage(deskCodeUsedError("mtn_momo_manual", "MTN1"))
	if used != "Checked in, but the code was already used; take payment again." {
		t.Errorf("code-used message = %q", used)
	}
	other := upfrontPaymentFailureMessage(errors.New("db down"))
	if other == used || other == "" {
		t.Errorf("other failure message = %q, want the general message", other)
	}
}
