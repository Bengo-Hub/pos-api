package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entfacility "github.com/bengobox/pos-service/internal/ent/facility"
	entfacilitybooking "github.com/bengobox/pos-service/internal/ent/facilitybooking"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	treasury "github.com/bengobox/pos-service/internal/modules/treasury"
	"github.com/bengobox/pos-service/internal/platform/events"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// HotelHandler handles hotel management endpoints (rooms, guests, folio, facilities, bookings).
type HotelHandler struct {
	log             *zap.Logger
	client          *ent.Client
	publisher       *events.Publisher
	treasuryClient  *treasury.Client
	inventoryClient *inventory.Client
	subsClient      *subscriptions.Client
}

func NewHotelHandler(log *zap.Logger, client *ent.Client, publisher *events.Publisher) *HotelHandler {
	return &HotelHandler{log: log, client: client, publisher: publisher}
}

func (h *HotelHandler) SetTreasuryClient(c *treasury.Client) {
	h.treasuryClient = c
}

// SetSubscriptionsClient injects the subscriptions S2S client used to enforce
// plan limits (max_rooms, max_conference_events) before creating billable resources.
func (h *HotelHandler) SetSubscriptionsClient(c *subscriptions.Client) {
	h.subsClient = c
}

// writeUsageLimitExceeded responds 403 with the structured code the frontend
// error-handler recognises (usage_limit_exceeded → toast + upgrade prompt).
func writeUsageLimitExceeded(w http.ResponseWriter, message string, limit int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   "usage_limit_exceeded",
		"message": message,
		"limit":   limit,
	})
}

