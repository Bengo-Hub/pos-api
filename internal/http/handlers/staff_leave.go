package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entlv "github.com/bengobox/pos-service/internal/ent/leaverequest"
	authclient "github.com/Bengo-Hub/shared-auth-client"
)

type LeaveRequestHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewLeaveRequestHandler(log *zap.Logger, db *ent.Client) *LeaveRequestHandler {
	return &LeaveRequestHandler{log: log, db: db}
}

type createLeaveRequestInput struct {
	StartDate string `json:"start_date"` // YYYY-MM-DD
	EndDate   string `json:"end_date"`   // YYYY-MM-DD
	LeaveType string `json:"leave_type"` // annual | sick | unpaid | maternity | compassionate | other
	Reason    string `json:"reason"`
}

type leaveStatusInput struct {
	Status          string `json:"status"`           // approved | rejected
	RejectionReason string `json:"rejection_reason"`
}

// ListStaffLeaveRequests handles GET /{tenantID}/pos/staff/{staffID}/leave-requests
func (h *LeaveRequestHandler) ListStaffLeaveRequests(w http.ResponseWriter, r *http.Request) {
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

	leaves, err := h.db.LeaveRequest.Query().
		Where(entlv.TenantID(tid), entlv.StaffMemberID(staffID)).
		Order(ent.Desc(entlv.FieldCreatedAt)).
		All(r.Context())
	if err != nil {
		h.log.Error("list leave requests failed", zap.Error(err))
		jsonError(w, "failed to list leave requests", http.StatusInternalServerError)
		return
	}
	jsonOK(w, leaves)
}

// ListLeaveRequests handles GET /{tenantID}/pos/leave-requests (manager view — all staff)
func (h *LeaveRequestHandler) ListLeaveRequests(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	query := h.db.LeaveRequest.Query().
		Where(entlv.TenantID(tid)).
		Order(ent.Desc(entlv.FieldCreatedAt))

	if status := r.URL.Query().Get("status"); status != "" {
		query = query.Where(entlv.StatusEQ(entlv.Status(status)))
	}

	leaves, err := query.All(r.Context())
	if err != nil {
		h.log.Error("list leave requests failed", zap.Error(err))
		jsonError(w, "failed to list leave requests", http.StatusInternalServerError)
		return
	}
	jsonOK(w, leaves)
}

// CreateLeaveRequest handles POST /{tenantID}/pos/staff/{staffID}/leave-requests
func (h *LeaveRequestHandler) CreateLeaveRequest(w http.ResponseWriter, r *http.Request) {
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

	var inp createLeaveRequestInput
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	start, err := time.Parse("2006-01-02", inp.StartDate)
	if err != nil {
		jsonError(w, "invalid start_date — use YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	end, err := time.Parse("2006-01-02", inp.EndDate)
	if err != nil {
		jsonError(w, "invalid end_date — use YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	if end.Before(start) {
		jsonError(w, "end_date must not be before start_date", http.StatusUnprocessableEntity)
		return
	}

	if entlv.LeaveTypeValidator(entlv.LeaveType(inp.LeaveType)) != nil {
		jsonError(w, "leave_type must be annual, sick, unpaid, maternity, compassionate, or other", http.StatusBadRequest)
		return
	}

	claims, _ := authclient.ClaimsFromContext(r.Context())
	requestorID, _ := uuid.Parse(claims.Subject)

	creator := h.db.LeaveRequest.Create().
		SetTenantID(tid).
		SetStaffMemberID(staffID).
		SetStartDate(start).
		SetEndDate(end).
		SetLeaveType(entlv.LeaveType(inp.LeaveType)).
		SetStatus(entlv.StatusPending).
		SetRequestedBy(requestorID)

	if inp.Reason != "" {
		creator = creator.SetReason(inp.Reason)
	}

	saved, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create leave request failed", zap.Error(err))
		jsonError(w, "failed to create leave request", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusCreated, saved)
}

// UpdateLeaveStatus handles PATCH /{tenantID}/pos/leave-requests/{leaveID}/status
func (h *LeaveRequestHandler) UpdateLeaveStatus(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	leaveID, err := uuid.Parse(chi.URLParam(r, "leaveID"))
	if err != nil {
		jsonError(w, "invalid leave_id", http.StatusBadRequest)
		return
	}

	existing, err := h.db.LeaveRequest.Get(r.Context(), leaveID)
	if err != nil || existing.TenantID != tid {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	var inp leaveStatusInput
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if inp.Status != "approved" && inp.Status != "rejected" {
		jsonError(w, "status must be 'approved' or 'rejected'", http.StatusUnprocessableEntity)
		return
	}

	claims, _ := authclient.ClaimsFromContext(r.Context())
	approverID, _ := uuid.Parse(claims.Subject)

	upd := h.db.LeaveRequest.UpdateOneID(leaveID).
		SetStatus(entlv.Status(inp.Status)).
		SetApprovedBy(approverID)

	if inp.RejectionReason != "" {
		upd = upd.SetRejectionReason(inp.RejectionReason)
	}

	saved, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update leave status failed", zap.Error(err))
		jsonError(w, "failed to update leave request", http.StatusInternalServerError)
		return
	}
	jsonOK(w, saved)
}
