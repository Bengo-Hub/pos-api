package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entqueue "github.com/bengobox/pos-service/internal/ent/servicequeueentry"
)

type QueueHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewQueueHandler(log *zap.Logger, db *ent.Client) *QueueHandler {
	return &QueueHandler{log: log, db: db}
}

type createQueueEntryInput struct {
	OutletID      string `json:"outlet_id"`
	CustomerName  string `json:"customer_name"`
	CustomerPhone string `json:"customer_phone"`
	ServiceName   string `json:"service_name"`
	StaffMemberID string `json:"staff_member_id"`
	Notes         string `json:"notes"`
}

type updateQueueStatusInput struct {
	Status string `json:"status"`
}

// List returns active queue entries for the tenant, optionally filtered by outlet_id query param.
func (h *QueueHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	q := h.db.ServiceQueueEntry.Query().
		Where(entqueue.TenantID(tid)).
		Order(ent.Asc(entqueue.FieldCreatedAt))

	if outletStr := r.URL.Query().Get("outlet_id"); outletStr != "" {
		if oid, err := uuid.Parse(outletStr); err == nil {
			q = q.Where(entqueue.OutletID(oid))
		}
	}
	if statusFilter := r.URL.Query().Get("status"); statusFilter != "" {
		q = q.Where(entqueue.StatusEQ(entqueue.Status(statusFilter)))
	} else {
		// Default: exclude done/cancelled
		q = q.Where(entqueue.StatusIn(entqueue.StatusWaiting, entqueue.StatusInProgress))
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	entries, err := q.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("queue list", zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(entries, total, p))
}

// Create adds a new walk-in entry to the queue.
func (h *QueueHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	var input createQueueEntryInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if input.CustomerName == "" {
		http.Error(w, "customer_name required", http.StatusBadRequest)
		return
	}

	oid, err := uuid.Parse(input.OutletID)
	if err != nil {
		http.Error(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	// Queue position = current count of non-done entries + 1
	count, _ := h.db.ServiceQueueEntry.Query().
		Where(entqueue.TenantID(tid), entqueue.OutletID(oid),
			entqueue.StatusIn(entqueue.StatusWaiting, entqueue.StatusInProgress)).
		Count(r.Context())

	create := h.db.ServiceQueueEntry.Create().
		SetTenantID(tid).
		SetOutletID(oid).
		SetCustomerName(input.CustomerName).
		SetQueuePosition(count + 1)

	if input.CustomerPhone != "" {
		create = create.SetCustomerPhone(input.CustomerPhone)
	}
	if input.ServiceName != "" {
		create = create.SetServiceName(input.ServiceName)
	}
	if input.Notes != "" {
		create = create.SetNotes(input.Notes)
	}
	if input.StaffMemberID != "" {
		if sid, err := uuid.Parse(input.StaffMemberID); err == nil {
			create = create.SetStaffMemberID(sid)
		}
	}

	entry, err := create.Save(r.Context())
	if err != nil {
		h.log.Error("queue create", zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(entry)
}

// UpdateStatus transitions queue entry status (waiting → in_progress → done / cancelled).
func (h *QueueHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	entryID, err := uuid.Parse(chi.URLParam(r, "entryID"))
	if err != nil {
		http.Error(w, "invalid entry id", http.StatusBadRequest)
		return
	}

	var input updateQueueStatusInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	newStatus := entqueue.Status(input.Status)
	switch newStatus {
	case entqueue.StatusWaiting, entqueue.StatusInProgress, entqueue.StatusDone, entqueue.StatusCancelled:
	default:
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}

	entry, err := h.db.ServiceQueueEntry.Get(r.Context(), entryID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if entry.TenantID != tid {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	update := h.db.ServiceQueueEntry.UpdateOneID(entryID).SetStatus(newStatus)

	now := time.Now()
	if newStatus == entqueue.StatusInProgress {
		update = update.SetStartedAt(now)
	} else if newStatus == entqueue.StatusDone {
		update = update.SetCompletedAt(now)
	}

	updated, err := update.Save(r.Context())
	if err != nil {
		h.log.Error("queue update", zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}

// AssignStaff handles POST /{tenantID}/pos/queue/entries/{entryID}/assign
func (h *QueueHandler) AssignStaff(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	entryID, err := uuid.Parse(chi.URLParam(r, "entryID"))
	if err != nil {
		http.Error(w, "invalid entry id", http.StatusBadRequest)
		return
	}

	var input struct {
		StaffMemberID string `json:"staff_member_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	staffID, err := uuid.Parse(input.StaffMemberID)
	if err != nil {
		http.Error(w, "invalid staff_member_id", http.StatusBadRequest)
		return
	}

	entry, err := h.db.ServiceQueueEntry.Get(r.Context(), entryID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if entry.TenantID != tid {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	updated, err := h.db.ServiceQueueEntry.UpdateOneID(entryID).
		SetStaffMemberID(staffID).
		Save(r.Context())
	if err != nil {
		h.log.Error("queue assign staff failed", zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}
