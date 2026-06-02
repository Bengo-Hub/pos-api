package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	enteventbooking "github.com/bengobox/pos-service/internal/ent/eventbooking"
	entmeal "github.com/bengobox/pos-service/internal/ent/mealentitlement"
)

type eventBookingInput struct {
	FacilityID              string    `json:"facility_id"`
	InventoryBundleID       string    `json:"inventory_bundle_id"`
	EventType               string    `json:"event_type"`
	Title                   string    `json:"title"`
	ClientName              string    `json:"client_name"`
	ContactPhone            string    `json:"contact_phone"`
	ContactEmail            string    `json:"contact_email"`
	CRMContactID            string    `json:"crm_contact_id"`
	StartAt                 time.Time `json:"start_at"`
	EndAt                   time.Time `json:"end_at"`
	ConferenceDays          int       `json:"conference_days"`
	DelegateCount           int       `json:"delegate_count"`
	ExpectedPax             int       `json:"expected_pax"`
	GuaranteedMinimumCovers int       `json:"guaranteed_minimum_covers"`
	SetupStyle              string    `json:"setup_style"`
	DepositAmount           float64   `json:"deposit_amount"`
	DepositRefundable       *bool     `json:"deposit_refundable"`
	TotalAmount             float64   `json:"total_amount"`
	Currency                string    `json:"currency"`
	SpecialRequests         string    `json:"special_requests"`
	CreatedBy               string    `json:"created_by"`
}

// CreateEventBooking handles POST /{tenantID}/hotel/events — create a conference/event (BEO).
func (h *HotelHandler) CreateEventBooking(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var in eventBookingInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	facilityID, err := uuid.Parse(in.FacilityID)
	if err != nil {
		jsonError(w, "valid facility_id is required", http.StatusBadRequest)
		return
	}
	if in.Title == "" || in.ClientName == "" {
		jsonError(w, "title and client_name are required", http.StatusBadRequest)
		return
	}
	if in.ConferenceDays < 1 {
		in.ConferenceDays = 1
	}
	if in.Currency == "" {
		in.Currency = "KES"
	}
	outletID, _ := uuid.Parse(httpware.GetOutletID(r.Context()))
	createdBy, _ := uuid.Parse(in.CreatedBy)

	b := h.client.EventBooking.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetFacilityID(facilityID).
		SetTitle(in.Title).
		SetClientName(in.ClientName).
		SetContactPhone(in.ContactPhone).
		SetContactEmail(in.ContactEmail).
		SetStartAt(in.StartAt).
		SetEndAt(in.EndAt).
		SetConferenceDays(in.ConferenceDays).
		SetDelegateCount(in.DelegateCount).
		SetExpectedPax(in.ExpectedPax).
		SetGuaranteedMinimumCovers(in.GuaranteedMinimumCovers).
		SetSetupStyle(in.SetupStyle).
		SetDepositAmount(in.DepositAmount).
		SetTotalAmount(in.TotalAmount).
		SetCurrency(in.Currency).
		SetSpecialRequests(in.SpecialRequests).
		SetCreatedBy(createdBy)
	if in.EventType != "" {
		b = b.SetEventType(enteventbooking.EventType(in.EventType))
	}
	if in.DepositRefundable != nil {
		b = b.SetDepositRefundable(*in.DepositRefundable)
	}
	if bundleID, perr := uuid.Parse(in.InventoryBundleID); perr == nil {
		b = b.SetInventoryBundleID(bundleID)
	}
	if crmID, perr := uuid.Parse(in.CRMContactID); perr == nil {
		b = b.SetCrmContactID(crmID)
	}
	event, err := b.Save(r.Context())
	if err != nil {
		h.log.Error("create event booking failed", zap.Error(err))
		jsonError(w, "failed to create event booking", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishConferenceEventBooked(r.Context(), tid, map[string]any{
			"event_booking_id": event.ID,
			"facility_id":      facilityID,
			"title":            event.Title,
			"delegate_count":   event.DelegateCount,
			"conference_days":  event.ConferenceDays,
			"start_at":         event.StartAt,
		})
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, event)
}

// ListEventBookings handles GET /{tenantID}/hotel/events.
func (h *HotelHandler) ListEventBookings(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.client.EventBooking.Query().Where(enteventbooking.TenantID(tid))
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			q = q.Where(enteventbooking.OutletID(oid))
		}
	}
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(enteventbooking.StatusEQ(enteventbooking.Status(status)))
	}
	events, err := q.Order(ent.Desc(enteventbooking.FieldStartAt)).Limit(200).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, events)
}

// GetEventBooking handles GET /{tenantID}/hotel/events/{id} with its meal entitlements.
func (h *HotelHandler) GetEventBooking(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid event id", http.StatusBadRequest)
		return
	}
	event, err := h.client.EventBooking.Query().
		Where(enteventbooking.ID(id), enteventbooking.TenantID(tid)).
		WithMealEntitlements().
		Only(r.Context())
	if err != nil {
		jsonError(w, "event not found", http.StatusNotFound)
		return
	}
	jsonOK(w, event)
}

// reconcileRow is one issued-vs-redeemed line in the cover reconciliation report.
type reconcileRow struct {
	ConferenceDay string `json:"conference_day"`
	MealPeriod    string `json:"meal_period"`
	Issued        int    `json:"issued"`
	Redeemed      int    `json:"redeemed"`
}

// ReconcileEvent handles GET /{tenantID}/hotel/events/{id}/reconciliation —
// issued vs redeemed meal vouchers grouped by day and meal period.
func (h *HotelHandler) ReconcileEvent(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid event id", http.StatusBadRequest)
		return
	}
	cards, err := h.client.MealEntitlement.Query().
		Where(entmeal.TenantID(tid), entmeal.EventBookingID(id)).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	type key struct {
		day    string
		period string
	}
	agg := map[key]*reconcileRow{}
	for _, c := range cards {
		k := key{day: c.ConferenceDay.Format("2006-01-02"), period: string(c.MealPeriod)}
		row := agg[k]
		if row == nil {
			row = &reconcileRow{ConferenceDay: k.day, MealPeriod: k.period}
			agg[k] = row
		}
		row.Issued++
		if c.Status == entmeal.StatusRedeemed {
			row.Redeemed++
		}
	}
	rows := make([]reconcileRow, 0, len(agg))
	for _, r := range agg {
		rows = append(rows, *r)
	}
	jsonOK(w, map[string]any{"event_booking_id": id, "rows": rows})
}
