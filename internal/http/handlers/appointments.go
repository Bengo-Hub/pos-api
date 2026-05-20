package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entappt "github.com/bengobox/pos-service/internal/ent/appointment"
)

type AppointmentHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewAppointmentHandler(log *zap.Logger, db *ent.Client) *AppointmentHandler {
	return &AppointmentHandler{log: log, db: db}
}

type createAppointmentInput struct {
	OutletID      string    `json:"outlet_id"`
	CustomerID    *string   `json:"customer_id"`
	CustomerName  string    `json:"customer_name"`
	CustomerPhone string    `json:"customer_phone"`
	StaffMemberID *string   `json:"staff_member_id"`
	ServiceItemID string    `json:"service_item_id"`
	ServiceSKU    string    `json:"service_sku"`
	StartTime     time.Time `json:"start_time"`
	EndTime       time.Time `json:"end_time"`
	Notes         string    `json:"notes"`
}

type updateAppointmentInput struct {
	Status        string     `json:"status"`
	StaffMemberID *string    `json:"staff_member_id"`
	StartTime     *time.Time `json:"start_time"`
	EndTime       *time.Time `json:"end_time"`
	Notes         string     `json:"notes"`
	PosOrderID    *string    `json:"pos_order_id"`
}

// List handles GET /{tenantID}/pos/appointments
func (h *AppointmentHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.Appointment.Query().Where(entappt.TenantID(tid))

	if s := r.URL.Query().Get("status"); s != "" {
		q = q.Where(entappt.StatusEQ(entappt.Status(s)))
	}
	if smID := r.URL.Query().Get("staff_member_id"); smID != "" {
		staffID, err := uuid.Parse(smID)
		if err == nil {
			q = q.Where(entappt.StaffMemberIDEQ(staffID))
		}
	}
	if from := r.URL.Query().Get("start_from"); from != "" {
		t, err := time.Parse(time.RFC3339, from)
		if err == nil {
			q = q.Where(entappt.StartTimeGTE(t))
		}
	}
	if to := r.URL.Query().Get("start_to"); to != "" {
		t, err := time.Parse(time.RFC3339, to)
		if err == nil {
			q = q.Where(entappt.StartTimeLTE(t))
		}
	}

	appts, err := q.Order(ent.Asc(entappt.FieldStartTime)).All(r.Context())
	if err != nil {
		h.log.Error("list appointments failed", zap.Error(err))
		jsonError(w, "failed to list appointments", http.StatusInternalServerError)
		return
	}
	jsonOK(w, appts)
}

// Create handles POST /{tenantID}/pos/appointments
func (h *AppointmentHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createAppointmentInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.ServiceSKU == "" || input.ServiceItemID == "" {
		jsonError(w, "service_item_id and service_sku are required", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}
	serviceItemID, err := uuid.Parse(input.ServiceItemID)
	if err != nil {
		jsonError(w, "invalid service_item_id", http.StatusBadRequest)
		return
	}

	creator := h.db.Appointment.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetServiceItemID(serviceItemID).
		SetServiceSku(input.ServiceSKU).
		SetStartTime(input.StartTime).
		SetEndTime(input.EndTime).
		SetCustomerName(input.CustomerName).
		SetCustomerPhone(input.CustomerPhone).
		SetNotes(input.Notes)

	if input.CustomerID != nil {
		cid, err := uuid.Parse(*input.CustomerID)
		if err == nil {
			creator = creator.SetCustomerID(cid)
		}
	}
	if input.StaffMemberID != nil {
		smid, err := uuid.Parse(*input.StaffMemberID)
		if err == nil {
			creator = creator.SetStaffMemberID(smid)
		}
	}

	appt, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create appointment failed", zap.Error(err))
		jsonError(w, "failed to create appointment", http.StatusInternalServerError)
		return
	}
	jsonOK(w, appt)
}

// Get handles GET /{tenantID}/pos/appointments/{id}
func (h *AppointmentHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	apptID, err := uuid.Parse(chi.URLParam(r, "appointmentID"))
	if err != nil {
		jsonError(w, "invalid appointment_id", http.StatusBadRequest)
		return
	}

	appt, err := h.db.Appointment.Get(r.Context(), apptID)
	if err != nil || appt.TenantID != tid {
		jsonError(w, "appointment not found", http.StatusNotFound)
		return
	}
	jsonOK(w, appt)
}

// Update handles PUT /{tenantID}/pos/appointments/{id}
func (h *AppointmentHandler) Update(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	apptID, err := uuid.Parse(chi.URLParam(r, "appointmentID"))
	if err != nil {
		jsonError(w, "invalid appointment_id", http.StatusBadRequest)
		return
	}

	existing, err := h.db.Appointment.Get(r.Context(), apptID)
	if err != nil || existing.TenantID != tid {
		jsonError(w, "appointment not found", http.StatusNotFound)
		return
	}

	var input updateAppointmentInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	upd := h.db.Appointment.UpdateOneID(apptID)
	if input.Status != "" {
		upd = upd.SetStatus(entappt.Status(input.Status))
	}
	if input.Notes != "" {
		upd = upd.SetNotes(input.Notes)
	}
	if input.StartTime != nil {
		upd = upd.SetStartTime(*input.StartTime)
	}
	if input.EndTime != nil {
		upd = upd.SetEndTime(*input.EndTime)
	}
	if input.StaffMemberID != nil {
		smid, err := uuid.Parse(*input.StaffMemberID)
		if err == nil {
			upd = upd.SetStaffMemberID(smid)
		}
	}
	if input.PosOrderID != nil {
		oid, err := uuid.Parse(*input.PosOrderID)
		if err == nil {
			upd = upd.SetPosOrderID(oid)
		}
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update appointment failed", zap.Error(err))
		jsonError(w, "failed to update appointment", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// Availability handles GET /{tenantID}/pos/appointments/availability
// Returns time slots for a given staff member and date that don't overlap with existing appointments.
func (h *AppointmentHandler) Availability(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	dateStr := r.URL.Query().Get("date")
	smIDStr := r.URL.Query().Get("staff_member_id")
	if dateStr == "" || smIDStr == "" {
		jsonError(w, "date and staff_member_id are required", http.StatusBadRequest)
		return
	}

	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		jsonError(w, "invalid date format, use YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	staffID, err := uuid.Parse(smIDStr)
	if err != nil {
		jsonError(w, "invalid staff_member_id", http.StatusBadRequest)
		return
	}

	dayStart := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)

	bookedAppts, err := h.db.Appointment.Query().
		Where(
			entappt.TenantID(tid),
			entappt.StaffMemberIDEQ(staffID),
			entappt.StartTimeGTE(dayStart),
			entappt.StartTimeLTE(dayEnd),
			entappt.StatusNEQ(entappt.StatusCancelled),
		).
		Order(ent.Asc(entappt.FieldStartTime)).
		All(r.Context())
	if err != nil {
		h.log.Error("availability query failed", zap.Error(err))
		jsonError(w, "failed to query availability", http.StatusInternalServerError)
		return
	}

	type bookedSlot struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	}
	booked := make([]bookedSlot, 0, len(bookedAppts))
	for _, a := range bookedAppts {
		booked = append(booked, bookedSlot{Start: a.StartTime, End: a.EndTime})
	}
	jsonOK(w, map[string]any{
		"date":         dateStr,
		"staff_member": smIDStr,
		"booked_slots": booked,
	})
}
