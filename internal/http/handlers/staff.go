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
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posrolev2"
	"github.com/bengobox/pos-service/internal/ent/posuserroleassignment"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	entstaffoutlet "github.com/bengobox/pos-service/internal/ent/staffoutlet"
	entuser "github.com/bengobox/pos-service/internal/ent/user"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
	"github.com/bengobox/pos-service/internal/posrole"
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

// S2SPinHash answers a staff member's PIN hash for inventory-api, so someone newly given inventory
// keeps the PIN they use at the till. Internal key only. The hash is a bcrypt digest, and the same
// value already travels on the auth.user.pin_set event.
// GET /api/v1/s2s/{tenant}/staff/{userID}/pin-hash
func (h *StaffHandler) S2SPinHash(w http.ResponseWriter, r *http.Request) {
	tid, err := s2sTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	uid, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		jsonError(w, "invalid user", http.StatusBadRequest)
		return
	}
	member, err := h.client.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.UserID(uid), entstaff.IsActive(true), entstaff.PinHashNotNil()).
		First(r.Context())
	if err != nil || member.PinHash == nil || *member.PinHash == "" {
		jsonError(w, "no pin", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]any{"pin_hash": *member.PinHash})
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

// privilegedStaffRole reports whether a raw role is admin or manager tier after canonicalising
// (lowercase, trim, aliases such as owner, superuser, super_admin, pos_admin, store_manager).
// Only an admin-level caller may create, edit, promote, deactivate or reset the PIN of such
// staff. The old guard compared the raw string with {"admin","manager"}, so a manager could
// create "owner" or "superuser" staff, and " Admin" slipped past too.
func privilegedStaffRole(role string) bool {
	return posrole.IsAdminLevel(role)
}

// callerIsUser reports whether the signed-in caller is the given auth user.
func callerIsUser(r *http.Request, userID uuid.UUID) bool {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil || userID == uuid.Nil {
		return false
	}
	sub, err := uuid.Parse(claims.Subject)
	return err == nil && sub == userID
}

// posRoleRank ranks POS roles so the highest-privilege one wins when a principal carries
// several roles (SSO principals often carry a global role list). Used ONLY to resolve the
// caller's effective role for the manager-authority guardrails — enforcement of access to a
// route is done by RequireServicePermission middleware, not by this ranking.
var posRoleRank = map[string]int{
	"superuser": 100, "owner": 95, "super_admin": 95, "pos_admin": 92,
	"admin": 90, "store_manager": 72, "manager": 60, "accountant": 40,
	"receptionist": 30,
	"cashier": 20, "barista": 20, "waiter": 18, "bar": 16, "kitchen": 16,
	"stylist": 16, "therapist": 16, "technician": 16, "viewer": 5,
}

// adminLevelStaffRoles lists every raw StaffMember.Role value (current names + legacy aliases,
// see posrole.Canonicalize) that grants admin/manager-tier outlet access — e.g. PIN login into,
// or SwitchOutlet to, any outlet in the tenant rather than only outlets a StaffOutlet row assigns.
var adminLevelStaffRoles = []string{
	"admin", "manager",
	"superuser", "owner", "super_admin", "pos_admin", "tenant_admin", "system_admin", "administrator",
	"store_manager", "outlet_manager", "supervisor",
}

// isAdminLevelRole reports whether a raw StaffMember.Role (or claims role) grants admin/manager
// tier outlet access. Thin wrapper kept for call-site readability in this package.
func isAdminLevelRole(role string) bool {
	return posrole.IsAdminLevel(role)
}

// canonicalizeRole folds SSO/global role aliases onto the POS role vocabulary so the
// guardrails treat an SSO "owner"/"superuser" as admin-level and "staff" as a cashier.
func canonicalizeRole(role string) string {
	return posrole.Canonicalize(role)
}

