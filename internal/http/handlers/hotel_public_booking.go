package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
	entroombooking "github.com/bengobox/pos-service/internal/ent/roombooking"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// This file implements the guest-facing self-service room-booking surface: an unauthenticated
// availability check plus a booking submission, mirrored on tables.go's GetAvailableSlots/
// CreateReservation (the existing table-reservation widget) but adapted from a single time
// slot to a date-RANGE stay. Both endpoints sit in the router's public `pub` group alongside
// /pos/reservations — see router.go. A submitted booking always lands as status=pending; a
// staff member confirms or rejects it from the Bookings page (UpdateRoomBooking, unchanged —
// no separate confirm endpoint was needed since that already accepts any status transition).

// roomsOccupiedInWindow returns the set of room IDs (from the given candidates) with a guest
// stay overlapping [arrival, departure) — the shared definition of "occupied" for date-range
// availability, reused by both the aggregate availability check and the single-type
// server-side re-validation at booking time. Mirrors the overlap window HotelOccupancyReport
// already uses for occupied room-nights.
func (h *HotelHandler) roomsOccupiedInWindow(ctx context.Context, roomIDs []uuid.UUID, arrival, departure time.Time) (map[uuid.UUID]bool, error) {
	occupied := map[uuid.UUID]bool{}
	if len(roomIDs) == 0 {
		return occupied, nil
	}
	guests, err := h.client.RoomGuest.Query().
		Where(entroomguest.RoomIDIn(roomIDs...), entroomguest.CheckInDateLT(departure)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range guests {
		stayEnd := g.CheckOutDate
		if g.Status == entroomguest.StatusCheckedOut && g.CheckedOutAt != nil {
			stayEnd = *g.CheckedOutAt
		}
		if stayEnd.After(arrival) {
			occupied[g.RoomID] = true
		}
	}
	return occupied, nil
}

// countAvailableRoomsOfType counts active rooms of one room_type at an outlet with no guest
// stay overlapping [arrival, departure).
func (h *HotelHandler) countAvailableRoomsOfType(ctx context.Context, tid, outletID uuid.UUID, roomType string, arrival, departure time.Time) (int, error) {
	rooms, err := h.client.Room.Query().
		Where(entroom.TenantID(tid), entroom.OutletID(outletID), entroom.IsActive(true), entroom.RoomTypeEQ(entroom.RoomType(roomType))).
		All(ctx)
	if err != nil || len(rooms) == 0 {
		return 0, err
	}
	roomIDs := make([]uuid.UUID, len(rooms))
	for i, rm := range rooms {
		roomIDs[i] = rm.ID
	}
	occupied, err := h.roomsOccupiedInWindow(ctx, roomIDs, arrival, departure)
	if err != nil {
		return 0, err
	}
	available := 0
	for _, id := range roomIDs {
		if !occupied[id] {
			available++
		}
	}
	return available, nil
}

// resolveHospitalityOutlet loads an outlet and rejects it unless it belongs to this tenant and
// is actually running the hotel module — this endpoint has no RequireUseCase middleware
// (public), so this is the manual equivalent, preventing the widget from being pointed at an
// unrelated retail outlet.
func (h *HotelHandler) resolveHospitalityOutlet(ctx context.Context, tid, outletID uuid.UUID) bool {
	outlet, err := h.client.Outlet.Query().Where(entoutlet.ID(outletID), entoutlet.TenantID(tid)).Only(ctx)
	return err == nil && outlet.UseCase != nil && *outlet.UseCase == "hospitality"
}

type roomTypeAvailability struct {
	RoomType       string  `json:"room_type"`
	AvailableCount int     `json:"available_count"`
	TotalCount     int     `json:"total_count"`
	SampleRate     float64 `json:"sample_rate"`
	Currency       string  `json:"currency"`
}

// PublicRoomAvailability handles GET /{tenantID}/pos/room-bookings/availability?outlet_id=&
// arrival_date=&departure_date= (YYYY-MM-DD) — for each room_type at the outlet, how many
// rooms are free across the whole requested stay, plus an indicative average nightly rate.
func (h *HotelHandler) PublicRoomAvailability(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(r.URL.Query().Get("outlet_id"))
	if err != nil {
		jsonError(w, "outlet_id is required", http.StatusBadRequest)
		return
	}
	if !h.resolveHospitalityOutlet(r.Context(), tid, outletID) {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}
	arrival, err := parseFlexibleDate(r.URL.Query().Get("arrival_date"))
	if err != nil {
		jsonError(w, "arrival_date is required (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	departure, err := parseFlexibleDate(r.URL.Query().Get("departure_date"))
	if err != nil {
		jsonError(w, "departure_date is required (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	if !departure.After(arrival) {
		jsonError(w, "departure_date must be after arrival_date", http.StatusBadRequest)
		return
	}
	if arrival.Before(time.Now().Truncate(24 * time.Hour)) {
		jsonError(w, "arrival_date must not be in the past", http.StatusBadRequest)
		return
	}

	rooms, err := h.client.Room.Query().
		Where(entroom.TenantID(tid), entroom.OutletID(outletID), entroom.IsActive(true)).
		All(r.Context())
	if err != nil {
		h.log.Error("public room availability: room query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(rooms) == 0 {
		jsonOK(w, map[string]any{"data": []roomTypeAvailability{}})
		return
	}
	roomIDs := make([]uuid.UUID, len(rooms))
	for i, rm := range rooms {
		roomIDs[i] = rm.ID
	}
	occupied, err := h.roomsOccupiedInWindow(r.Context(), roomIDs, arrival, departure)
	if err != nil {
		h.log.Error("public room availability: guest query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type typeAgg struct {
		total, available, rateCount int
		rateSum                     float64
		currency                    string
	}
	byType := map[string]*typeAgg{}
	for _, rm := range rooms {
		agg, ok := byType[string(rm.RoomType)]
		if !ok {
			agg = &typeAgg{currency: rm.Currency}
			byType[string(rm.RoomType)] = agg
		}
		agg.total++
		if rm.RatePerNight > 0 {
			agg.rateSum += rm.RatePerNight
			agg.rateCount++
		}
		if !occupied[rm.ID] {
			agg.available++
		}
	}

	out := make([]roomTypeAvailability, 0, len(byType))
	for rt, agg := range byType {
		rate := 0.0
		if agg.rateCount > 0 {
			rate = agg.rateSum / float64(agg.rateCount)
		}
		out = append(out, roomTypeAvailability{
			RoomType: rt, AvailableCount: agg.available, TotalCount: agg.total,
			SampleRate: rate, Currency: agg.currency,
		})
	}
	jsonOK(w, map[string]any{"data": out})
}

type publicRoomBookingInput struct {
	OutletID      string `json:"outlet_id"`
	RoomType      string `json:"room_type"`
	RoomsCount    int    `json:"rooms_count"`
	ArrivalDate   string `json:"arrival_date"`
	DepartureDate string `json:"departure_date"`
	LeadGuestName string `json:"lead_guest_name"`
	Email         string `json:"email"`
	Phone         string `json:"phone"`
	Adults        int    `json:"adults"`
	Children      int    `json:"children"`
	Notes         string `json:"notes"`
}

// CreatePublicRoomBooking handles POST /{tenantID}/pos/room-bookings — the room-booking
// widget's submit call. Creates a RoomBooking with source=online, status=pending; a staff
// member reviews and confirms it from the Bookings page like any other amendment.
func (h *HotelHandler) CreatePublicRoomBooking(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var input publicRoomBookingInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(input.LeadGuestName) == "" {
		jsonError(w, "lead_guest_name is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(input.Phone) == "" && strings.TrimSpace(input.Email) == "" {
		jsonError(w, "phone or email is required so we can contact you", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(input.RoomType) == "" {
		jsonError(w, "room_type is required", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}
	if !h.resolveHospitalityOutlet(r.Context(), tid, outletID) {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}
	arrival, err := parseFlexibleDate(input.ArrivalDate)
	if err != nil {
		jsonError(w, "arrival_date is required (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	departure, err := parseFlexibleDate(input.DepartureDate)
	if err != nil {
		jsonError(w, "departure_date is required (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	if !departure.After(arrival) {
		jsonError(w, "departure_date must be after arrival_date", http.StatusBadRequest)
		return
	}
	if arrival.Before(time.Now().Truncate(24 * time.Hour)) {
		jsonError(w, "arrival_date must not be in the past", http.StatusBadRequest)
		return
	}
	if input.RoomsCount < 1 {
		input.RoomsCount = 1
	}

	// Re-check availability server-side (the widget already showed this from
	// PublicRoomAvailability, but never trust the client) before accepting.
	available, verr := h.countAvailableRoomsOfType(r.Context(), tid, outletID, input.RoomType, arrival, departure)
	if verr != nil {
		h.log.Error("public room booking: availability check failed", zap.Error(verr))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if available < input.RoomsCount {
		jsonError(w, "not enough rooms available for those dates", http.StatusConflict)
		return
	}

	meta := map[string]any{
		"room_type":    input.RoomType,
		"booking_type": "group",
		"adults":       input.Adults,
		"children":     input.Children,
	}
	if input.Notes != "" {
		meta["notes"] = input.Notes
	}

	b := h.client.RoomBooking.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetConfirmationNo("BK-" + uuid.NewString()[:8]).
		SetLeadGuestName(strings.TrimSpace(input.LeadGuestName)).
		SetRoomsCount(input.RoomsCount).
		SetArrivalDate(arrival).
		SetDepartureDate(departure).
		SetSource(entroombooking.SourceOnline).
		SetStatus(entroombooking.StatusPending).
		SetMetadata(meta)
	if input.Email != "" {
		b = b.SetEmail(input.Email)
	}
	if input.Phone != "" {
		b = b.SetPhone(input.Phone)
	}

	booking, err := b.Save(r.Context())
	if err != nil {
		h.log.Error("create public room booking failed", zap.Error(err))
		jsonError(w, "failed to create booking", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishHotelBookingCreated(r.Context(), tid, map[string]any{
			"booking_id": booking.ID, "confirmation_no": booking.ConfirmationNo,
			"rooms_count": booking.RoomsCount, "lead_guest_name": booking.LeadGuestName,
			"arrival_date": booking.ArrivalDate, "departure_date": booking.DepartureDate,
			"source": "online",
		})
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, booking)
}
