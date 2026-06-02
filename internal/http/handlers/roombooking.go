package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/pos-service/internal/ent"
	entroombooking "github.com/bengobox/pos-service/internal/ent/roombooking"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// roomBookingInput is the request body for creating a multi-room (group) booking header.
type roomBookingInput struct {
	ConfirmationNo            string    `json:"confirmation_no"`
	LeadGuestName             string    `json:"lead_guest_name"`
	Email                     string    `json:"email"`
	Phone                     string    `json:"phone"`
	RoomsCount                int       `json:"rooms_count"`
	ArrivalDate               time.Time `json:"arrival_date"`
	DepartureDate             time.Time `json:"departure_date"`
	InventoryRatePlanBundleID string    `json:"inventory_rate_plan_bundle_id"`
	MarketSegment             string    `json:"market_segment"`
	Source                    string    `json:"source"`
	CRMContactID              string    `json:"crm_contact_id"`
	CreatedBy                 string    `json:"created_by"`
}

// CreateRoomBooking handles POST /{tenantID}/hotel/bookings — create a group/multi-room booking header.
func (h *HotelHandler) CreateRoomBooking(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var input roomBookingInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.LeadGuestName == "" {
		jsonError(w, "lead_guest_name is required", http.StatusBadRequest)
		return
	}
	if input.RoomsCount < 1 {
		input.RoomsCount = 1
	}
	if input.ConfirmationNo == "" {
		input.ConfirmationNo = "BK-" + uuid.NewString()[:8]
	}

	outletID, _ := uuid.Parse(httpware.GetOutletID(r.Context()))
	createdBy, _ := uuid.Parse(input.CreatedBy)

	b := h.client.RoomBooking.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetConfirmationNo(input.ConfirmationNo).
		SetLeadGuestName(input.LeadGuestName).
		SetEmail(input.Email).
		SetPhone(input.Phone).
		SetRoomsCount(input.RoomsCount).
		SetArrivalDate(input.ArrivalDate).
		SetDepartureDate(input.DepartureDate).
		SetMarketSegment(input.MarketSegment).
		SetCreatedBy(createdBy)
	if input.Source != "" {
		b = b.SetSource(entroombooking.Source(input.Source))
	}
	if bundleID, perr := uuid.Parse(input.InventoryRatePlanBundleID); perr == nil {
		b = b.SetInventoryRatePlanBundleID(bundleID)
	}
	if crmID, perr := uuid.Parse(input.CRMContactID); perr == nil {
		b = b.SetCrmContactID(crmID)
	}
	booking, err := b.Save(r.Context())
	if err != nil {
		h.log.Error("create room booking failed", zap.Error(err))
		jsonError(w, "failed to create booking", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishHotelBookingCreated(r.Context(), tid, map[string]any{
			"booking_id":      booking.ID,
			"confirmation_no": booking.ConfirmationNo,
			"rooms_count":     booking.RoomsCount,
			"lead_guest_name": booking.LeadGuestName,
			"arrival_date":    booking.ArrivalDate,
			"departure_date":  booking.DepartureDate,
		})
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, booking)
}

// ListRoomBookings handles GET /{tenantID}/hotel/bookings.
func (h *HotelHandler) ListRoomBookings(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.client.RoomBooking.Query().Where(entroombooking.TenantID(tid))
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			q = q.Where(entroombooking.OutletID(oid))
		}
	}
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entroombooking.StatusEQ(entroombooking.Status(status)))
	}
	bookings, err := q.Order(ent.Desc(entroombooking.FieldArrivalDate)).Limit(200).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, bookings)
}

// GetRoomBooking handles GET /{tenantID}/hotel/bookings/{id} with its guest rows.
func (h *HotelHandler) GetRoomBooking(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid booking id", http.StatusBadRequest)
		return
	}
	booking, err := h.client.RoomBooking.Query().
		Where(entroombooking.ID(id), entroombooking.TenantID(tid)).
		WithGuests().
		Only(r.Context())
	if err != nil {
		jsonError(w, "booking not found", http.StatusNotFound)
		return
	}
	jsonOK(w, booking)
}

// inventoryServiceItemDTO is the lightweight item shape returned to hotel forms for picking
// the authoritative inventory master (room-type / facility / amenity).
type inventoryServiceItemDTO struct {
	ID    string `json:"id"`
	SKU   string `json:"sku"`
	Name  string `json:"name"`
	Image string `json:"image_url,omitempty"`
}

// ListInventoryServiceItems handles GET /{tenantID}/hotel/inventory-service-items?use_case=...
// Proxies inventory-api for SERVICE master items of a hospitality use_case so the room/facility/
// amenity forms can link to the authoritative inventory item (and inherit its rate/pricing).
func (h *HotelHandler) ListInventoryServiceItems(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	useCase := r.URL.Query().Get("use_case")
	if useCase == "" {
		jsonError(w, "use_case is required", http.StatusBadRequest)
		return
	}
	// Resolve tenant slug (inventory /v1/{tenant} accepts slug or UUID; prefer slug from context).
	tenantSlug := httpware.GetTenantSlug(r.Context())
	if tenantSlug == "" {
		if t, lookupErr := h.client.Tenant.Get(r.Context(), tid); lookupErr == nil {
			tenantSlug = t.Slug
		}
	}
	if tenantSlug == "" {
		tenantSlug = tid.String()
	}
	items, err := fetchInventoryServiceItems(r.Context(), tenantSlug, useCase)
	if err != nil {
		h.log.Error("list inventory service items failed", zap.Error(err))
		jsonError(w, "failed to fetch inventory items", http.StatusBadGateway)
		return
	}
	out := make([]inventoryServiceItemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, inventoryServiceItemDTO{ID: it.ID, SKU: it.SKU, Name: it.Name, Image: it.ImageURL})
	}
	jsonOK(w, out)
}

// ListBookingGuests handles GET /{tenantID}/hotel/bookings/{id}/guests.
func (h *HotelHandler) ListBookingGuests(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid booking id", http.StatusBadRequest)
		return
	}
	guests, err := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.BookingID(id)).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, guests)
}