// requesterRole resolves the caller's EFFECTIVE POS role from the auth claims. Both SSO JWTs
// (global roles) and terminal PIN JWTs (Roles = [staffMember.role]) carry the role in
// claims.Roles, so we pick the highest-privilege role after folding global aliases onto the
// POS vocabulary. Platform owners / superusers resolve to "admin". Returns "" when unknown.
//
// NOTE: this replaces the previous implementation that read a `pos_role` context key which was
// never written anywhere — the manager-authority guards below were silently inert until now.
func requesterRole(r *http.Request) string {
	// Legacy context key kept as an explicit override if some middleware ever sets it.
	if role, ok := r.Context().Value("pos_role").(string); ok && role != "" {
		return canonicalizeRole(role)
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil {
		return ""
	}
	if claims.IsPlatformOwner || claims.IsSuperuser() {
		return "admin"
	}
	best, bestRank := "", -1
	for _, role := range claims.Roles {
		c := canonicalizeRole(role)
		if rank, known := posRoleRank[c]; known && rank > bestRank {
			best, bestRank = c, rank
		}
	}
	return best
}

// requesterIsAdminLevel reports whether the caller outranks a manager (admin/owner/platform).
// Managers get a restricted subset of RBAC authority (the "guardrailed manager" policy):
// they may manage custom roles and non-privileged staff, but may not touch system roles or
// admin/manager-level staff — only an admin-level caller may.
func requesterIsAdminLevel(r *http.Request) bool {
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
		if claims.IsPlatformOwner || claims.IsSuperuser() {
			return true
		}
	}
	return requesterRole(r) == "admin"
}

// ── GET /{tenant}/pos/staff/admin — full staff list for management UI ─────────

type staffAdminItem struct {
	ID             string   `json:"id"`
	UserID         string   `json:"user_id"`
	OutletID       string   `json:"outlet_id"`
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	EmploymentType string   `json:"employment_type"`
	IsActive       bool     `json:"is_active"`
	HasPIN         bool     `json:"has_pin"`
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
	// Same search-past-pagination fix as inventory-api's supplier picker: without this, a
	// staff member sorting past the default page limit is invisible to any frontend picker
	// that searches by typing (assign-staff selects on layaway/shift-rotation/staff-credit).
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		baseQ = baseQ.Where(entstaff.NameContainsFold(search))
	}
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

// authMemberResponse mirrors auth-api's tenantMemberResponse (only the fields we need). Roles is
// the membership's roles after the add, so CreateStaff can see that an existing person it was
// given by email is already admin or manager tier.
type authMemberResponse struct {
	UserID string   `json:"user_id"`
	Roles  []string `json:"roles"`
}

// authBaseURL is the auth-api base for S2S calls. AUTH_API_URL wins, mirroring tenant.Syncer's
// override so every S2S path hits the same base.
func (h *StaffHandler) authBaseURL() string {
	if envURL := os.Getenv("AUTH_API_URL"); envURL != "" {
		return envURL
	}
	return h.authURL
}

// provisionAuthMember calls auth-api's S2S member endpoint to find-or-create the auth
// user by email and attach the tenant membership, returning the REAL auth user id and the
// membership's roles. This is the single source of user identity — pos never invents its own
// user id.
func (h *StaffHandler) provisionAuthMember(ctx context.Context, tenantID uuid.UUID, email, name, role, phone, outletID string) (uuid.UUID, []string, error) {
	authBase := h.authBaseURL()
	if authBase == "" || h.internalKey == "" {
		return uuid.Nil, nil, fmt.Errorf("auth S2S not configured (AUTH_SERVICE_URL / INTERNAL_SERVICE_KEY)")
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
		return uuid.Nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", h.internalKey)

	resp, err := h.http.Do(req)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("auth member provision: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return uuid.Nil, nil, fmt.Errorf("auth member provision: HTTP %d", resp.StatusCode)
	}
	var out authMemberResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return uuid.Nil, nil, fmt.Errorf("auth member provision: decode: %w", err)
	}
	id, err := uuid.Parse(out.UserID)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("auth member provision: bad user_id %q: %w", out.UserID, err)
	}
	return id, out.Roles, nil
}

