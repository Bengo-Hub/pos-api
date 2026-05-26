package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
)

// StaffHandler handles staff CRUD operations for the pos-ui admin/team panel.
// Only accessible to manager and admin roles (enforced via STAFF_MANAGE permission).
type StaffHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewStaffHandler(log *zap.Logger, client *ent.Client) *StaffHandler {
	return &StaffHandler{log: log, client: client}
}

// managerRoles that may NOT be created/edited/deactivated by a manager (only admin can).
var managementProtectedRoles = map[string]bool{
	"admin":   true,
	"manager": true,
}

func requesterRole(r *http.Request) string {
	if role, ok := r.Context().Value("pos_role").(string); ok {
		return role
	}
	return ""
}

// ── GET /{tenant}/pos/staff/admin — full staff list for management UI ─────────

type staffAdminItem struct {
	ID             string  `json:"id"`
	UserID         string  `json:"user_id"`
	OutletID       string  `json:"outlet_id"`
	Name           string  `json:"name"`
	Role           string  `json:"role"`
	EmploymentType string  `json:"employment_type"`
	IsActive       bool    `json:"is_active"`
	HasPIN         bool    `json:"has_pin"`
	HourlyRate     *float64 `json:"hourly_rate,omitempty"`
	DailyRate      *float64 `json:"daily_rate,omitempty"`
	MonthlySalary  *float64 `json:"monthly_salary,omitempty"`
	MpesaPhone     *string  `json:"mpesa_phone,omitempty"`
	CreatedAt      string   `json:"created_at"`
}

func (h *StaffHandler) ListStaffForAdmin(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	members, err := h.client.StaffMember.Query().
		Where(entstaff.TenantID(tid)).
		Order(ent.Asc(entstaff.FieldName)).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]staffAdminItem, 0, len(members))
	for _, m := range members {
		item := staffAdminItem{
			ID:             m.ID.String(),
			UserID:         m.UserID.String(),
			OutletID:       m.OutletID.String(),
			Name:           m.Name,
			Role:           m.Role,
			EmploymentType: string(m.EmploymentType),
			IsActive:       m.IsActive,
			HasPIN:         m.PinHash != nil,
			HourlyRate:     m.HourlyRate,
			DailyRate:      m.DailyRate,
			MonthlySalary:  m.MonthlySalary,
			MpesaPhone:     m.MpesaPhone,
			CreatedAt:      m.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		out = append(out, item)
	}
	jsonOK(w, map[string]any{"data": out, "total": len(out)})
}

// ── POST /{tenant}/pos/staff — create new staff member ───────────────────────

type createStaffInput struct {
	UserID         string  `json:"user_id"`
	OutletID       string  `json:"outlet_id"`
	Name           string  `json:"name"`
	Role           string  `json:"role"`
	EmploymentType string  `json:"employment_type"`
	HourlyRate     *float64 `json:"hourly_rate"`
	DailyRate      *float64 `json:"daily_rate"`
	MonthlySalary  *float64 `json:"monthly_salary"`
	MpesaPhone     *string  `json:"mpesa_phone"`
}

