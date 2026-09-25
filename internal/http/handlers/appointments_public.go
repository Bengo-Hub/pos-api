package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	entappt "github.com/bengobox/pos-service/internal/ent/appointment"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
)

// appointmentCapacityLookback is how far back the default parallel capacity looks for distinct
// staff members who took appointments at the outlet.
const appointmentCapacityLookback = 60 * 24 * time.Hour

// PublicBookedSlots handles GET /{tenantID}/pos/appointments/booked-slots?outlet_id=&date=YYYY-MM-DD
// (public, no customer data). The online storefront's appointment picker uses it to stop offering
// times the outlet is already fully booked for. It returns every booked interval that day (start,
// end only) and the outlet's parallel capacity: how many bookings can run at the same time. The
// capacity is the outlet setting metadata "appointment_capacity" when set, otherwise the number of
// distinct staff members who took appointments there recently (at least 1), so a three-chair salon
// keeps taking bookings at 10:00 after the first one lands.
func (h *AppointmentHandler) PublicBookedSlots(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		// The storefront addresses the tenant by slug; resolve it when the tenant middleware only
		// carried the slug.
		if t, terr := h.db.Tenant.Query().Where(enttenant.Slug(chi.URLParam(r, "tenantID"))).Only(r.Context()); terr == nil {
			tid, err = t.ID, nil
		}
	}
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(r.URL.Query().Get("outlet_id"))
	if err != nil {
		jsonError(w, "outlet_id is required", http.StatusBadRequest)
		return
	}
	day, err := time.Parse("2006-01-02", r.URL.Query().Get("date"))
	if err != nil {
		jsonError(w, "date is required (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	// A generous window around the calendar day absorbs any timezone offset between the
	// storefront and the outlet; the picker filters by its own local day.
	from := day.Add(-24 * time.Hour)
	to := day.Add(48 * time.Hour)

	appts, err := h.db.Appointment.Query().
		Where(
			entappt.TenantID(tid),
			entappt.OutletID(outletID),
			entappt.StatusNotIn(entappt.StatusCancelled, entappt.StatusNoShow),
			entappt.StartTimeLT(to),
			entappt.EndTimeGT(from),
		).
		Order(entappt.ByStartTime()).
		All(r.Context())
	if err != nil {
		h.log.Error("public booked slots query failed", zap.Error(err))
		jsonError(w, "failed to load availability", http.StatusInternalServerError)
		return
	}

	type slot struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	}
	booked := make([]slot, 0, len(appts))
	for _, a := range appts {
		booked = append(booked, slot{Start: a.StartTime, End: a.EndTime})
	}
	jsonOK(w, map[string]any{
		"date":         day.Format("2006-01-02"),
		"outlet_id":    outletID.String(),
		"capacity":     h.appointmentCapacity(r.Context(), tid, outletID),
		"booked_slots": booked,
	})
}

// appointmentCapacity resolves how many appointments the outlet can run in parallel.
func (h *AppointmentHandler) appointmentCapacity(ctx context.Context, tid, outletID uuid.UUID) int {
	if setting, err := h.db.OutletSetting.Query().Where(entoutletsetting.OutletID(outletID)).Only(ctx); err == nil && setting.Metadata != nil {
		switch v := setting.Metadata["appointment_capacity"].(type) {
		case float64:
			if v >= 1 {
				return int(v)
			}
		case int:
			if v >= 1 {
				return v
			}
		}
	}
	var staff []uuid.UUID
	if err := h.db.Appointment.Query().
		Where(
			entappt.TenantID(tid),
			entappt.OutletID(outletID),
			entappt.StaffMemberIDNotNil(),
			entappt.StartTimeGT(time.Now().Add(-appointmentCapacityLookback)),
		).
		Unique(true).
		Select(entappt.FieldStaffMemberID).
		Scan(ctx, &staff); err == nil && len(staff) > 1 {
		return len(staff)
	}
	return 1
}
