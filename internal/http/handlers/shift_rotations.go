package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entrot "github.com/bengobox/pos-service/internal/ent/shiftrotation"
	entslot "github.com/bengobox/pos-service/internal/ent/shiftrotationslot"
)

type ShiftRotationHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewShiftRotationHandler(log *zap.Logger, db *ent.Client) *ShiftRotationHandler {
	return &ShiftRotationHandler{log: log, db: db}
}

type createRotationInput struct {
	Name      string `json:"name"`
	CycleDays int    `json:"cycle_days"` // defaults to 14
	StartDate string `json:"start_date"` // YYYY-MM-DD
}

type rotationSlotInput struct {
	StaffMemberID string `json:"staff_member_id"`
	CycleDay      int    `json:"cycle_day"`
	StartTime     string `json:"start_time"` // HH:MM
	EndTime       string `json:"end_time"`   // HH:MM
	IsOffDay      bool   `json:"is_off_day"`
}

// ListRotations handles GET /{tenantID}/pos/shift-rotations
func (h *ShiftRotationHandler) ListRotations(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	query := h.db.ShiftRotation.Query().
		Where(entrot.TenantID(tid)).
		Order(ent.Desc(entrot.FieldCreatedAt))

	if r.URL.Query().Get("active") == "true" {
		query = query.Where(entrot.IsActive(true))
	}

	rotations, err := query.All(r.Context())
	if err != nil {
		h.log.Error("list rotations failed", zap.Error(err))
		jsonError(w, "failed to list rotations", http.StatusInternalServerError)
		return
	}
	jsonOK(w, rotations)
}

// CreateRotation handles POST /{tenantID}/pos/shift-rotations
func (h *ShiftRotationHandler) CreateRotation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var inp createRotationInput
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if inp.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	start, err := time.Parse("2006-01-02", inp.StartDate)
	if err != nil {
		jsonError(w, "invalid start_date — use YYYY-MM-DD", http.StatusBadRequest)
		return
	}

	cycleDays := inp.CycleDays
	if cycleDays <= 0 {
		cycleDays = 14
	}

	saved, err := h.db.ShiftRotation.Create().
		SetTenantID(tid).
		SetName(inp.Name).
		SetCycleDays(cycleDays).
		SetStartDate(start).
		SetIsActive(true).
		Save(r.Context())
	if err != nil {
		h.log.Error("create rotation failed", zap.Error(err))
		jsonError(w, "failed to create rotation", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusCreated, saved)
}

// GetRotation handles GET /{tenantID}/pos/shift-rotations/{rotationID}
func (h *ShiftRotationHandler) GetRotation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	rotationID, err := uuid.Parse(chi.URLParam(r, "rotationID"))
	if err != nil {
		jsonError(w, "invalid rotation_id", http.StatusBadRequest)
		return
	}

	rotation, err := h.db.ShiftRotation.Get(r.Context(), rotationID)
	if err != nil || rotation.TenantID != tid {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	slots, err := h.db.ShiftRotationSlot.Query().
		Where(entslot.RotationID(rotationID)).
		Order(ent.Asc(entslot.FieldCycleDay)).
		All(r.Context())
	if err != nil {
		h.log.Error("list rotation slots failed", zap.Error(err))
		jsonError(w, "failed to load rotation slots", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"rotation": rotation, "slots": slots})
}

// UpdateRotation handles PATCH /{tenantID}/pos/shift-rotations/{rotationID}
func (h *ShiftRotationHandler) UpdateRotation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	rotationID, err := uuid.Parse(chi.URLParam(r, "rotationID"))
	if err != nil {
		jsonError(w, "invalid rotation_id", http.StatusBadRequest)
		return
	}

	existing, err := h.db.ShiftRotation.Get(r.Context(), rotationID)
	if err != nil || existing.TenantID != tid {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	var inp struct {
		Name     *string `json:"name"`
		IsActive *bool   `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	upd := h.db.ShiftRotation.UpdateOneID(rotationID)
	if inp.Name != nil && *inp.Name != "" {
		upd = upd.SetName(*inp.Name)
	}
	if inp.IsActive != nil {
		upd = upd.SetIsActive(*inp.IsActive)
	}

	saved, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update rotation failed", zap.Error(err))
		jsonError(w, "failed to update rotation", http.StatusInternalServerError)
		return
	}
	jsonOK(w, saved)
}

// UpsertSlots handles PUT /{tenantID}/pos/shift-rotations/{rotationID}/slots
// Replaces all slots for the rotation with the provided set.
func (h *ShiftRotationHandler) UpsertSlots(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	rotationID, err := uuid.Parse(chi.URLParam(r, "rotationID"))
	if err != nil {
		jsonError(w, "invalid rotation_id", http.StatusBadRequest)
		return
	}

	existing, err := h.db.ShiftRotation.Get(r.Context(), rotationID)
	if err != nil || existing.TenantID != tid {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	var inputs []rotationSlotInput
	if err := json.NewDecoder(r.Body).Decode(&inputs); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Delete existing slots and replace with new set.
	if _, err := h.db.ShiftRotationSlot.Delete().Where(entslot.RotationID(rotationID)).Exec(r.Context()); err != nil {
		h.log.Error("delete rotation slots failed", zap.Error(err))
		jsonError(w, "failed to replace slots", http.StatusInternalServerError)
		return
	}

	var created []*ent.ShiftRotationSlot
	for _, inp := range inputs {
		smID, err := uuid.Parse(inp.StaffMemberID)
		if err != nil || inp.CycleDay < 1 {
			continue
		}
		slot, err := h.db.ShiftRotationSlot.Create().
			SetRotationID(rotationID).
			SetTenantID(tid).
			SetStaffMemberID(smID).
			SetCycleDay(inp.CycleDay).
			SetStartTime(inp.StartTime).
			SetEndTime(inp.EndTime).
			SetIsOffDay(inp.IsOffDay).
			Save(r.Context())
		if err != nil {
			h.log.Warn("failed to create rotation slot", zap.Error(err))
			continue
		}
		created = append(created, slot)
	}

	jsonOK(w, created)
}
