package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entss "github.com/bengobox/pos-service/internal/ent/staffschedule"
)

type StaffScheduleHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewStaffScheduleHandler(log *zap.Logger, db *ent.Client) *StaffScheduleHandler {
	return &StaffScheduleHandler{log: log, db: db}
}

type upsertScheduleInput struct {
	OutletID    string `json:"outlet_id"`
	DayOfWeek   int    `json:"day_of_week"`
	StartTime   string `json:"start_time"`
	EndTime     string `json:"end_time"`
	IsAvailable *bool  `json:"is_available"`
	Notes       string `json:"notes"`
}

// ListSchedule handles GET /{tenantID}/pos/staff/{staffID}/schedule
func (h *StaffScheduleHandler) ListSchedule(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	staffID, err := uuid.Parse(chi.URLParam(r, "staffID"))
	if err != nil {
		jsonError(w, "invalid staff_id", http.StatusBadRequest)
		return
	}

	schedules, err := h.db.StaffSchedule.Query().
		Where(entss.TenantID(tid), entss.StaffMemberID(staffID)).
		Order(ent.Asc(entss.FieldDayOfWeek)).
		All(r.Context())
	if err != nil {
		h.log.Error("list staff schedule failed", zap.Error(err))
		jsonError(w, "failed to list schedule", http.StatusInternalServerError)
		return
	}
	jsonOK(w, schedules)
}

// UpsertSchedule handles PUT /{tenantID}/pos/staff/{staffID}/schedule
func (h *StaffScheduleHandler) UpsertSchedule(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	staffID, err := uuid.Parse(chi.URLParam(r, "staffID"))
	if err != nil {
		jsonError(w, "invalid staff_id", http.StatusBadRequest)
		return
	}

	var inputs []upsertScheduleInput
	if err := json.NewDecoder(r.Body).Decode(&inputs); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	var results []*ent.StaffSchedule
	for _, inp := range inputs {
		if inp.DayOfWeek < 0 || inp.DayOfWeek > 6 {
			continue
		}

		outletID, _ := uuid.Parse(inp.OutletID)

		existing, err := h.db.StaffSchedule.Query().
			Where(entss.TenantID(tid), entss.StaffMemberID(staffID), entss.DayOfWeek(inp.DayOfWeek)).
			Only(r.Context())

		if err == nil {
			// Update
			upd := h.db.StaffSchedule.UpdateOneID(existing.ID)
			if inp.StartTime != "" {
				upd = upd.SetStartTime(inp.StartTime)
			}
			if inp.EndTime != "" {
				upd = upd.SetEndTime(inp.EndTime)
			}
			if inp.IsAvailable != nil {
				upd = upd.SetIsAvailable(*inp.IsAvailable)
			}
			if inp.Notes != "" {
				upd = upd.SetNotes(inp.Notes)
			}
			saved, err := upd.Save(r.Context())
			if err != nil {
				h.log.Warn("failed to update schedule entry", zap.Error(err))
				continue
			}
			results = append(results, saved)
		} else {
			// Create
			isAvailable := true
			if inp.IsAvailable != nil {
				isAvailable = *inp.IsAvailable
			}
			creator := h.db.StaffSchedule.Create().
				SetTenantID(tid).
				SetStaffMemberID(staffID).
				SetDayOfWeek(inp.DayOfWeek).
				SetStartTime(inp.StartTime).
				SetEndTime(inp.EndTime).
				SetIsAvailable(isAvailable)
			if inp.OutletID != "" {
				creator = creator.SetOutletID(outletID)
			}
			if inp.Notes != "" {
				creator = creator.SetNotes(inp.Notes)
			}
			saved, err := creator.Save(r.Context())
			if err != nil {
				h.log.Warn("failed to create schedule entry", zap.Error(err))
				continue
			}
			results = append(results, saved)
		}
	}

	jsonOK(w, results)
}