// SetInventoryClient injects the inventory S2S client so direct folio charges
// (minibar/room-service consumables) can backflush stock.
func (h *HotelHandler) SetInventoryClient(c *inventory.Client) {
	h.inventoryClient = c
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
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			q = q.Where(entroom.OutletID(oid))
		}
	}

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
	OutletID        string  `json:"outlet_id"`
	RoomNumber      string  `json:"room_number"`
	Name            string  `json:"name"`
	RoomType        string  `json:"room_type"`
	Floor           int     `json:"floor"`
	RatePerNight    float64 `json:"rate_per_night"`
	Currency        string  `json:"currency"`
	InventoryItemID string  `json:"inventory_item_id"`
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

	// Enforce the max_rooms plan limit. pos-api owns Room records, so we count
	// authoritatively here and compare against the plan limit. Fails open when
	// the limit is unset/unlimited or subscriptions-api is unreachable.
	if h.subsClient != nil {
		if limit, ok := h.subsClient.GetLimit(r.Context(), tid.String(), "max_rooms"); ok {
			count, cerr := h.client.Room.Query().Where(entroom.TenantID(tid)).Count(r.Context())
			if cerr == nil && count >= limit {
				writeUsageLimitExceeded(w, fmt.Sprintf("Your plan allows up to %d rooms. Upgrade your subscription to add more.", limit), limit)
				return
			}
		}
	}

	if input.Currency == "" {
		input.Currency = "KES"
	}
	roomType := entroom.RoomType(input.RoomType)
	if input.RoomType == "" {
		roomType = entroom.RoomTypeStandard
	}

	roomOutletID := parseOptionalUUID(input.OutletID, r)

	roomCreate := h.client.Room.Create().
		SetTenantID(tid).
		SetOutletID(roomOutletID).
		SetRoomNumber(input.RoomNumber).
		SetName(input.Name).
		SetRoomType(roomType).
		SetFloor(input.Floor).
		SetRatePerNight(input.RatePerNight).
		SetCurrency(input.Currency)
	if invID, perr := uuid.Parse(input.InventoryItemID); perr == nil {
		roomCreate = roomCreate.SetInventoryItemID(invID)
	}
	room, err := roomCreate.Save(r.Context())
	if err != nil {
		h.log.Error("create room failed", zap.Error(err))
		jsonError(w, "failed to create room", http.StatusInternalServerError)
		return
	}

	// Track usage for the max_rooms plan limit (subscriptions-api consumes pos.room.created).
	if h.publisher != nil {
		_ = h.publisher.PublishRoomCreated(r.Context(), tid, map[string]any{
			"room_id":     room.ID,
			"room_number": room.RoomNumber,
			"room_type":   string(room.RoomType),
		})
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
	GuestName           string     `json:"guest_name"`
	FirstName           string     `json:"first_name"`
	LastName            string     `json:"last_name"`
	Email               string     `json:"email"`
	Phone               string     `json:"phone"`
	Nationality         string     `json:"nationality"`
	IDType              string     `json:"id_type"`
	IDNumber            string     `json:"id_number"`
	IDDocumentURL       string     `json:"id_document_url"`
	Adults              int        `json:"adults"`
	Children            int        `json:"children"`
	ChildAges           []int      `json:"child_ages"`
	Nights              int        `json:"nights"`
	ExpectedArrivalAt   *time.Time `json:"expected_arrival_at"`
	ExpectedDepartureAt *time.Time `json:"expected_departure_at"`
	Source              string     `json:"source"`
	BookingID           string     `json:"booking_id"`
	CRMContactID        string     `json:"crm_contact_id"`
	CheckedBy           string     `json:"checked_in_by"`
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
	// Guest ID is required (RoomGuest.id_number is NotEmpty at the schema level). Validate
	// here so a missing/blank ID returns a clear 400 instead of leaking the ent validator
	// failure as a 500.
	if strings.TrimSpace(input.IDNumber) == "" {
		jsonError(w, "id_number is required for check-in", http.StatusBadRequest)
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
	nightlyRate := room.RatePerNight
	// Resolve the authoritative nightly rate from inventory-api when the room is linked to a
	// room-type SERVICE item; fall back to the local (synced) rate snapshot otherwise.
	if h.inventoryClient != nil && room.InventoryItemID != nil {
		if price, ok, perr := h.inventoryClient.GetItemPrice(r.Context(), tid.String(), room.InventoryItemID.String(), 1); perr == nil && ok && price.UnitPrice > 0 {
			nightlyRate = price.UnitPrice
		} else if perr != nil {
			h.log.Warn("check-in: inventory price lookup failed, using local rate", zap.Error(perr))
		}
	}
	totalCharge := nightlyRate * float64(input.Nights)
	checkedInBy, _ := uuid.Parse(input.CheckedBy)

	tx, err := h.client.Tx(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if input.Adults < 1 {
		input.Adults = 1
	}
	guestBuilder := tx.RoomGuest.Create().
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
		SetAdults(input.Adults).
		SetChildren(input.Children)
	if input.FirstName != "" {
		guestBuilder = guestBuilder.SetFirstName(input.FirstName)
	}
	if input.LastName != "" {
		guestBuilder = guestBuilder.SetLastName(input.LastName)
	}
	if input.Email != "" {
		guestBuilder = guestBuilder.SetEmail(input.Email)
	}
	if input.Nationality != "" {
		guestBuilder = guestBuilder.SetNationality(input.Nationality)
	}
	if input.IDType != "" {
		guestBuilder = guestBuilder.SetIDType(entroomguest.IDType(input.IDType))
	}
	if input.IDDocumentURL != "" {
		guestBuilder = guestBuilder.SetIDDocumentURL(input.IDDocumentURL)
	}
	if len(input.ChildAges) > 0 {
		guestBuilder = guestBuilder.SetChildAges(input.ChildAges)
	}
	if input.Source != "" {
		guestBuilder = guestBuilder.SetSource(entroomguest.Source(input.Source))
	}
	if input.ExpectedArrivalAt != nil {
		guestBuilder = guestBuilder.SetExpectedArrivalAt(*input.ExpectedArrivalAt)
	}
	if input.ExpectedDepartureAt != nil {
		guestBuilder = guestBuilder.SetExpectedDepartureAt(*input.ExpectedDepartureAt)
	} else {
		guestBuilder = guestBuilder.SetExpectedDepartureAt(checkOutDate)
	}
	if bookingID, perr := uuid.Parse(input.BookingID); perr == nil {
		guestBuilder = guestBuilder.SetBookingID(bookingID)
	}
	if crmID, perr := uuid.Parse(input.CRMContactID); perr == nil {
		guestBuilder = guestBuilder.SetCrmContactID(crmID)
	}
	guest, err := guestBuilder.Save(r.Context())
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

	// GATED CHECKOUT: the bill (room nights + every folio extra, minus payments already taken) must
	// be fully cleared before a guest can be checked out. Folio extras are therefore always settled
	// at checkout even when the room itself was paid in full at check-in. The receptionist clears the
	// balance via POST /rooms/{id}/settle (which can auto-checkout) before this endpoint succeeds.
	summary, serr := h.loadFolioSummary(r, tid, roomID)
	if serr != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	totalFolio := 0.0
	if summary != nil {
		totalFolio = summary.ChargesTotal
		if summary.Balance > 0.009 {
			respondJSON(w, http.StatusConflict, map[string]any{
				"error":       "outstanding_balance",
				"message":     "Settle the outstanding bill before checking the guest out.",
				"balance":     summary.Balance,
				"charges_total": summary.ChargesTotal,
				"paid_total":  summary.PaidTotal,
				"currency":    summary.Currency,
			})
			return
		}
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

	// Balance is already cleared (gated above), so no payment intent is created here — settlement
	// happens via POST /rooms/{id}/settle before checkout.
	jsonOK(w, map[string]any{
		"guest":       guest,
		"total_folio": totalFolio,
		"status":      "checked_out",
	})
}

type postFolioInput struct {
	Description       string     `json:"description"`
	Amount            float64    `json:"amount"`
	ChargeType        string     `json:"charge_type"`
	POSOrderID        *uuid.UUID `json:"pos_order_id,omitempty"`
	InventorySku      string     `json:"inventory_sku,omitempty"`
	InventoryBundleID *uuid.UUID `json:"inventory_bundle_id,omitempty"`
	Quantity          float64    `json:"quantity,omitempty"`
	CreatedBy         string     `json:"created_by"`
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

	// Coerce the charge_type to a known enum value. The UI posts free-form types such as
	// "restaurant" (a POS bill charged to the room) that are NOT in the enum — binding them
	// straight through made Ent's enum validation fail and returned a 500 "failed to post
	// charge". Unknown types now fall back to "other" so a charge is never silently rejected.
	chargeType := normalizeFolioChargeType(input.ChargeType)
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
	if input.InventorySku != "" {
		c = c.SetInventorySku(input.InventorySku)
	}
	if input.InventoryBundleID != nil {
		c = c.SetInventoryBundleID(*input.InventoryBundleID)
	}

	item, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("post folio charge failed", zap.Error(err))
		jsonError(w, "failed to post charge", http.StatusInternalServerError)
		return
	}

	// Backflush stock for a direct consumable folio charge (minibar/room-service/food)
	// that did NOT originate from a POS order — order-sourced charges already backflush
	// via pos.sale.finalized, so we skip those to avoid double deduction.
	if h.inventoryClient != nil && input.InventorySku != "" && input.POSOrderID == nil {
		qty := input.Quantity
		if qty <= 0 {
			qty = 1
		}
		// WithoutCancel preserves the request's tenant context for the async S2S call (the inventory
		// client resolves the tenant from ctx) while surviving the request's completion; a bare
		// context.Background() dropped the tenant → inventory "no default warehouse for tenant".
		bgCtx := context.WithoutCancel(r.Context())
		go func(sku string, q float64) {
			ctx, cancel := context.WithTimeout(bgCtx, 10*time.Second)
			defer cancel()
			if _, cErr := h.inventoryClient.RecordConsumption(ctx, tid.String(), inventory.ConsumptionRequest{
				OrderID: item.ID.String(),
				Items:   []inventory.ConsumptionItem{{SKU: sku, Quantity: q}},
			}); cErr != nil {
				h.log.Warn("folio charge backflush failed", zap.String("sku", sku), zap.Error(cErr))
				if h.publisher != nil {
					_ = h.publisher.PublishInventoryConsumptionFailed(context.Background(), tid, map[string]any{
						"folio_item_id": item.ID.String(),
						"tenant_id":     tid.String(),
						"sku":           sku,
						"quantity":      q,
						"error":         cErr.Error(),
					})
				}
			}
		}(input.InventorySku, qty)
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
	OutletID        string  `json:"outlet_id"`
	Name            string  `json:"name"`
	FacilityType    string  `json:"facility_type"`
	Capacity        int     `json:"capacity"`
	RatePerSession  float64 `json:"rate_per_session"`
	Currency        string  `json:"currency"`
	OpeningTime     string  `json:"opening_time"`
	ClosingTime     string  `json:"closing_time"`
	BookingMode     string  `json:"booking_mode"`      // exclusive | shared (co-working)
	InventoryItemID string  `json:"inventory_item_id"` // link to inventory SERVICE item (rate/package master)
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

	create := h.client.Facility.Create().
		SetTenantID(tid).
		SetOutletID(facilityOutletID).
		SetName(input.Name).
		SetFacilityType(facilityType).
		SetCapacity(input.Capacity).
		SetRatePerSession(input.RatePerSession).
		SetCurrency(input.Currency).
		SetOpeningTime(input.OpeningTime).
		SetClosingTime(input.ClosingTime)
	if input.BookingMode != "" {
		create = create.SetBookingMode(entfacility.BookingMode(input.BookingMode))
	}
	if iid, perr := uuid.Parse(strings.TrimSpace(input.InventoryItemID)); perr == nil {
		create = create.SetInventoryItemID(iid)
	}
	facility, err := create.Save(r.Context())
	if err != nil {
		h.log.Error("create facility failed", zap.Error(err))
		jsonError(w, "failed to create facility", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, facility)
}

type updateFacilityInput struct {
	Name            *string  `json:"name"`
	FacilityType    *string  `json:"facility_type"`
	Capacity        *int     `json:"capacity"`
	RatePerSession  *float64 `json:"rate_per_session"`
	Currency        *string  `json:"currency"`
	OpeningTime     *string  `json:"opening_time"`
	ClosingTime     *string  `json:"closing_time"`
	Status          *string  `json:"status"`
	BookingMode     *string  `json:"booking_mode"`
	InventoryItemID *string  `json:"inventory_item_id"`
}

// UpdateFacility handles PATCH /{tenantID}/hotel/facilities/{id}
func (h *HotelHandler) UpdateFacility(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid facility id", http.StatusBadRequest)
		return
	}
	var input updateFacilityInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	upd := h.client.Facility.Update().
		Where(entfacility.ID(id), entfacility.TenantID(tid))
	if input.Name != nil {
		upd = upd.SetName(*input.Name)
	}
	if input.FacilityType != nil && *input.FacilityType != "" {
		upd = upd.SetFacilityType(entfacility.FacilityType(*input.FacilityType))
	}
	if input.Capacity != nil {
		upd = upd.SetCapacity(*input.Capacity)
	}
	if input.RatePerSession != nil {
		upd = upd.SetRatePerSession(*input.RatePerSession)
	}
	if input.Currency != nil && *input.Currency != "" {
		upd = upd.SetCurrency(*input.Currency)
	}
	if input.OpeningTime != nil {
		upd = upd.SetOpeningTime(*input.OpeningTime)
	}
	if input.ClosingTime != nil {
		upd = upd.SetClosingTime(*input.ClosingTime)
	}
	if input.Status != nil && *input.Status != "" {
		upd = upd.SetStatus(entfacility.Status(*input.Status))
	}
	if input.BookingMode != nil && *input.BookingMode != "" {
		upd = upd.SetBookingMode(entfacility.BookingMode(*input.BookingMode))
	}
	if input.InventoryItemID != nil {
		if iid, perr := uuid.Parse(strings.TrimSpace(*input.InventoryItemID)); perr == nil {
			upd = upd.SetInventoryItemID(iid)
		} else if strings.TrimSpace(*input.InventoryItemID) == "" {
			upd = upd.ClearInventoryItemID()
		}
	}
	if _, err := upd.Save(r.Context()); err != nil {
		h.log.Error("update facility failed", zap.Error(err))
		jsonError(w, "failed to update facility", http.StatusInternalServerError)
		return
	}
	facility, err := h.client.Facility.Query().
		Where(entfacility.ID(id), entfacility.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "facility not found", http.StatusNotFound)
		return
	}
	jsonOK(w, facility)
}

// DeleteFacility handles DELETE /{tenantID}/hotel/facilities/{id}
func (h *HotelHandler) DeleteFacility(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid facility id", http.StatusBadRequest)
		return
	}
	n, err := h.client.Facility.Delete().
		Where(entfacility.ID(id), entfacility.TenantID(tid)).Exec(r.Context())
	if err != nil {
		h.log.Error("delete facility failed", zap.Error(err))
		jsonError(w, "failed to delete facility", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		jsonError(w, "facility not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type bookFacilityInput struct {
	GuestName string `json:"guest_name"`
	Phone     string `json:"phone"`
	// SessionDate is a calendar date sent as "2006-01-02" (the UI's <input type="date">).
	// It is a string (not time.Time) because a bare date is NOT valid RFC3339 and would fail
	// JSON binding into a time.Time — the cause of the "invalid request body" 400 on booking.
	SessionDate string     `json:"session_date"`
	StartTime   string     `json:"start_time"`
	EndTime     string     `json:"end_time"`
	GuestsCount int        `json:"guests_count"`
	Amount      float64    `json:"amount"`
	RoomGuestID *uuid.UUID `json:"room_guest_id,omitempty"`
	BookedBy    string     `json:"booked_by"`
	Notes       string     `json:"notes"`
	// Seats consumed from a shared (co-working) facility's capacity. Defaults to guests_count
	// when omitted so existing callers keep working.
	Seats int `json:"seats"`
	// OutletID / POSOrderID link the booking to the POS sale that charged it (co-working sold
	// at the till). Optional — a front-desk reservation can be created uncharged.
	OutletID   string `json:"outlet_id"`
	POSOrderID string `json:"pos_order_id"`
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

	// Parse the calendar date (accept a bare "2006-01-02" or a full RFC3339 timestamp).
	sessionDate, derr := parseFlexibleDate(input.SessionDate)
	if derr != nil {
		jsonError(w, "session_date is required (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}

	// start_time/end_time are optional, but if one is supplied the other must be too
	// (and both must be HH:MM) so double-booking detection has a well-formed window.
	input.StartTime = strings.TrimSpace(input.StartTime)
	input.EndTime = strings.TrimSpace(input.EndTime)
	if (input.StartTime == "") != (input.EndTime == "") {
		jsonError(w, "start_time and end_time must be provided together (HH:MM)", http.StatusBadRequest)
		return
	}
	for _, hm := range []string{input.StartTime, input.EndTime} {
		if hm == "" {
			continue
		}
		if _, perr := time.Parse("15:04", hm); perr != nil {
			jsonError(w, "start_time and end_time must be in HH:MM format", http.StatusBadRequest)
			return
		}
	}

	facilityBookedBy, _ := uuid.Parse(input.BookedBy)

	// Load the facility once for both availability gating and pricing.
	fac, fErr := h.client.Facility.Query().
		Where(entfacility.ID(facilityID), entfacility.TenantID(tid)).Only(r.Context())
	if fErr != nil {
		jsonError(w, "facility not found", http.StatusNotFound)
		return
	}

	// Maintenance/closed facilities are never bookable, in either mode.
	if fac.Status == entfacility.StatusMaintenance || fac.Status == entfacility.StatusClosed {
		jsonError(w, fmt.Sprintf("facility is not available for booking (status: %s)", fac.Status), http.StatusConflict)
		return
	}

	// seats this booking consumes — defaults to guests_count (then 1) so existing callers keep working.
	seats := input.Seats
	if seats < 1 {
		seats = input.GuestsCount
	}
	if seats < 1 {
		seats = 1
	}

	isShared := fac.BookingMode == entfacility.BookingModeShared

	if !isShared {
		// ── Exclusive mode (private meeting room / conference hall) ───────────────────────
		// The booking holds the whole space; capacity only caps head-count, and ANY overlapping
		// confirmed booking is rejected.
		if fac.Status != entfacility.StatusAvailable {
			jsonError(w, fmt.Sprintf("facility is not available for booking (status: %s)", fac.Status), http.StatusConflict)
			return
		}
		if fac.Capacity > 0 && input.GuestsCount > fac.Capacity {
			jsonError(w, fmt.Sprintf("guests (%d) exceed the facility capacity (%d)", input.GuestsCount, fac.Capacity), http.StatusConflict)
			return
		}
		if input.StartTime != "" && input.EndTime != "" {
			for _, b := range h.sameDayConfirmedBookings(r.Context(), tid, facilityID, sessionDate) {
				// HH:MM strings compare lexicographically within a day; overlap = startA < endB && endA > startB.
				if b.StartTime != "" && b.EndTime != "" && input.StartTime < b.EndTime && input.EndTime > b.StartTime {
					jsonError(w, fmt.Sprintf("facility already booked for an overlapping time (%s–%s)", b.StartTime, b.EndTime), http.StatusConflict)
					return
				}
			}
		}
	} else {
		// ── Shared mode (co-working) ──────────────────────────────────────────────────────
		// Many independent bookings may overlap in time until the SUM of their seats reaches
		// capacity. A single booking also can't exceed total capacity.
		if fac.Capacity > 0 && seats > fac.Capacity {
			jsonError(w, fmt.Sprintf("requested seats (%d) exceed the space capacity (%d)", seats, fac.Capacity), http.StatusConflict)
			return
		}
		if fac.Capacity > 0 {
			booked := overlappingSeats(h.sameDayConfirmedBookings(r.Context(), tid, facilityID, sessionDate), input.StartTime, input.EndTime)
			if booked+seats > fac.Capacity {
				jsonError(w, fmt.Sprintf("only %d of %d seats free for that time — cannot book %d", fac.Capacity-booked, fac.Capacity, seats), http.StatusConflict)
				return
			}
		}
	}

	// Resolve the session amount when the caller didn't supply one: prefer the authoritative
	// inventory price (when the facility is linked to a SERVICE item), else the local rate.
	amount := input.Amount
	if amount <= 0 {
		amount = fac.RatePerSession
		if h.inventoryClient != nil && fac.InventoryItemID != nil {
			if price, ok, perr := h.inventoryClient.GetItemPrice(r.Context(), tid.String(), fac.InventoryItemID.String(), 1); perr == nil && ok && price.UnitPrice > 0 {
				amount = price.UnitPrice
			}
		}
	}

	c := h.client.FacilityBooking.Create().
		SetTenantID(tid).
		SetFacilityID(facilityID).
		SetGuestName(input.GuestName).
		SetPhone(input.Phone).
		SetSessionDate(sessionDate).
		SetStartTime(input.StartTime).
		SetEndTime(input.EndTime).
		SetGuestsCount(input.GuestsCount).
		SetSeats(seats).
		SetAmount(amount).
		SetBookedBy(facilityBookedBy)

	if input.RoomGuestID != nil {
		c = c.SetRoomGuestID(*input.RoomGuestID)
	}
	if input.Notes != "" {
		c = c.SetNotes(input.Notes)
	}
	if oid := parseOptionalUUID(input.OutletID, r); oid != uuid.Nil {
		c = c.SetOutletID(oid)
	}
	if poid, perr := uuid.Parse(strings.TrimSpace(input.POSOrderID)); perr == nil {
		c = c.SetPosOrderID(poid)
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

// sameDayConfirmedBookings returns every confirmed booking for a facility on a given
// calendar date — the working set for both exclusive overlap checks and shared seat counting.
func (h *HotelHandler) sameDayConfirmedBookings(ctx context.Context, tid, facilityID uuid.UUID, sessionDate time.Time) []*ent.FacilityBooking {
	return sameDayConfirmedFacilityBookings(ctx, h.client, tid, facilityID, sessionDate)
}

// sameDayConfirmedFacilityBookings is the free-function form so non-hotel callers (the order
// handler's auto-assign-on-sale hook) can reuse the same seat-counting query without a
// *HotelHandler dependency.
func sameDayConfirmedFacilityBookings(ctx context.Context, client *ent.Client, tid, facilityID uuid.UUID, sessionDate time.Time) []*ent.FacilityBooking {
	dayStart := time.Date(sessionDate.Year(), sessionDate.Month(), sessionDate.Day(), 0, 0, 0, 0, sessionDate.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)
	rows, _ := client.FacilityBooking.Query().
		Where(
			entfacilitybooking.FacilityID(facilityID),
			entfacilitybooking.TenantID(tid),
			entfacilitybooking.StatusEQ(entfacilitybooking.StatusConfirmed),
			entfacilitybooking.SessionDateGTE(dayStart),
			entfacilitybooking.SessionDateLT(dayEnd),
		).All(ctx)
	return rows
}

// bookingsTimeOverlap reports whether [startA,endA) and [startB,endB) overlap. A booking
// with no time window is treated as spanning the whole day (overlaps everything) so seat
// counting never under-counts an all-day co-working pass.
func bookingsTimeOverlap(startA, endA, startB, endB string) bool {
	if startA == "" || endA == "" || startB == "" || endB == "" {
		return true
	}
	// HH:MM strings compare lexicographically within a day.
	return startA < endB && endA > startB
}

// overlappingSeats sums the seats of confirmed bookings that overlap the given time window —
// the seats already taken for a shared (co-working) facility at that time.
func overlappingSeats(bookings []*ent.FacilityBooking, startTime, endTime string) int {
	total := 0
	for _, b := range bookings {
		if bookingsTimeOverlap(startTime, endTime, b.StartTime, b.EndTime) {
			s := b.Seats
			if s < 1 {
				s = b.GuestsCount
			}
			if s < 1 {
				s = 1
			}
			total += s
		}
	}
	return total
}

// GetFacilityAvailability handles GET /{tenantID}/hotel/facilities/{id}/availability?date=&start=&end=
// It reports how many seats are free for a shared (co-working) space at a given date/time, so the
// terminal can gate an "Assign Space" action before charging. For exclusive facilities it reports
// whether the window is free (available_seats = capacity or 0).
func (h *HotelHandler) GetFacilityAvailability(w http.ResponseWriter, r *http.Request) {
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
	fac, fErr := h.client.Facility.Query().
		Where(entfacility.ID(facilityID), entfacility.TenantID(tid)).Only(r.Context())
	if fErr != nil {
		jsonError(w, "facility not found", http.StatusNotFound)
		return
	}

	sessionDate, derr := parseFlexibleDate(r.URL.Query().Get("date"))
	if derr != nil {
		jsonError(w, "date is required (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	start := strings.TrimSpace(r.URL.Query().Get("start"))
	end := strings.TrimSpace(r.URL.Query().Get("end"))

	sameDay := h.sameDayConfirmedBookings(r.Context(), tid, facilityID, sessionDate)
	isShared := fac.BookingMode == entfacility.BookingModeShared

	booked := overlappingSeats(sameDay, start, end)
	available := fac.Capacity - booked
	if fac.Capacity <= 0 {
		available = 0 // 0 capacity = unmetered; caller should treat as unlimited/not-tracked
	}
	if available < 0 {
		available = 0
	}

	// Exclusive spaces are all-or-nothing: free only when no overlapping booking exists.
	if !isShared {
		free := true
		if start != "" && end != "" {
			for _, b := range sameDay {
				if b.StartTime != "" && b.EndTime != "" && start < b.EndTime && end > b.StartTime {
					free = false
					break
				}
			}
		} else {
			free = len(sameDay) == 0
		}
		if free {
			available = fac.Capacity
		} else {
			available = 0
		}
	}

	jsonOK(w, map[string]any{
		"facility_id":     facilityID,
		"booking_mode":    string(fac.BookingMode),
		"capacity":        fac.Capacity,
		"booked_seats":    booked,
		"available_seats": available,
		"is_bookable":     fac.Status != entfacility.StatusMaintenance && fac.Status != entfacility.StatusClosed && (isShared || available > 0),
		"date":            sessionDate.Format("2006-01-02"),
		"start_time":      start,
		"end_time":        end,
	})
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
		"guest_id":               guest.ID,
		"late_checkout_approved": true,
		"surcharge_amount":       input.SurchargeAmount,
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

// parseFlexibleDate accepts a bare calendar date ("2006-01-02", what an <input type="date">
// posts) or a full RFC3339 timestamp, returning the parsed time. A bare date is NOT valid
// RFC3339, so binding it into a time.Time field fails — callers parse the raw string instead.
func parseFlexibleDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q", s)
}

// validFolioChargeTypes is the set of charge_type enum values accepted by the RoomFolioItem
// schema. Keep in sync with internal/ent/schema/roomfolioitem.go.
var validFolioChargeTypes = map[string]struct{}{
	"room_charge": {}, "food": {}, "laundry": {}, "minibar": {}, "room_service": {},
	"amenity": {}, "facility": {}, "late_checkout": {}, "damage": {}, "package": {},
	"conference": {}, "meal_voucher": {}, "restaurant": {}, "other": {},
}

// normalizeFolioChargeType maps a caller-supplied charge_type onto a valid enum value,
// defaulting unknown/empty values to "other" so an unexpected type never 500s the charge.
func normalizeFolioChargeType(raw string) entroomfolioitem.ChargeType {
	v := strings.ToLower(strings.TrimSpace(raw))
	if _, ok := validFolioChargeTypes[v]; ok {
		return entroomfolioitem.ChargeType(v)
	}
	return entroomfolioitem.ChargeTypeOther
}
