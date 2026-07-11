package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/bengobox/pos-service/internal/ent"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	entstaffoutlet "github.com/bengobox/pos-service/internal/ent/staffoutlet"
	entuser "github.com/bengobox/pos-service/internal/ent/user"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// StaffHandler handles staff CRUD operations for the pos-ui admin/team panel.
// Only accessible to manager and admin roles (enforced via STAFF_MANAGE permission).
type StaffHandler struct {
	log         *zap.Logger
	client      *ent.Client
	authURL     string
	internalKey string
	http        *http.Client
}

func NewStaffHandler(log *zap.Logger, client *ent.Client, authURL, internalKey string) *StaffHandler {
	return &StaffHandler{
		log:         log,
		client:      client,
		authURL:     authURL,
		internalKey: internalKey,
		http:        &http.Client{Timeout: 15 * time.Second},
	}
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

	p := pagination.Parse(r)
	baseQ := h.client.StaffMember.Query().Where(entstaff.TenantID(tid))
	total, _ := baseQ.Clone().Count(r.Context())
	members, err := baseQ.Order(ent.Asc(entstaff.FieldName)).
		WithOutlets(func(soq *ent.StaffOutletQuery) {
			soq.Where(entstaffoutlet.IsHomeOutlet(true)).Limit(1)
		}).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]staffAdminItem, 0, len(members))
	for _, m := range members {
		homeOutletID := ""
		if len(m.Edges.Outlets) > 0 {
			homeOutletID = m.Edges.Outlets[0].OutletID.String()
		}
		item := staffAdminItem{
			ID:             m.ID.String(),
			UserID:         m.UserID.String(),
			OutletID:       homeOutletID,
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
	jsonOK(w, pagination.NewResponse(out, total, p))
}

// ── POST /{tenant}/pos/staff — create new staff member ───────────────────────

type createStaffInput struct {
	// Email is REQUIRED. Every staff member is provisioned in auth-service first (S2S)
	// so the POS user is linked to the REAL auth user id, consistent across all service
	// DBs. No service mints its own user id anymore (that produced orphan records that
	// SSO later duplicated — see the Berita Nyambura incident). user_id, when supplied,
	// is honoured as an override for an already-known auth user id.
	Email          string   `json:"email"`
	UserID         string   `json:"user_id"` // optional override; normally resolved from auth by email
	OutletID       string   `json:"outlet_id"`
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	EmploymentType string   `json:"employment_type"`
	Phone          string   `json:"phone"`
	PIN            string   `json:"pin"` // optional 4-6 digit terminal PIN; set on the new member if provided
	HourlyRate     *float64 `json:"hourly_rate"`
	DailyRate      *float64 `json:"daily_rate"`
	MonthlySalary  *float64 `json:"monthly_salary"`
	MpesaPhone     *string  `json:"mpesa_phone"`
}

// authMemberResponse mirrors auth-api's tenantMemberResponse (only the field we need).
type authMemberResponse struct {
	UserID string `json:"user_id"`
}

// provisionAuthMember calls auth-api's S2S member endpoint to find-or-create the auth
// user by email and attach the tenant membership, returning the REAL auth user id.
// This is the single source of user identity — pos never invents its own user id.
func (h *StaffHandler) provisionAuthMember(ctx context.Context, tenantID uuid.UUID, email, name, role, phone, outletID string) (uuid.UUID, error) {
	authBase := h.authURL
	if envURL := os.Getenv("AUTH_API_URL"); envURL != "" {
		authBase = envURL // mirror tenant.Syncer's override so both S2S paths hit the same base
	}
	if authBase == "" || h.internalKey == "" {
		return uuid.Nil, fmt.Errorf("auth S2S not configured (AUTH_SERVICE_URL / INTERNAL_SERVICE_KEY)")
	}
	body := map[string]any{
		"email": strings.ToLower(strings.TrimSpace(email)),
		"name":  name,
		"roles": []string{role}, // POS role names align with SSO role names (waiter, cashier, ...)
	}
	if phone != "" {
		body["phone"] = phone
	}
	if outletID != "" {
		body["outlet_id"] = outletID
	}
	buf, _ := json.Marshal(body)
	url := strings.TrimRight(authBase, "/") + "/api/v1/s2s/tenants/" + tenantID.String() + "/members"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return uuid.Nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", h.internalKey)

	resp, err := h.http.Do(req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth member provision: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return uuid.Nil, fmt.Errorf("auth member provision: HTTP %d", resp.StatusCode)
	}
	var out authMemberResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return uuid.Nil, fmt.Errorf("auth member provision: decode: %w", err)
	}
	id, err := uuid.Parse(out.UserID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth member provision: bad user_id %q: %w", out.UserID, err)
	}
	return id, nil
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
	// Email is mandatory: it's the key auth-service uses to find-or-create the real
	// user id. Without it we'd have to invent an id (the orphan bug we're eliminating).
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	if input.UserID == "" && (input.Email == "" || !strings.Contains(input.Email, "@")) {
		jsonError(w, "a valid email is required to create a staff member", http.StatusBadRequest)
		return
	}

	// Manager cannot create admin or manager-level accounts
	if managementProtectedRoles[input.Role] && requesterRole(r) == "manager" {
		jsonError(w, "managers cannot create admin or manager-level staff", http.StatusForbidden)
		return
	}

	// Enforce the plan's structural staff caps (hard-block, no overage): total head-count
	// (max_staff) and, for cashier accounts, the cashier seat cap (max_cashiers).
	if count, cerr := h.client.StaffMember.Query().Where(entstaff.TenantID(tid)).Count(r.Context()); cerr == nil {
		if !subscriptions.CheckStructuralLimit(w, r, "staff", subscriptions.LimitStaff, count) {
			return
		}
	}
	if input.Role == "cashier" {
		if count, cerr := h.client.StaffMember.Query().
			Where(entstaff.TenantID(tid), entstaff.Role("cashier")).
			Count(r.Context()); cerr == nil {
			if !subscriptions.CheckStructuralLimit(w, r, "cashiers", subscriptions.LimitCashiers, count) {
				return
			}
		}
	}

	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	// Resolve the REAL auth-service user id. When a user_id override is supplied we
	// trust it; otherwise we provision the user in auth-service by email (S2S) and use
	// the id it returns. We NEVER mint our own id — that produced orphan users that SSO
	// later duplicated (Berita Nyambura incident).
	var userID uuid.UUID
	if input.UserID != "" {
		userID, err = uuid.Parse(input.UserID)
		if err != nil {
			jsonError(w, "invalid user_id", http.StatusBadRequest)
			return
		}
	} else {
		userID, err = h.provisionAuthMember(r.Context(), tid, input.Email, input.Name, input.Role, input.Phone, input.OutletID)
		if err != nil {
			h.log.Error("provision auth member", zap.Error(err))
			jsonError(w, "could not provision user in auth-service", http.StatusBadGateway)
			return
		}
	}

	// Ensure a local POS user row exists, linked to the real auth id. Idempotent: the
	// async auth.user.created event provisions the same row, whichever lands first wins.
	if input.Email != "" {
		if uerr := h.client.User.Create().
			SetID(userID).
			SetAuthServiceUserID(userID).
			SetTenantID(tid).
			SetEmail(input.Email).
			SetFullName(input.Name).
			SetStatus("active").
			SetSyncStatus("synced").
			SetSyncAt(time.Now()).
			OnConflictColumns(entuser.FieldID).
			DoNothing().
			Exec(r.Context()); uerr != nil && !ent.IsConstraintError(uerr) {
			h.log.Warn("ensure pos user row", zap.Error(uerr))
		}
	}

	empType := entstaff.EmploymentTypeFullTime
	if input.EmploymentType != "" {
		empType = entstaff.EmploymentType(input.EmploymentType)
	}

	q := h.client.StaffMember.Create().
		SetTenantID(tid).
		SetUserID(userID).
		SetName(input.Name).
		SetRole(input.Role).
		SetEmploymentType(empType).
		SetIsActive(true)

	// Optional terminal PIN — lets the new member clock in immediately without an SSO login.
	if input.PIN != "" {
		if len(input.PIN) < 4 {
			jsonError(w, "pin must be at least 4 digits", http.StatusBadRequest)
			return
		}
		hash, hErr := bcrypt.GenerateFromPassword([]byte(input.PIN), bcrypt.DefaultCost)
		if hErr != nil {
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		q = q.SetPinHash(string(hash)).SetPinFastHash(pinFastHash(tid, userID, input.PIN))
	}

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

	// Assign staff to outlet via join table.
	_ = h.client.StaffOutlet.Create().
		SetTenantID(tid).
		SetStaffMemberID(member.ID).
		SetOutletID(outletID).
		SetIsHomeOutlet(true).
		OnConflict().DoNothing().Exec(r.Context())

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
			// Upsert StaffOutlet — add new outlet assignment if not already assigned.
			_ = h.client.StaffOutlet.Create().
				SetTenantID(tid).
				SetStaffMemberID(member.ID).
				SetOutletID(oid).
				SetIsHomeOutlet(true).
				OnConflict().DoNothing().Exec(r.Context())
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
