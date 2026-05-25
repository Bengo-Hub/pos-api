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
	"github.com/bengobox/pos-service/internal/platform/events"
	treasury "github.com/bengobox/pos-service/internal/modules/treasury"
)

// HotelHandler handles hotel management endpoints (rooms, guests, folio, facilities, bookings).
type HotelHandler struct {
	log            *zap.Logger
	client         *ent.Client
	publisher      *events.Publisher
	treasuryClient *treasury.Client
}

func NewHotelHandler(log *zap.Logger, client *ent.Client, publisher *events.Publisher) *HotelHandler {
	return &HotelHandler{log: log, client: client, publisher: publisher}
}

func (h *HotelHandler) SetTreasuryClient(c *treasury.Client) {
	h.treasuryClient = c
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
	OutletID     string  `json:"outlet_id"`
	RoomNumber   string  `json:"room_number"`
	Name         string  `json:"name"`
	RoomType     string  `json:"room_type"`
	Floor        int     `json:"floor"`
	RatePerNight float64 `json:"rate_per_night"`
	Currency     string  `json:"currency"`
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

	roomOutletID := parseOptionalUUID(input.OutletID, r)

	room, err := h.client.Room.Create().
		SetTenantID(tid).
		SetOutletID(roomOutletID).
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
	GuestName string `json:"guest_name"`
	Phone     string `json:"phone"`
	IDNumber  string `json:"id_number"`
	Nights    int    `json:"nights"`
	CheckedBy string `json:"checked_in_by"`
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
	checkedInBy, _ := uuid.Parse(input.CheckedBy)

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
		SetCheckedInBy(checkedInBy).
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
		SetCreatedBy(checkedInBy).
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

	if h.publisher != nil {
		_ = h.publisher.PublishHotelCheckIn(r.Context(), tid, map[string]any{
			"room_id":       roomID,
			"room_number":   room.RoomNumber,
			"guest_id":      guest.ID,
			"guest_name":    input.GuestName,
			"nights":        input.Nights,
			"total_charge":  totalCharge,
			"check_in_date": now,
		})
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, guest)
}