func (h *StaffHandler) CreateStaff(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createStaffInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Name == "" || input.Role == "" {
		jsonError(w, "name and role are required", http.StatusBadRequest)
		return
	}

	// Manager cannot create admin or manager-level accounts
	if managementProtectedRoles[input.Role] && requesterRole(r) == "manager" {
		jsonError(w, "managers cannot create admin or manager-level staff", http.StatusForbidden)
		return
	}

	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	userID, err := uuid.Parse(input.UserID)
	if err != nil {
		jsonError(w, "invalid user_id", http.StatusBadRequest)
		return
	}

	empType := entstaff.EmploymentTypeFullTime
	if input.EmploymentType != "" {
		empType = entstaff.EmploymentType(input.EmploymentType)
	}

	q := h.client.StaffMember.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetUserID(userID).
		SetName(input.Name).
		SetRole(input.Role).
		SetEmploymentType(empType).
		SetIsActive(true)

	if input.HourlyRate != nil {
		q = q.SetHourlyRate(*input.HourlyRate)
	}
	if input.DailyRate != nil {
		q = q.SetDailyRate(*input.DailyRate)
	}
	if input.MonthlySalary != nil {
		q = q.SetMonthlySalary(*input.MonthlySalary)
	}
	if input.MpesaPhone != nil {
		q = q.SetMpesaPhone(*input.MpesaPhone)
	}

	member, err := q.Save(r.Context())
	if err != nil {
		h.log.Error("create staff", zap.Error(err))
		jsonError(w, "failed to create staff member", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusCreated, map[string]any{"id": member.ID.String(), "name": member.Name})
}

// ── PATCH /{tenant}/pos/staff/{staffID} — update staff profile ───────────────

type updateStaffInput struct {
	Name           *string  `json:"name"`
	Role           *string  `json:"role"`
	OutletID       *string  `json:"outlet_id"`
	EmploymentType *string  `json:"employment_type"`
	IsActive       *bool    `json:"is_active"`
	HourlyRate     *float64 `json:"hourly_rate"`
	DailyRate      *float64 `json:"daily_rate"`
	MonthlySalary  *float64 `json:"monthly_salary"`
	MpesaPhone     *string  `json:"mpesa_phone"`
}

func (h *StaffHandler) UpdateStaff(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	staffID, err := uuid.Parse(chi.URLParam(r, "staffID"))
	if err != nil {
		jsonError(w, "invalid staffID", http.StatusBadRequest)
		return
	}

	var input updateStaffInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	member, err := h.client.StaffMember.Query().
		Where(entstaff.ID(staffID), entstaff.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "staff member not found", http.StatusNotFound)
		return
	}

	// Manager cannot update admin or manager-level staff
	if managementProtectedRoles[member.Role] && requesterRole(r) == "manager" {
		jsonError(w, "managers cannot edit admin or manager-level staff", http.StatusForbidden)
		return
	}
	if input.Role != nil && managementProtectedRoles[*input.Role] && requesterRole(r) == "manager" {
		jsonError(w, "managers cannot assign admin or manager role", http.StatusForbidden)
		return
	}

	upd := h.client.StaffMember.UpdateOne(member)
	if input.Name != nil {
		upd = upd.SetName(*input.Name)
	}
	if input.Role != nil {
		upd = upd.SetRole(*input.Role)
	}
	if input.OutletID != nil {
		if oid, err := uuid.Parse(*input.OutletID); err == nil {
			upd = upd.SetOutletID(oid)
		}
	}
	if input.EmploymentType != nil {
		upd = upd.SetEmploymentType(entstaff.EmploymentType(*input.EmploymentType))
	}
	if input.IsActive != nil {
		upd = upd.SetIsActive(*input.IsActive)
	}
	if input.HourlyRate != nil {
		upd = upd.SetHourlyRate(*input.HourlyRate)
	}
	if input.DailyRate != nil {
		upd = upd.SetDailyRate(*input.DailyRate)
	}
	if input.MonthlySalary != nil {
		upd = upd.SetMonthlySalary(*input.MonthlySalary)
	}
	if input.MpesaPhone != nil {
		upd = upd.SetMpesaPhone(*input.MpesaPhone)
	}

	if _, err := upd.Save(r.Context()); err != nil {
		h.log.Error("update staff", zap.Error(err))
		jsonError(w, "failed to update staff member", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── POST /{tenant}/pos/staff/{staffID}/deactivate — soft-delete staff ─────────

func (h *StaffHandler) DeactivateStaff(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	staffID, err := uuid.Parse(chi.URLParam(r, "staffID"))
	if err != nil {
		jsonError(w, "invalid staffID", http.StatusBadRequest)
		return
	}

	member, err := h.client.StaffMember.Query().
		Where(entstaff.ID(staffID), entstaff.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "staff member not found", http.StatusNotFound)
		return
	}

	// Manager cannot deactivate admin or manager-level staff
	if managementProtectedRoles[member.Role] && requesterRole(r) == "manager" {
		jsonError(w, "managers cannot deactivate admin or manager-level staff", http.StatusForbidden)
		return
	}

	if err := h.client.StaffMember.UpdateOne(member).SetIsActive(false).Exec(r.Context()); err != nil {
		h.log.Error("deactivate staff", zap.Error(err))
		jsonError(w, "failed to deactivate staff member", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
