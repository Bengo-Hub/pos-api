package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	entroomamenity "github.com/bengobox/pos-service/internal/ent/roomamenity"
	entroomamenityassignment "github.com/bengobox/pos-service/internal/ent/roomamenityassignment"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// ListAmenities handles GET /{tenantID}/hotel/amenities
func (h *HotelHandler) ListAmenities(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.RoomAmenity.Query().
		Where(entroomamenity.TenantID(tid), entroomamenity.IsActive(true))

	if at := r.URL.Query().Get("amenity_type"); at != "" {
		q = q.Where(entroomamenity.AmenityTypeEQ(entroomamenity.AmenityType(at)))
	}

	amenities, err := q.All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": amenities, "total": len(amenities)})
}

type createAmenityInput struct {
	OutletID       string  `json:"outlet_id"`
	Name           string  `json:"name"`
	AmenityType    string  `json:"amenity_type"`
	Description    string  `json:"description"`
	BillingMode    string  `json:"billing_mode"`
	Rate           float64 `json:"rate"`
	Currency       string  `json:"currency"`
}

// CreateAmenity handles POST /{tenantID}/hotel/amenities
func (h *HotelHandler) CreateAmenity(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createAmenityInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	outletID := parseOptionalUUID(input.OutletID, r)
	amenityType := entroomamenity.AmenityTypeOther
	if input.AmenityType != "" {
		amenityType = entroomamenity.AmenityType(input.AmenityType)
	}
	billingMode := entroomamenity.BillingModeFree
	if input.BillingMode != "" {
		billingMode = entroomamenity.BillingMode(input.BillingMode)
	}
	currency := input.Currency
	if currency == "" {
		currency = "KES"
	}

	c := h.client.RoomAmenity.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetName(input.Name).
		SetAmenityType(amenityType).
		SetBillingMode(billingMode).
		SetRate(input.Rate).
		SetCurrency(currency)

	if input.Description != "" {
		c = c.SetDescription(input.Description)
	}

	amenity, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create amenity failed", zap.Error(err))
		jsonError(w, "failed to create amenity", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, amenity)
}

// ListRoomAmenities handles GET /{tenantID}/hotel/rooms/{id}/amenities
func (h *HotelHandler) ListRoomAmenities(w http.ResponseWriter, r *http.Request) {
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

	assignments, err := h.client.RoomAmenityAssignment.Query().
		Where(entroomamenityassignment.TenantID(tid), entroomamenityassignment.RoomID(roomID)).
		WithAmenity().
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": assignments, "total": len(assignments)})
}

type assignAmenityInput struct {
	AmenityID  string `json:"amenity_id"`
	IsIncluded bool   `json:"is_included"`
	Notes      string `json:"notes"`
}

// AssignAmenityToRoom handles POST /{tenantID}/hotel/rooms/{id}/amenities
func (h *HotelHandler) AssignAmenityToRoom(w http.ResponseWriter, r *http.Request) {
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

	var input assignAmenityInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	amenityID, err := uuid.Parse(input.AmenityID)
	if err != nil {
		jsonError(w, "invalid amenity_id", http.StatusBadRequest)
		return
	}

	c := h.client.RoomAmenityAssignment.Create().
		SetTenantID(tid).
		SetRoomID(roomID).
		SetAmenityID(amenityID).
		SetIsIncluded(input.IsIncluded)
	if input.Notes != "" {
		c = c.SetNotes(input.Notes)
	}

	assignment, err := c.Save(r.Context())
	if err != nil {
		jsonError(w, "failed to assign amenity", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, assignment)
}

// ChargeAmenityToGuest handles POST /{tenantID}/hotel/rooms/{id}/amenities/{amenityId}/charge
// Posts an amenity charge to the active guest's folio.
func (h *HotelHandler) ChargeAmenityToGuest(w http.ResponseWriter, r *http.Request) {
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
	amenityID, err := uuid.Parse(chi.URLParam(r, "amenityId"))
	if err != nil {
		jsonError(w, "invalid amenity id", http.StatusBadRequest)
		return
	}

	var input struct {
		Quantity    int    `json:"quantity"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Quantity < 1 {
		input.Quantity = 1
	}

	amenity, err := h.client.RoomAmenity.Get(r.Context(), amenityID)
	if err != nil {
		jsonError(w, "amenity not found", http.StatusNotFound)
		return
	}

	guest, err := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "no active guest for this room", http.StatusNotFound)
		return
	}

	desc := input.Description
	if desc == "" {
		desc = amenity.Name
	}
	chargedBy, _ := uuid.Parse(r.Header.Get("X-User-ID"))

	item, err := h.client.RoomFolioItem.Create().
		SetTenantID(tid).
		SetRoomID(roomID).
		SetRoomGuestID(guest.ID).
		SetDescription(desc).
		SetAmount(amenity.Rate * float64(input.Quantity)).
		SetCurrency(amenity.Currency).
		SetChargeType(entroomfolioitem.ChargeTypeAmenity).
		SetCreatedBy(chargedBy).
		Save(r.Context())
	if err != nil {
		jsonError(w, "failed to post amenity charge", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, item)
}