// authTenantMemberRoles finds a person among the tenant's members in auth-api (S2S member list,
// GET /api/v1/s2s/{tenant}/users) by user id or by email (either may be empty), and returns the
// first match's membership roles. CreateStaff may fall back from a user id to the email, so both
// are checked and the roles of every match are returned together. found is false when neither
// names a member. Any failure is an error, so the caller fails closed.
func (h *StaffHandler) authTenantMemberRoles(ctx context.Context, tenantID, userID uuid.UUID, email string) (roles []string, found bool, err error) {
	authBase := h.authBaseURL()
	if authBase == "" || h.internalKey == "" {
		return nil, false, fmt.Errorf("auth S2S not configured (AUTH_SERVICE_URL / INTERNAL_SERVICE_KEY)")
	}
	url := strings.TrimRight(authBase, "/") + "/api/v1/s2s/" + tenantID.String() + "/users"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("X-API-Key", h.internalKey)
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("auth member list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("auth member list: HTTP %d", resp.StatusCode)
	}
	var members []struct {
		ID    string   `json:"id"`
		Email string   `json:"email"`
		Roles []string `json:"roles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		return nil, false, fmt.Errorf("auth member list: decode: %w", err)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	for _, m := range members {
		id, perr := uuid.Parse(m.ID)
		if perr != nil {
			continue
		}
		if (userID != uuid.Nil && id == userID) ||
			(email != "" && strings.EqualFold(strings.TrimSpace(m.Email), email)) {
			roles = append(roles, m.Roles...)
			found = true
		}
	}
	return roles, found, nil
}

// authUserIsPlatformOwner asks auth-api (S2S) whether the user is a platform owner. Unlike the
// PIN handler's lookup it reports failures, so a guard can fail closed.
func (h *StaffHandler) authUserIsPlatformOwner(ctx context.Context, userID uuid.UUID) (bool, error) {
	authBase := h.authBaseURL()
	if authBase == "" || h.internalKey == "" {
		return false, fmt.Errorf("auth S2S not configured (AUTH_SERVICE_URL / INTERNAL_SERVICE_KEY)")
	}
	url := strings.TrimRight(authBase, "/") + "/api/v1/s2s/users/" + userID.String() + "/platform-owner"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-API-Key", h.internalKey)
	resp, err := h.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("auth platform owner lookup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("auth platform owner lookup: HTTP %d", resp.StatusCode)
	}
	var out struct {
		IsPlatformOwner bool `json:"is_platform_owner"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("auth platform owner lookup: decode: %w", err)
	}
	return out.IsPlatformOwner, nil
}

// userHasPrivilegedPOSAssignment reports whether userID holds a POS role assignment on tenant tid
// whose role code is admin or manager tier. A staff row bound to that person inherits those
// grants (resolveEffectivePermissions unions them in), so for binding and PIN purposes the row
// is that tier whatever its own role says.
//
// Read through role_id, as resolveEffectivePermissions does: the POSRoleV2 user_assignments edge
// maps to a different column that assignments never fill.
func userHasPrivilegedPOSAssignment(ctx context.Context, client *ent.Client, tid, userID uuid.UUID) (bool, error) {
	if userID == uuid.Nil {
		return false, nil
	}
	assignments, err := client.POSUserRoleAssignment.Query().
		Where(posuserroleassignment.TenantID(tid), posuserroleassignment.UserID(userID)).
		All(ctx)
	if err != nil {
		return false, fmt.Errorf("read pos role assignments: %w", err)
	}
	if len(assignments) == 0 {
		return false, nil
	}
	ids := make([]uuid.UUID, 0, len(assignments))
	for _, a := range assignments {
		ids = append(ids, a.RoleID)
	}
	codes, err := client.POSRoleV2.Query().Where(posrolev2.IDIn(ids...)).Select(posrolev2.FieldRoleCode).Strings(ctx)
	if err != nil {
		return false, fmt.Errorf("read assigned pos roles: %w", err)
	}
	return posrole.IsAdminLevelAny(codes), nil
}

// effectiveStaffPrivileged reports whether a staff row is admin or manager tier for the guards:
// its own role, or a POS role assignment its person holds in the row's tenant.
func effectiveStaffPrivileged(ctx context.Context, client *ent.Client, member *ent.StaffMember) (bool, error) {
	if privilegedStaffRole(member.Role) {
		return true, nil
	}
	return userHasPrivilegedPOSAssignment(ctx, client, member.TenantID, member.UserID)
}