type checkOutInput struct {
	CheckedBy string `json:"checked_out_by"`
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
	checkedOutBy, _ := uuid.Parse(input.CheckedBy)
	tx, err := h.client.Tx(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Mark guest as checked out
	_, err = tx.RoomGuest.UpdateOne(guest).
		SetStatus(entroomguest.StatusCheckedOut).
		SetCheckedOutBy(checkedOutBy).
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

	// Auto-create housekeeping task for post-checkout clean
	guestIDCopy := guest.ID
	go func() {
		_, _ = h.client.HousekeepingTask.Create().
			SetTenantID(tid).
			SetRoomID(roomID).
			SetNillableRoomGuestID(&guestIDCopy).
			SetTaskType("checkout_clean").
			SetPriority("urgent").
			Save(r.Context())
	}()

	if h.publisher != nil {
		_ = h.publisher.PublishHotelCheckOut(r.Context(), tid, map[string]any{
			"room_id":        roomID,
			"guest_id":       guest.ID,
			"guest_name":     guest.GuestName,
			"total_folio":    totalFolio,
			"checked_out_at": now,
		})
	}

	resp := map[string]any{
		"guest":       guest,
		"total_folio": totalFolio,
		"status":      "checked_out",
	}

	// Create treasury payment intent for the folio total so pos-ui can present the payment modal.
	if h.treasuryClient != nil && totalFolio > 0 {
		tenantSlug := chi.URLParam(r, "tenantID")
		intent, err := h.treasuryClient.CreateIntent(r.Context(), tenantSlug, guest.ID.String(), treasury.CreateIntentRequest{
			SourceService: "pos",
			ReferenceID:   guest.ID.String(),
			ReferenceType: "hotel_folio",
			Amount:        totalFolio,
			Currency:      "KES",
			PaymentMethod: "pending",
			Description:   fmt.Sprintf("Hotel folio checkout - %s", guest.GuestName),
			Metadata: map[string]any{
				"room_id":  roomID,
				"guest_id": guest.ID,
			},
		})
		if err != nil {
			h.log.Warn("failed to create treasury intent for hotel folio", zap.Error(err))
		} else {
			resp["intent_id"] = intent.ID
			resp["intent_status"] = intent.Status
		}
	}

	jsonOK(w, resp)
}

type postFolioInput struct {
	Description string     `json:"description"`
	Amount      float64    `json:"amount"`
	ChargeType  string     `json:"charge_type"`
	POSOrderID  *uuid.UUID `json:"pos_order_id,omitempty"`
	CreatedBy   string     `json:"created_by"`
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
	folioCreatedBy, _ := uuid.Parse(input.CreatedBy)

	c := h.client.RoomFolioItem.Create().
		SetTenantID(tid).
		SetRoomID(roomID).
		SetRoomGuestID(guest.ID).
		SetDescription(input.Description).
		SetAmount(input.Amount).
		SetChargeType(chargeType).
		SetCreatedBy(folioCreatedBy)

	if input.POSOrderID != nil {
		c = c.SetPosOrderID(*input.POSOrderID)
	}

	item, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("post folio charge failed", zap.Error(err))
		jsonError(w, "failed to post charge", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishHotelFolioCharge(r.Context(), tid, map[string]any{
			"room_id":     roomID,
			"guest_id":    guest.ID,
			"item_id":     item.ID,
			"description": input.Description,
			"amount":      input.Amount,
			"charge_type": input.ChargeType,
		})
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
	OutletID       string  `json:"outlet_id"`
	Name           string  `json:"name"`
	FacilityType   string  `json:"facility_type"`
	Capacity       int     `json:"capacity"`
	RatePerSession float64 `json:"rate_per_session"`
	Currency       string  `json:"currency"`
	OpeningTime    string  `json:"opening_time"`
	ClosingTime    string  `json:"closing_time"`
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

	facilityOutletID := parseOptionalUUID(input.OutletID, r)

	facility, err := h.client.Facility.Create().
		SetTenantID(tid).
		SetOutletID(facilityOutletID).
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
	BookedBy    string     `json:"booked_by"`
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

	facilityBookedBy, _ := uuid.Parse(input.BookedBy)

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
		SetBookedBy(facilityBookedBy)

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

// CompleteFacilityBooking handles POST /{tenantID}/hotel/facilities/bookings/{bookingID}/complete
// Marks the booking completed and, if the guest is a hotel guest, auto-posts the charge to their folio.
func (h *HotelHandler) CompleteFacilityBooking(w http.ResponseWriter, r *http.Request) {
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

	booking, err := h.client.FacilityBooking.Query().
		Where(entfacilitybooking.ID(bookingID), entfacilitybooking.TenantID(tid)).
		WithFacility().
		Only(r.Context())
	if err != nil {
		jsonError(w, "booking not found", http.StatusNotFound)
		return
	}

	_, err = booking.Update().SetStatus(entfacilitybooking.StatusCompleted).Save(r.Context())
	if err != nil {
		jsonError(w, "failed to complete booking", http.StatusInternalServerError)
		return
	}

	// Auto-post charge to guest folio if this booking is linked to a hotel guest
	if booking.RoomGuestID != nil && booking.Amount > 0 {
		guest, guestErr := h.client.RoomGuest.Get(r.Context(), *booking.RoomGuestID)
		if guestErr == nil && guest.Status == entroomguest.StatusActive {
			facilityName := ""
			if booking.Edges.Facility != nil {
				facilityName = booking.Edges.Facility.Name
			}
			chargedBy, _ := uuid.Parse(r.Header.Get("X-User-ID"))
			_, _ = h.client.RoomFolioItem.Create().
				SetTenantID(tid).
				SetRoomID(guest.RoomID).
				SetRoomGuestID(guest.ID).
				SetDescription(fmt.Sprintf("Facility: %s (%s)", facilityName, booking.StartTime)).
				SetAmount(booking.Amount).
				SetChargeType(entroomfolioitem.ChargeTypeFacility).
				SetCreatedBy(chargedBy).
				Save(r.Context())
		}
	}

	jsonOK(w, map[string]any{"status": "completed", "booking_id": bookingID, "folio_charged": booking.RoomGuestID != nil && booking.Amount > 0})
}

// LateCheckout handles POST /{tenantID}/hotel/rooms/{id}/late-checkout
// Approves a late checkout and posts a late checkout surcharge to the guest's folio.
func (h *HotelHandler) LateCheckout(w http.ResponseWriter, r *http.Request) {
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

	var input struct {
		SurchargeAmount float64 `json:"surcharge_amount"`
		Notes           string  `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	guest, err := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "no active guest for this room", http.StatusNotFound)
		return
	}

	_, err = guest.Update().
		SetLateCheckoutApproved(true).
		SetLateCheckoutSurcharge(input.SurchargeAmount).
		Save(r.Context())
	if err != nil {
		jsonError(w, "failed to approve late checkout", http.StatusInternalServerError)
		return
	}

	// Post surcharge to folio if amount > 0
	if input.SurchargeAmount > 0 {
		chargedBy, _ := uuid.Parse(r.Header.Get("X-User-ID"))
		desc := "Late checkout surcharge"
		if input.Notes != "" {
			desc += " - " + input.Notes
		}
		_, _ = h.client.RoomFolioItem.Create().
			SetTenantID(tid).
			SetRoomID(roomID).
			SetRoomGuestID(guest.ID).
			SetDescription(desc).
			SetAmount(input.SurchargeAmount).
			SetChargeType(entroomfolioitem.ChargeTypeLateCheckout).
			SetCreatedBy(chargedBy).
			Save(r.Context())
	}

	jsonOK(w, map[string]any{
		"guest_id":              guest.ID,
		"late_checkout_approved": true,
		"surcharge_amount":      input.SurchargeAmount,
	})
}

// BatchCheckout handles POST /{tenantID}/hotel/rooms/batch-checkout
// Checks out multiple rooms at once (e.g., tour groups).
func (h *HotelHandler) BatchCheckout(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		RoomIDs   []string `json:"room_ids"`
		CheckedBy string   `json:"checked_out_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.RoomIDs) == 0 {
		jsonError(w, "room_ids is required", http.StatusBadRequest)
		return
	}

	checkedOutBy, _ := uuid.Parse(input.CheckedBy)
	now := time.Now()
	ctx := r.Context()

	type batchResult struct {
		RoomID     string  `json:"room_id"`
		GuestName  string  `json:"guest_name"`
		TotalFolio float64 `json:"total_folio"`
		Error      string  `json:"error,omitempty"`
	}
	results := make([]batchResult, 0, len(input.RoomIDs))

	for _, ridStr := range input.RoomIDs {
		roomID, err := uuid.Parse(ridStr)
		if err != nil {
			results = append(results, batchResult{RoomID: ridStr, Error: "invalid room_id"})
			continue
		}

		guest, err := h.client.RoomGuest.Query().
			Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
			Only(ctx)
		if err != nil {
			results = append(results, batchResult{RoomID: ridStr, Error: "no active guest"})
			continue
		}

		items, _ := h.client.RoomFolioItem.Query().
			Where(entroomfolioitem.TenantID(tid), entroomfolioitem.RoomGuestID(guest.ID)).
			All(ctx)
		var totalFolio float64
		for _, item := range items {
			totalFolio += item.Amount
		}

		guest.Update().
			SetStatus(entroomguest.StatusCheckedOut).
			SetCheckedOutBy(checkedOutBy).
			SetCheckedOutAt(now).
			Exec(ctx) //nolint
		h.client.Room.UpdateOneID(roomID).SetStatus(entroom.StatusCleaning).Exec(ctx) //nolint

		// Auto-create housekeeping task
		gid := guest.ID
		h.client.HousekeepingTask.Create().
			SetTenantID(tid).
			SetRoomID(roomID).
			SetNillableRoomGuestID(&gid).
			SetTaskType("checkout_clean").
			SetPriority("urgent").
			Exec(ctx) //nolint

		results = append(results, batchResult{
			RoomID:     ridStr,
			GuestName:  guest.GuestName,
			TotalFolio: totalFolio,
		})
	}

	jsonOK(w, map[string]any{"results": results, "processed": len(results)})
}
