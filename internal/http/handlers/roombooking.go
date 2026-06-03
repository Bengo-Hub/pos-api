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
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entroombooking "github.com/bengobox/pos-service/internal/ent/roombooking"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// bookingPolicy is the tenant/outlet amendment & cancellation policy, stored in
// OutletSetting.metadata["booking_policy"] (no dedicated schema → no migration).
// Fees apply when an amendment/cancellation happens INSIDE the configured window
// (i.e. fewer than N hours before arrival). Fees default to 0 (no penalty) until a
// business configures them.
type bookingPolicy struct {
	FreeAmendmentWindowHours float64 `json:"free_amendment_window_hours"`
	CancellationWindowHours  float64 `json:"cancellation_window_hours"`
	AmendmentFee             float64 `json:"amendment_fee"`
	CancellationFee          float64 `json:"cancellation_fee"`
	Currency                 string  `json:"currency"`
}

func defaultBookingPolicy() bookingPolicy {
	return bookingPolicy{FreeAmendmentWindowHours: 48, CancellationWindowHours: 72, Currency: "KES"}
}

// resolveBookingPolicy reads the outlet's booking policy from OutletSetting.metadata,
// falling back to sensible defaults (free windows, zero fees) when unset.
func (h *HotelHandler) resolveBookingPolicy(r *http.Request, tid, outletID uuid.UUID) bookingPolicy {
	policy := defaultBookingPolicy()
	if outletID == uuid.Nil {
		return policy
	}
	setting, err := h.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Only(r.Context())
	if err != nil || setting.Metadata == nil {
		return policy
	}
	raw, ok := setting.Metadata["booking_policy"]
	if !ok {
		return policy
	}
	b, mErr := json.Marshal(raw)
	if mErr != nil {
		return policy
	}
	_ = json.Unmarshal(b, &policy) // best-effort; keep defaults for missing keys
	if policy.Currency == "" {
		policy.Currency = "KES"
	}
	return policy
}

// computeBookingFee returns the fee owed for an amendment/cancellation given the policy
// and how far ahead of arrival the action happens. Cancellation when isCancel=true.
func computeBookingFee(policy bookingPolicy, arrival, now time.Time, isCancel bool) float64 {
	hoursAhead := arrival.Sub(now).Hours()
	if isCancel {
		if hoursAhead < policy.CancellationWindowHours {
			return policy.CancellationFee
		}
		return 0
	}
	if hoursAhead < policy.FreeAmendmentWindowHours {
		return policy.AmendmentFee
	}
	return 0
}