// bindTargetPrivileged reports whether a staff row bound to userID on tenant tid would act for
// an admin or manager tier person: a platform owner in auth, the holder of an admin or manager
// POS role assignment here, or the person behind an existing admin or manager staff row here.
// Membership roles in auth are checked by the caller, which already has them.
func (h *StaffHandler) bindTargetPrivileged(ctx context.Context, tid, userID uuid.UUID) (bool, error) {
	if priv, err := userHasPrivilegedPOSAssignment(ctx, h.client, tid, userID); err != nil || priv {
		return priv, err
	}
	roles, err := h.client.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.UserID(userID)).
		Select(entstaff.FieldRole).
		Strings(ctx)
	if err != nil {
		return false, fmt.Errorf("read staff rows: %w", err)
	}
	if posrole.IsAdminLevelAny(roles) {
		return true, nil
	}
	return h.authUserIsPlatformOwner(ctx, userID)
}

// isTenantMember asks auth-api whether userID holds a membership in tenantID, through the S2S
// member lookup pos already uses for PIN outlet limits
// (GET /api/v1/s2s/tenants/{tenant}/members/{user}/outlet-limit: 200 member, 404 not a member).
// Any other answer is an error, so the caller fails closed.
func (h *StaffHandler) isTenantMember(ctx context.Context, tenantID, userID uuid.UUID) (bool, error) {
	authBase := h.authURL
	if envURL := os.Getenv("AUTH_API_URL"); envURL != "" {
		authBase = envURL
	}
	if authBase == "" || h.internalKey == "" {
		return false, fmt.Errorf("auth S2S not configured (AUTH_SERVICE_URL / INTERNAL_SERVICE_KEY)")
	}
	url := strings.TrimRight(authBase, "/") + "/api/v1/s2s/tenants/" + tenantID.String() + "/members/" + userID.String() + "/outlet-limit"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-API-Key", h.internalKey)
	resp, err := h.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("auth member lookup: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("auth member lookup: HTTP %d", resp.StatusCode)
	}
}

