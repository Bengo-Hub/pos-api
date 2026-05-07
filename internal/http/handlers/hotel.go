package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entfacility "github.com/bengobox/pos-service/internal/ent/facility"
	entfacilitybooking "github.com/bengobox/pos-service/internal/ent/facilitybooking"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// HotelHandler handles hotel management endpoints (rooms, guests, folio, facilities, bookings).
type HotelHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewHotelHandler(log *zap.Logger, client *ent.Client) *HotelHandler {
	return &HotelHandler{log: log, client: client}
}

// --- Rooms ---

// ListRooms handles GET /{tenantID}/hotel/rooms
func (h *HotelHandler) ListRooms(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.Room.Query().Where(entroom.TenantID(tid))

	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entroom.StatusEQ(entroom.Status(status)))
	}
	if floorStr := r.URL.Query().Get("floor"); floorStr != "" {
		var f int
		if n, err := fmt.Sscanf(floorStr, "%d", &f); err == nil && n == 1 {
			q = q.Where(entroom.Floor(f))
		}
	}

	rooms, err := q.Order(ent.Asc(entroom.FieldFloor), ent.Asc(entroom.FieldRoomNumber)).All(r.Context())
	if err != nil {
		h.log.Error("list rooms failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": rooms, "total": len(rooms)})
}

// GetRoom handles GET /{tenantID}/hotel/rooms/{id}
func (h *HotelHandler) GetRoom(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid room id", http.StatusBadRequest)
		return
	}

	room, err := h.client.Room.Query().
		Where(entroom.ID(roomID), entroom.TenantID(tid)).
		WithGuests(func(q *ent.RoomGuestQuery) {
			q.Where(entroomguest.StatusEQ(entroomguest.StatusActive)).Limit(1)
		}).
		WithFolioItems().
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "room not found", http.StatusNotFound)
			return
		}
		h.log.Error("get room failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, room)
}

type createRoomInput struct {
	OutletID     uuid.UUID `json:"outlet_id"`
	RoomNumber   string    `json:"room_number"`
	Name         string    `json:"name"`
	RoomType     string    `json:"room_type"`
	Floor        int       `json:"floor"`
	RatePerNight float64   `json:"rate_per_night"`
	Currency     string    `json:"currency"`
}