// roomBookingInput is the request body for creating a multi-room (group) booking header.
type roomBookingInput struct {
	ConfirmationNo            string    `json:"confirmation_no"`
	LeadGuestName             string    `json:"lead_guest_name"`
	Email                     string    `json:"email"`
	Phone                     string    `json:"phone"`
	RoomsCount                int       `json:"rooms_count"`
	ArrivalDate               time.Time `json:"arrival_date"`
	DepartureDate             time.Time `json:"departure_date"`
	InventoryRatePlanBundleID string         `json:"inventory_rate_plan_bundle_id"`
	MarketSegment             string         `json:"market_segment"`
	Source                    string         `json:"source"`
	CRMContactID              string         `json:"crm_contact_id"`
	CreatedBy                 string         `json:"created_by"`
	// Metadata carries flexible, non-relational booking details (booking_type,
	// adults, children, notes, package_inclusions) — stored as-is on the booking.
	Metadata map[string]any `json:"metadata"`
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
	if len(input.Metadata) > 0 {
		b = b.SetMetadata(input.Metadata)
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

type updateRoomBookingInput struct {
	LeadGuestName *string        `json:"lead_guest_name"`
	Email         *string        `json:"email"`
	Phone         *string        `json:"phone"`
	RoomsCount    *int           `json:"rooms_count"`
	ArrivalDate   *time.Time     `json:"arrival_date"`
	DepartureDate *time.Time     `json:"departure_date"`
	MarketSegment *string        `json:"market_segment"`
	Status        *string        `json:"status"`
	Metadata      map[string]any `json:"metadata"`
}

// UpdateRoomBooking handles PATCH /{tenantID}/hotel/bookings/{id} — amend or cancel a
// group/individual booking. Applies the outlet's amendment/cancellation fee policy based on
// how close to arrival the change is made, records the fee on the booking metadata, and
// returns it as `applied_fee` so the UI can surface what the guest owes.
func (h *HotelHandler) UpdateRoomBooking(w http.ResponseWriter, r *http.Request) {
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
	var in updateRoomBookingInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	existing, err := h.client.RoomBooking.Query().
		Where(entroombooking.ID(id), entroombooking.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "booking not found", http.StatusNotFound)
		return
	}

	isCancel := in.Status != nil && *in.Status == "cancelled"
	isAmendment := in.ArrivalDate != nil || in.DepartureDate != nil || in.RoomsCount != nil
	policy := h.resolveBookingPolicy(r, tid, existing.OutletID)
	appliedFee := 0.0
	if isCancel {
		appliedFee = computeBookingFee(policy, existing.ArrivalDate, time.Now(), true)
	} else if isAmendment {
		appliedFee = computeBookingFee(policy, existing.ArrivalDate, time.Now(), false)
	}

	upd := existing.Update()
	if in.LeadGuestName != nil {
		upd = upd.SetLeadGuestName(*in.LeadGuestName)
	}
	if in.Email != nil {
		upd = upd.SetEmail(*in.Email)
	}
	if in.Phone != nil {
		upd = upd.SetPhone(*in.Phone)
	}
	if in.RoomsCount != nil && *in.RoomsCount >= 1 {
		upd = upd.SetRoomsCount(*in.RoomsCount)
	}
	if in.ArrivalDate != nil {
		upd = upd.SetArrivalDate(*in.ArrivalDate)
	}
	if in.DepartureDate != nil {
		upd = upd.SetDepartureDate(*in.DepartureDate)
	}
	if in.MarketSegment != nil {
		upd = upd.SetMarketSegment(*in.MarketSegment)
	}
	if in.Status != nil && *in.Status != "" {
		upd = upd.SetStatus(entroombooking.Status(*in.Status))
	}

	// Merge metadata: preserve existing, apply caller changes, then record any fee charged.
	meta := map[string]any{}
	for k, v := range existing.Metadata {
		meta[k] = v
	}
	for k, v := range in.Metadata {
		meta[k] = v
	}
	if appliedFee > 0 {
		feeKey := "last_amendment_fee"
		if isCancel {
			feeKey = "cancellation_fee"
		}
		meta[feeKey] = appliedFee
		meta["fee_currency"] = policy.Currency
		meta["fee_charged_at"] = time.Now().Format(time.RFC3339)
	}
	upd = upd.SetMetadata(meta)

	booking, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update room booking failed", zap.Error(err))
		jsonError(w, "failed to update booking", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		payload := map[string]any{
			"booking_id":      booking.ID,
			"confirmation_no": booking.ConfirmationNo,
			"status":          string(booking.Status),
			"applied_fee":     appliedFee,
			"fee_currency":    policy.Currency,
		}
		if isCancel {
			_ = h.publisher.PublishRoomBookingCancelled(r.Context(), tid, payload)
		} else {
			_ = h.publisher.PublishRoomBookingUpdated(r.Context(), tid, payload)
		}
	}

	jsonOK(w, map[string]any{"booking": booking, "applied_fee": appliedFee, "fee_currency": policy.Currency})
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

// inventoryBundleDTO is the slimmed bundle returned to hotel forms for the package picker.
type inventoryBundleDTO struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	SKU         string  `json:"sku"`
	PackageType string  `json:"package_type"`
	Price       float64 `json:"price"`
}

// ListInventoryBundles handles GET /{tenantID}/hotel/inventory-bundles.
// Proxies inventory-api Bundles so the conference/event form can pick a package by name.
func (h *HotelHandler) ListInventoryBundles(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	tenantSlug := httpware.GetTenantSlug(r.Context())
	if tenantSlug == "" {
		if t, lookupErr := h.client.Tenant.Get(r.Context(), tid); lookupErr == nil {
			tenantSlug = t.Slug
		}
	}
	if tenantSlug == "" {
		tenantSlug = tid.String()
	}
	bundles, err := fetchInventoryBundles(r.Context(), tenantSlug)
	if err != nil {
		h.log.Error("list inventory bundles failed", zap.Error(err))
		jsonError(w, "failed to fetch inventory bundles", http.StatusBadGateway)
		return
	}
	out := make([]inventoryBundleDTO, 0, len(bundles))
	for _, b := range bundles {
		out = append(out, inventoryBundleDTO{ID: b.ID, Name: b.Name, SKU: b.SKU, PackageType: b.PackageType, Price: b.Price})
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