// syncAuthMemberRoles pushes a staff member's (base) role to auth-service by user_id so the SSO
// JWT `roles` claim reflects a role change made in the POS Team UI. Without this, editing a
// member's role via UpdateStaff only touched the local StaffMember.Role and the JWT roles claim
// went stale — breaking JWT-role-name resolution of custom roles (the urban-loft custom-role
// "can't access reports/modules" gap). auth-api AddTenantMember find-or-updates the membership
// and SetRoles(roles) on the existing row, so this is idempotent. Best-effort: a sync failure is
// logged by the caller and does NOT fail the local update (pos /auth/me still resolves via
// StaffMember.Role).
func (h *StaffHandler) syncAuthMemberRoles(ctx context.Context, tenantID, userID uuid.UUID, role, outletID string) error {
	authBase := h.authURL
	if envURL := os.Getenv("AUTH_API_URL"); envURL != "" {
		authBase = envURL
	}
	if authBase == "" || h.internalKey == "" {
		return fmt.Errorf("auth S2S not configured (AUTH_SERVICE_URL / INTERNAL_SERVICE_KEY)")
	}
	body := map[string]any{
		"user_id": userID.String(),
		"roles":   []string{role},
	}
	if outletID != "" {
		body["outlet_id"] = outletID
	}
	buf, _ := json.Marshal(body)
	url := strings.TrimRight(authBase, "/") + "/api/v1/s2s/tenants/" + tenantID.String() + "/members"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", h.internalKey)
	resp, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("auth member role sync: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth member role sync: HTTP %d", resp.StatusCode)
	}
	return nil
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

	// Only an admin-level caller may create admin or manager-tier staff (any alias).
	input.Role = strings.TrimSpace(input.Role)
	if privilegedStaffRole(input.Role) && !requesterIsAdminLevel(r) {
		jsonError(w, "only an admin can create admin or manager-level staff", http.StatusForbidden)
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

	// Resolve the REAL auth-service user id. A user_id override is honoured only when that user
	// is already a member of this tenant in auth-api, or the caller is an SSO platform owner.
	// Trusting it blindly let a tenant admin point a staff row at any auth user (a platform
	// admin included), set its PIN and sign in as them. Otherwise we provision the user in
	// auth-service by email (S2S) and use the id it returns. We NEVER mint our own id, which
	// produced orphan users that SSO later duplicated (Berita Nyambura incident).
	//
	// A staff row bound to a person acts for that person: it inherits their POS role assignments,
	// and a later auth sync raises its role to theirs. So only an admin-level caller may bind a
	// row to someone who is admin or manager tier (in auth, in pos's role assignments, or as a
	// platform owner); anyone below admin is refused before anything is written, and when auth
	// cannot answer the bind is refused too.
	callerIsAdmin := requesterIsAdminLevel(r)
	var overrideID uuid.UUID
	if input.UserID != "" {
		parsed, perr := uuid.Parse(input.UserID)
		if perr != nil {
			jsonError(w, "invalid user_id", http.StatusBadRequest)
			return
		}
		overrideID = parsed
	}
	if !callerIsAdmin {
		roles, found, lerr := h.authTenantMemberRoles(r.Context(), tid, overrideID, input.Email)
		if lerr != nil {
			h.log.Error("verify staff bind target tier", zap.Error(lerr))
			jsonError(w, "could not verify the person with auth-service", http.StatusBadGateway)
			return
		}
		if found && posrole.IsAdminLevelAny(roles) {
			jsonError(w, "only an admin can add a staff record for an admin or manager", http.StatusForbidden)
			return
		}
	}

	var userID uuid.UUID
	var provisionedRoles []string
	useOverride := false
	if input.UserID != "" {
		override := overrideID
		claims, _ := authclient.ClaimsFromContext(r.Context())
		if isSSOPlatformOwner(claims) {
			useOverride = true
		} else {
			member, merr := h.isTenantMember(r.Context(), tid, override)
			if merr != nil {
				h.log.Error("verify staff user_id membership", zap.Error(merr))
				jsonError(w, "could not verify user_id with auth-service", http.StatusBadGateway)
				return
			}
			if member {
				useOverride = true
			} else if input.Email == "" || !strings.Contains(input.Email, "@") {
				jsonError(w, "user_id is not a member of this tenant", http.StatusForbidden)
				return
			}
		}
		if useOverride {
			userID = override
		}
	}
	if !useOverride {
		userID, provisionedRoles, err = h.provisionAuthMember(r.Context(), tid, input.Email, input.Name, input.Role, input.Phone, input.OutletID)
		if err != nil {
			h.log.Error("provision auth member", zap.Error(err))
			jsonError(w, "could not provision user in auth-service", http.StatusBadGateway)
			return
		}
	}
	if !callerIsAdmin {
		if posrole.IsAdminLevelAny(provisionedRoles) {
			h.log.Warn("staff bind refused: provisioned person is admin or manager tier",
				zap.String("tenant_id", tid.String()), zap.String("user_id", userID.String()))
			jsonError(w, "only an admin can add a staff record for an admin or manager", http.StatusForbidden)
			return
		}
		priv, perr := h.bindTargetPrivileged(r.Context(), tid, userID)
		if perr != nil {
			h.log.Error("verify staff bind target tier", zap.Error(perr))
			jsonError(w, "could not verify the person with auth-service", http.StatusBadGateway)
			return
		}
		if priv {
			h.log.Warn("staff bind refused: person is admin or manager tier",
				zap.String("tenant_id", tid.String()), zap.String("user_id", userID.String()))
			jsonError(w, "only an admin can add a staff record for an admin or manager", http.StatusForbidden)
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

	// Only an admin-level caller may edit admin or manager-tier staff, or promote anyone into
	// that tier (any alias, compared after canonicalising). The tier counts the person's POS
	// role assignments too, which the row inherits.
	if !requesterIsAdminLevel(r) {
		priv, perr := effectiveStaffPrivileged(r.Context(), h.client, member)
		if perr != nil {
			h.log.Error("update staff: read role assignments", zap.Error(perr))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		if priv {
			jsonError(w, "only an admin can edit admin or manager-level staff", http.StatusForbidden)
			return
		}
	}
	if input.Role != nil {
		trimmed := strings.TrimSpace(*input.Role)
		input.Role = &trimmed
		if privilegedStaffRole(trimmed) && !requesterIsAdminLevel(r) {
			jsonError(w, "only an admin can assign an admin or manager role", http.StatusForbidden)
			return
		}
	}

	upd := h.client.StaffMember.UpdateOne(member)
	if input.Name != nil {
		upd = upd.SetName(*input.Name)
	}
	pinCleared := false
	if input.Role != nil {
		upd = upd.SetRole(*input.Role)
		// A raise into a higher tier clears the PIN: it may have been chosen by a manager while
		// the row was below their tier, and it would now sign in at the higher tier. The person
		// sets a new PIN.
		if posrole.RaisesTier(member.Role, *input.Role) && (member.PinHash != nil || member.PinFastHash != nil) {
			upd = upd.ClearPinHash().ClearPinFastHash().SetPinFailedAttempts(0).ClearPinLockedUntil()
			pinCleared = true
		}
	}
	if input.OutletID != nil {
		oid, oerr := uuid.Parse(*input.OutletID)
		if oerr != nil {
			jsonError(w, "invalid outlet_id", http.StatusBadRequest)
			return
		}
		// A staff member has exactly ONE home outlet. The previous upsert only ever ADDED a new
		// StaffOutlet row (DoNothing on conflict), so "switching" outlets left the old outlet ALSO
		// flagged is_home_outlet=true — ListStaffForAdmin's .Where(IsHomeOutlet(true)).Limit(1)
		// query then non-deterministically returned either the old or the new outlet. Demote every
		// other home-outlet row for this staff member before upserting the target as home.
		if err := h.client.StaffOutlet.Update().
			Where(
				entstaffoutlet.StaffMemberID(member.ID),
				entstaffoutlet.IsHomeOutlet(true),
				entstaffoutlet.OutletIDNEQ(oid),
			).
			SetIsHomeOutlet(false).
			Exec(r.Context()); err != nil {
			h.log.Warn("update staff: demote previous home outlet failed", zap.Error(err), zap.String("staff_id", staffID.String()))
		}
		if err := h.client.StaffOutlet.Create().
			SetTenantID(tid).
			SetStaffMemberID(member.ID).
			SetOutletID(oid).
			SetIsHomeOutlet(true).
			OnConflictColumns(entstaffoutlet.FieldStaffMemberID, entstaffoutlet.FieldOutletID).
			UpdateNewValues().
			Exec(r.Context()); err != nil {
			h.log.Warn("update staff: outlet reassignment failed", zap.Error(err), zap.String("staff_id", staffID.String()))
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
	if pinCleared {
		h.log.Info("staff PIN cleared after a role raise",
			zap.String("tenant_id", tid.String()), zap.String("staff_id", member.ID.String()))
	}

	// Role changed → sync the new base role to auth-service so the SSO JWT `roles` claim stays
	// current (otherwise custom/elevated roles never reach terminal_jwt.go leg-3 resolution).
	// Best-effort: a sync failure is logged, not fatal (pos /auth/me still resolves the role).
	if input.Role != nil && *input.Role != "" && *input.Role != member.Role {
		outletID := ""
		if input.OutletID != nil {
			outletID = *input.OutletID
		}
		if serr := h.syncAuthMemberRoles(r.Context(), tid, member.UserID, *input.Role, outletID); serr != nil {
			h.log.Warn("update staff: auth role sync failed (non-fatal)", zap.Error(serr), zap.String("staff_id", staffID.String()))
		}
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

	// Only an admin-level caller may deactivate admin or manager-tier staff (row role or the
	// person's POS role assignments).
	if !requesterIsAdminLevel(r) {
		priv, perr := effectiveStaffPrivileged(r.Context(), h.client, member)
		if perr != nil {
			h.log.Error("deactivate staff: read role assignments", zap.Error(perr))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		if priv {
			jsonError(w, "only an admin can deactivate admin or manager-level staff", http.StatusForbidden)
			return
		}
	}

	if err := h.client.StaffMember.UpdateOne(member).SetIsActive(false).Exec(r.Context()); err != nil {
		h.log.Error("deactivate staff", zap.Error(err))
		jsonError(w, "failed to deactivate staff member", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