// CreateRoom handles POST /{tenantID}/hotel/rooms
func (h *HotelHandler) CreateRoom(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createRoomInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if input.Currency == "" {
		input.Currency = "KES"
	}
	roomType := entroom.RoomType(input.RoomType)
	if input.RoomType == "" {
		roomType = entroom.RoomTypeStandard
	}

	room, err := h.client.Room.Create().
		SetTenantID(tid).
		SetOutletID(input.OutletID).
		SetRoomNumber(input.RoomNumber).
		SetName(input.Name).
		SetRoomType(roomType).
		SetFloor(input.Floor).
		SetRatePerNight(input.RatePerNight).
		SetCurrency(input.Currency).
		Save(r.Context())
	if err != nil {
		h.log.Error("create room failed", zap.Error(err))
		jsonError(w, "failed to create room", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, room)
}

type updateRoomStatusInput struct {
	Status string `json:"status"`
}

// UpdateRoomStatus handles PATCH /{tenantID}/hotel/rooms/{id}/status
func (h *HotelHandler) UpdateRoomStatus(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid room id", http.StatusBadRequest)
		return
	}

	var input updateRoomStatusInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	room, err := h.client.Room.Query().
		Where(entroom.ID(roomID), entroom.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "room not found", http.StatusNotFound)
		return
	}

	updated, err := room.Update().SetStatus(entroom.Status(input.Status)).Save(r.Context())
	if err != nil {
		h.log.Error("update room status failed", zap.Error(err))
		jsonError(w, "failed to update room status", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

type checkInInput struct {
	GuestName  string    `json:"guest_name"`
	Phone      string    `json:"phone"`
	IDNumber   string    `json:"id_number"`
	Nights     int       `json:"nights"`
	CheckedBy  uuid.UUID `json:"checked_in_by"`
}

// CheckIn handles POST /{tenantID}/hotel/rooms/{id}/check-in
func (h *HotelHandler) CheckIn(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid room id", http.StatusBadRequest)
		return
	}

	var input checkInInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Nights < 1 {
		jsonError(w, "nights must be at least 1", http.StatusBadRequest)
		return
	}

	room, err := h.client.Room.Query().
		Where(entroom.ID(roomID), entroom.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "room not found", http.StatusNotFound)
		return
	}
	if room.Status != entroom.StatusAvailable && room.Status != entroom.StatusReserved {
		jsonError(w, "room is not available for check-in", http.StatusConflict)
		return
	}

	now := time.Now()
	checkOutDate := now.AddDate(0, 0, input.Nights)
	totalCharge := room.RatePerNight * float64(input.Nights)

	tx, err := h.client.Tx(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	guest, err := tx.RoomGuest.Create().
		SetTenantID(tid).
		SetRoomID(roomID).
		SetGuestName(input.GuestName).
		SetPhone(input.Phone).
		SetIDNumber(input.IDNumber).
		SetCheckInDate(now).
		SetNights(input.Nights).
		SetCheckOutDate(checkOutDate).
		SetTotalRoomCharge(totalCharge).
		SetCheckedInBy(input.CheckedBy).
		SetCheckedInAt(now).
		Save(r.Context())
	if err != nil {
		_ = tx.Rollback()
		h.log.Error("create room guest failed", zap.Error(err))
		jsonError(w, "failed to check in guest", http.StatusInternalServerError)
		return
	}

	// Post initial room charge to folio
	_, err = tx.RoomFolioItem.Create().
		SetTenantID(tid).
		SetRoomID(roomID).
		SetRoomGuestID(guest.ID).
		SetDescription("Room charge").
		SetAmount(totalCharge).
		SetCurrency(room.Currency).
		SetChargeType(entroomfolioitem.ChargeTypeRoomCharge).
		SetCreatedBy(input.CheckedBy).
		Save(r.Context())
	if err != nil {
		_ = tx.Rollback()
		h.log.Error("create folio item failed", zap.Error(err))
		jsonError(w, "failed to post room charge", http.StatusInternalServerError)
		return
	}

	// Mark room as occupied
	_, err = tx.Room.UpdateOne(room).SetStatus(entroom.StatusOccupied).Save(r.Context())
	if err != nil {
		_ = tx.Rollback()
		jsonError(w, "failed to update room status", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, guest)
}

type checkOutInput struct {
	CheckedBy uuid.UUID `json:"checked_out_by"`
}

// CheckOut handles POST /{tenantID}/hotel/rooms/{id}/check-out
func (h *HotelHandler) CheckOut(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid room id", http.StatusBadRequest)
		return
	}

	var input checkOutInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Find active guest
	guest, err := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "no active guest for this room", http.StatusNotFound)
		return
	}

	// Sum all folio charges
	items, err := h.client.RoomFolioItem.Query().
		Where(entroomfolioitem.TenantID(tid), entroomfolioitem.RoomGuestID(guest.ID)).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	var totalFolio float64
	for _, item := range items {
		totalFolio += item.Amount
	}

	now := time.Now()
	tx, err := h.client.Tx(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Mark guest as checked out
	_, err = tx.RoomGuest.UpdateOne(guest).
		SetStatus(entroomguest.StatusCheckedOut).
		SetCheckedOutBy(input.CheckedBy).
		SetCheckedOutAt(now).
		Save(r.Context())
	if err != nil {
		_ = tx.Rollback()
		jsonError(w, "failed to check out guest", http.StatusInternalServerError)
		return
	}

	// Mark room as cleaning
	_, err = tx.Room.UpdateOneID(roomID).SetStatus(entroom.StatusCleaning).Save(r.Context())
	if err != nil {
		_ = tx.Rollback()
		jsonError(w, "failed to update room status", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{
		"guest":       guest,
		"total_folio": totalFolio,
		"status":      "checked_out",
	})
}

type postFolioInput struct {
	Description string    `json:"description"`
	Amount      float64   `json:"amount"`
	ChargeType  string    `json:"charge_type"`
	POSOrderID  *uuid.UUID `json:"pos_order_id,omitempty"`
	CreatedBy   uuid.UUID `json:"created_by"`
}

// PostFolioCharge handles POST /{tenantID}/hotel/rooms/{id}/folio
func (h *HotelHandler) PostFolioCharge(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid room id", http.StatusBadRequest)
		return
	}

	var input postFolioInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Find active guest
	guest, err := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "no active guest for this room", http.StatusNotFound)
		return
	}

	chargeType := entroomfolioitem.ChargeType(input.ChargeType)
	if input.ChargeType == "" {
		chargeType = entroomfolioitem.ChargeTypeOther
	}

	c := h.client.RoomFolioItem.Create().
		SetTenantID(tid).
		SetRoomID(roomID).
		SetRoomGuestID(guest.ID).
		SetDescription(input.Description).
		SetAmount(input.Amount).
		SetChargeType(chargeType).
		SetCreatedBy(input.CreatedBy)

	if input.POSOrderID != nil {
		c = c.SetPosOrderID(*input.POSOrderID)
	}

	item, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("post folio charge failed", zap.Error(err))
		jsonError(w, "failed to post charge", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, item)
}

// GetRoomFolio handles GET /{tenantID}/hotel/rooms/{id}/folio
func (h *HotelHandler) GetRoomFolio(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid room id", http.StatusBadRequest)
		return
	}

	items, err := h.client.RoomFolioItem.Query().
		Where(entroomfolioitem.TenantID(tid), entroomfolioitem.RoomID(roomID)).
		Order(ent.Desc(entroomfolioitem.FieldCreatedAt)).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	var total float64
	for _, item := range items {
		total += item.Amount
	}

	jsonOK(w, map[string]any{"data": items, "total_amount": total})
}

// --- Facilities ---

