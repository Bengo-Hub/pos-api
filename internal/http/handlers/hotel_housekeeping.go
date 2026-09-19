package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	enthousekeeping "github.com/bengobox/pos-service/internal/ent/housekeepingtask"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
)

// ListHousekeepingTasks handles GET /{tenantID}/hotel/housekeeping
func (h *HotelHandler) ListHousekeepingTasks(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.HousekeepingTask.Query().Where(enthousekeeping.TenantID(tid))

	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(enthousekeeping.StatusEQ(enthousekeeping.Status(status)))
	}
	if roomIDStr := r.URL.Query().Get("room_id"); roomIDStr != "" {
		if rid, err := uuid.Parse(roomIDStr); err == nil {
			q = q.Where(enthousekeeping.RoomID(rid))
		}
	}
	if staffIDStr := r.URL.Query().Get("assigned_to"); staffIDStr != "" {
		if sid, err := uuid.Parse(staffIDStr); err == nil {
			q = q.Where(enthousekeeping.AssignedTo(sid))
		}
	}

	tasks, err := q.Order(enthousekeeping.ByCreatedAt()).All(r.Context())
	if err != nil {
		h.log.Error("list housekeeping tasks failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": tasks, "total": len(tasks)})
}

type createHousekeepingTaskInput struct {
	RoomID     string `json:"room_id"`
	TaskType   string `json:"task_type"`
	Priority   string `json:"priority"`
	AssignedTo string `json:"assigned_to"`
	Notes      string `json:"notes"`
	DueAt      string `json:"due_at"` // RFC3339
}

// CreateHousekeepingTask handles POST /{tenantID}/hotel/housekeeping
func (h *HotelHandler) CreateHousekeepingTask(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createHousekeepingTaskInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	roomID, err := uuid.Parse(input.RoomID)
	if err != nil {
		jsonError(w, "room_id is required", http.StatusBadRequest)
		return
	}

	taskType := enthousekeeping.TaskTypeRoutineClean
	if input.TaskType != "" {
		taskType = enthousekeeping.TaskType(input.TaskType)
	}
	priority := enthousekeeping.PriorityNormal
	if input.Priority == "urgent" {
		priority = enthousekeeping.PriorityUrgent
	}

	c := h.client.HousekeepingTask.Create().
		SetTenantID(tid).
		SetRoomID(roomID).
		SetTaskType(taskType).
		SetPriority(priority)

	if input.Notes != "" {
		c = c.SetNotes(input.Notes)
	}
	if staffID, err := uuid.Parse(input.AssignedTo); err == nil {
		c = c.SetAssignedTo(staffID)
	}
	if input.DueAt != "" {
		if t, err := time.Parse(time.RFC3339, input.DueAt); err == nil {
			c = c.SetDueAt(t)
		}
	}

	task, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create housekeeping task failed", zap.Error(err))
		jsonError(w, "failed to create task", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, task)
}

// UpdateHousekeepingTask handles PATCH /{tenantID}/hotel/housekeeping/{taskID}
// Used to update status (in_progress, completed, cancelled), reassign staff, add notes.
func (h *HotelHandler) UpdateHousekeepingTask(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	taskID, err := uuid.Parse(chi.URLParam(r, "taskID"))
	if err != nil {
		jsonError(w, "invalid task id", http.StatusBadRequest)
		return
	}

	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	task, err := h.client.HousekeepingTask.Query().
		Where(enthousekeeping.ID(taskID), enthousekeeping.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "task not found", http.StatusNotFound)
		return
	}

	updater := task.Update()
	justCompleted := false
	if v, ok := input["status"].(string); ok {
		updater.SetStatus(enthousekeeping.Status(v))
		if v == "completed" {
			updater.SetCompletedAt(time.Now())
			justCompleted = task.Status != enthousekeeping.StatusCompleted
		}
	}
	if v, ok := input["assigned_to"].(string); ok {
		if sid, err := uuid.Parse(v); err == nil {
			updater.SetAssignedTo(sid)
		}
	}
	if v, ok := input["notes"].(string); ok {
		updater.SetNotes(v)
	}
	if v, ok := input["priority"].(string); ok {
		updater.SetPriority(enthousekeeping.Priority(v))
	}

	updated, err := updater.Save(r.Context())
	if err != nil {
		h.log.Error("update housekeeping task failed", zap.Error(err))
		jsonError(w, "update failed", http.StatusInternalServerError)
		return
	}

	// Completing a clean/maintenance task is what actually frees the room back up — without
	// this, a room checked out via CheckOut/SettleFolio (which correctly sets it to "cleaning")
	// stayed stuck there forever: nothing else in the system ever moved it back to "available",
	// so every checked-out room silently vanished from the bookable pool until a staff member
	// happened to notice and patch its status by hand. Only transitions FROM the two
	// "out of service" statuses (cleaning/maintenance) — never overrides occupied/reserved, and
	// turndown/inspection tasks (service/QA on an otherwise-normal room) don't trigger this.
	if justCompleted {
		switch updated.TaskType {
		case enthousekeeping.TaskTypeCheckoutClean, enthousekeeping.TaskTypeRoutineClean, enthousekeeping.TaskTypeMaintenance:
			room, rerr := h.client.Room.Get(r.Context(), updated.RoomID)
			if rerr == nil && (room.Status == entroom.StatusCleaning || room.Status == entroom.StatusMaintenance) {
				if _, serr := h.client.Room.UpdateOneID(updated.RoomID).SetStatus(entroom.StatusAvailable).Save(r.Context()); serr != nil {
					h.log.Warn("housekeeping complete: failed to mark room available", zap.Stringer("room_id", updated.RoomID), zap.Error(serr))
				}
			}
		}
	}

	jsonOK(w, updated)
}
