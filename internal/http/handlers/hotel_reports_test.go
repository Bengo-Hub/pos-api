package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// TestHotelOccupancyReport_ActiveGuestJustCheckedIn_NightsAreWholeNotFractional guards the
// reported bug: a guest who had just checked in (or a same-day test booking) showed 0.0%
// occupancy alongside an ADR in the millions, because occupied room-nights was computed from
// raw wall-clock hours since check-in (a few minutes / 24h) instead of whole calendar nights,
// even though a full night's charge had already posted to the folio.
func TestHotelOccupancyReport_ActiveGuestJustCheckedIn_NightsAreWholeNotFractional(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()

	room, err := client.Room.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetRoomNumber("G1").
		SetName("G1").
		SetRatePerNight(800).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room: %v", err)
	}

	// Report window ends 2026-01-10 15:30 UTC. The guest checked in five minutes before that,
	// paid upfront for 3 nights (2400 = 3 x 800), and has not checked out yet.
	windowFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	windowTo := time.Date(2026, 1, 10, 15, 30, 0, 0, time.UTC)
	checkIn := windowTo.Add(-5 * time.Minute)
	scheduledCheckout := checkIn.AddDate(0, 0, 3)

	guest, err := client.RoomGuest.Create().
		SetTenantID(tid).
		SetRoomID(room.ID).
		SetGuestName("Test Guest").
		SetPhone("0700000000").
		SetIDNumber("ID-0001").
		SetCheckInDate(checkIn).
		SetNights(3).
		SetCheckOutDate(scheduledCheckout).
		SetTotalRoomCharge(2400).
		SetCheckedInBy(uuid.New()).
		SetCheckedInAt(checkIn).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room guest: %v", err)
	}

	if _, err := client.RoomFolioItem.Create().
		SetTenantID(tid).
		SetRoomID(room.ID).
		SetRoomGuestID(guest.ID).
		SetDescription("Room charge - 3 nights").
		SetAmount(2400).
		SetChargeType("room_charge").
		SetCreatedBy(uuid.New()).
		SetCreatedAt(checkIn).
		Save(context.Background()); err != nil {
		t.Fatalf("seed folio item: %v", err)
	}

	req := reportsRequest(t, tid, &outletID, "from="+windowFrom.Format(time.RFC3339)+"&to="+windowTo.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	h.HotelOccupancyReport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HotelOccupancyReport: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var result hotelOccupancyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v, body: %s", err, rec.Body.String())
	}

	if result.OccupiedRoomNights != 1 {
		t.Fatalf("expected exactly 1 occupied room-night (tonight, sold in full), got %v", result.OccupiedRoomNights)
	}
	if result.ADR != 2400 {
		t.Fatalf("expected ADR = room_revenue / 1 night = 2400, got %v (the reported bug produced ADR in the millions here)", result.ADR)
	}
	if result.RoomRevenue != 2400 {
		t.Fatalf("expected room_revenue = 2400, got %v", result.RoomRevenue)
	}
}

// TestHotelOccupancyReport_CheckedOutSameCalendarDay_NoOccupiedNightsNoInflatedADR covers a
// same-day check-in/checkout (a quick manual test cycle, or a genuine day-use booking): zero
// calendar nights were actually stayed, so ADR must render as 0 rather than dividing real
// revenue by a near-zero fractional night count and exploding into an unusable number.
func TestHotelOccupancyReport_CheckedOutSameCalendarDay_NoOccupiedNightsNoInflatedADR(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()

	room, err := client.Room.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetRoomNumber("G2").
		SetName("G2").
		SetRatePerNight(800).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room: %v", err)
	}

	windowFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	windowTo := time.Date(2026, 1, 10, 15, 30, 0, 0, time.UTC)
	checkIn := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	checkedOutAt := checkIn.Add(20 * time.Minute)

	guest, err := client.RoomGuest.Create().
		SetTenantID(tid).
		SetRoomID(room.ID).
		SetGuestName("Same Day Guest").
		SetPhone("0700000001").
		SetIDNumber("ID-0002").
		SetCheckInDate(checkIn).
		SetNights(3).
		SetCheckOutDate(checkIn.AddDate(0, 0, 3)).
		SetTotalRoomCharge(2400).
		SetCheckedInBy(uuid.New()).
		SetCheckedInAt(checkIn).
		SetStatus(entroomguest.StatusCheckedOut).
		SetCheckedOutAt(checkedOutAt).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room guest: %v", err)
	}

	if _, err := client.RoomFolioItem.Create().
		SetTenantID(tid).
		SetRoomID(room.ID).
		SetRoomGuestID(guest.ID).
		SetDescription("Room charge - 3 nights").
		SetAmount(2400).
		SetChargeType("room_charge").
		SetCreatedBy(uuid.New()).
		SetCreatedAt(checkIn).
		Save(context.Background()); err != nil {
		t.Fatalf("seed folio item: %v", err)
	}

	req := reportsRequest(t, tid, &outletID, "from="+windowFrom.Format(time.RFC3339)+"&to="+windowTo.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	h.HotelOccupancyReport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HotelOccupancyReport: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var result hotelOccupancyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v, body: %s", err, rec.Body.String())
	}

	if result.OccupiedRoomNights != 0 {
		t.Fatalf("expected 0 occupied room-nights for a same-day check-in/checkout, got %v", result.OccupiedRoomNights)
	}
	if result.ADR != 0 {
		t.Fatalf("expected ADR = 0 (undefined, guarded) rather than an inflated number, got %v", result.ADR)
	}
	if result.RoomRevenue != 2400 {
		t.Fatalf("posted revenue must still be reported in full regardless of nights, got %v", result.RoomRevenue)
	}
}