// ListFacilities handles GET /{tenantID}/hotel/facilities
func (h *HotelHandler) ListFacilities(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	facilities, err := h.client.Facility.Query().
		Where(entfacility.TenantID(tid), entfacility.IsActive(true)).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": facilities, "total": len(facilities)})
}

// GetFacility handles GET /{tenantID}/hotel/facilities/{id}
func (h *HotelHandler) GetFacility(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	facilityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid facility id", http.StatusBadRequest)
		return
	}

	facility, err := h.client.Facility.Query().
		Where(entfacility.ID(facilityID), entfacility.TenantID(tid)).
		WithBookings(func(q *ent.FacilityBookingQuery) {
			q.Where(entfacilitybooking.StatusEQ(entfacilitybooking.StatusConfirmed)).
				Order(ent.Asc(entfacilitybooking.FieldSessionDate))
		}).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "facility not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, facility)
}

type createFacilityInput struct {
	OutletID       uuid.UUID `json:"outlet_id"`
	Name           string    `json:"name"`
	FacilityType   string    `json:"facility_type"`
	Capacity       int       `json:"capacity"`
	RatePerSession float64   `json:"rate_per_session"`
	Currency       string    `json:"currency"`
	OpeningTime    string    `json:"opening_time"`
	ClosingTime    string    `json:"closing_time"`
}

// CreateFacility handles POST /{tenantID}/hotel/facilities
func (h *HotelHandler) CreateFacility(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createFacilityInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if input.Currency == "" {
		input.Currency = "KES"
	}
	facilityType := entfacility.FacilityType(input.FacilityType)
	if input.FacilityType == "" {
		facilityType = entfacility.FacilityTypeOther
	}

	facility, err := h.client.Facility.Create().
		SetTenantID(tid).
		SetOutletID(input.OutletID).
		SetName(input.Name).
		SetFacilityType(facilityType).
		SetCapacity(input.Capacity).
		SetRatePerSession(input.RatePerSession).
		SetCurrency(input.Currency).
		SetOpeningTime(input.OpeningTime).
		SetClosingTime(input.ClosingTime).
		Save(r.Context())
	if err != nil {
		h.log.Error("create facility failed", zap.Error(err))
		jsonError(w, "failed to create facility", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, facility)
}

type bookFacilityInput struct {
	GuestName   string     `json:"guest_name"`
	Phone       string     `json:"phone"`
	SessionDate time.Time  `json:"session_date"`
	StartTime   string     `json:"start_time"`
	EndTime     string     `json:"end_time"`
	GuestsCount int        `json:"guests_count"`
	Amount      float64    `json:"amount"`
	RoomGuestID *uuid.UUID `json:"room_guest_id,omitempty"`
	BookedBy    uuid.UUID  `json:"booked_by"`
	Notes       string     `json:"notes"`
}

// BookFacility handles POST /{tenantID}/hotel/facilities/{id}/book
func (h *HotelHandler) BookFacility(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	facilityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid facility id", http.StatusBadRequest)
		return
	}

	var input bookFacilityInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.GuestsCount < 1 {
		input.GuestsCount = 1
	}

	c := h.client.FacilityBooking.Create().
		SetTenantID(tid).
		SetFacilityID(facilityID).
		SetGuestName(input.GuestName).
		SetPhone(input.Phone).
		SetSessionDate(input.SessionDate).
		SetStartTime(input.StartTime).
		SetEndTime(input.EndTime).
		SetGuestsCount(input.GuestsCount).
		SetAmount(input.Amount).
		SetBookedBy(input.BookedBy)

	if input.RoomGuestID != nil {
		c = c.SetRoomGuestID(*input.RoomGuestID)
	}
	if input.Notes != "" {
		c = c.SetNotes(input.Notes)
	}

	booking, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("book facility failed", zap.Error(err))
		jsonError(w, "failed to create booking", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, booking)
}

type updateBookingInput struct {
	Status string `json:"status"`
}

// UpdateBooking handles PATCH /{tenantID}/hotel/facilities/bookings/{id}
func (h *HotelHandler) UpdateBooking(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	bookingID, err := uuid.Parse(chi.URLParam(r, "bookingID"))
	if err != nil {
		jsonError(w, "invalid booking id", http.StatusBadRequest)
		return
	}

	var input updateBookingInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	booking, err := h.client.FacilityBooking.Query().
		Where(entfacilitybooking.ID(bookingID), entfacilitybooking.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "booking not found", http.StatusNotFound)
		return
	}

	updated, err := booking.Update().SetStatus(entfacilitybooking.Status(input.Status)).Save(r.Context())
	if err != nil {
		jsonError(w, "failed to update booking", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// ListFacilityBookings handles GET /{tenantID}/hotel/facilities/bookings
func (h *HotelHandler) ListFacilityBookings(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.FacilityBooking.Query().Where(entfacilitybooking.TenantID(tid))

	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entfacilitybooking.StatusEQ(entfacilitybooking.Status(status)))
	}

	bookings, err := q.Order(ent.Asc(entfacilitybooking.FieldSessionDate)).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": bookings, "total": len(bookings)})
}
