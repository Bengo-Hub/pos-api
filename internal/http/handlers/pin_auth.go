package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	entstaffoutlet "github.com/bengobox/pos-service/internal/ent/staffoutlet"
	"github.com/bengobox/pos-service/internal/modules/rbac"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// pinFastHash computes hex(SHA256(tenantID+":"+userID+":"+pin)) for O(1) PIN lookup.
// Scoped to tenant+user so a single PIN works across all outlets the user is assigned to.
func pinFastHash(tenantID, userID uuid.UUID, pin string) string {
	h := sha256.Sum256([]byte(tenantID.String() + ":" + userID.String() + ":" + pin))
	return fmt.Sprintf("%x", h)
}

// useCaseRoles maps a POS outlet use case to the staff roles that make sense
// at that type of terminal. Only these roles appear in the PIN login staff grid
// when an outlet_id query param is provided.
var useCaseRoles = map[string][]string{
	"hospitality":   {"manager", "cashier", "waiter", "barista", "kitchen", "bar", "receptionist"},
	"quick_service": {"manager", "cashier", "barista", "kitchen"},
	"retail":        {"manager", "cashier"},
	"pharmacy":      {"manager", "cashier", "receptionist", "pharmacist"},
	"services":      {"manager", "cashier", "receptionist"},
}

// PINAuthHandler handles terminal PIN login for cashier/waiter/kitchen staff.
// Staff must use SSO (PKCE) at least once so a StaffMember record exists;
// a manager then sets the PIN via SetPIN so the staff can log in offline.
type PINAuthHandler struct {
	log    *zap.Logger
	client *ent.Client
	// jwtSecret is the HMAC-SHA256 secret used to sign short-lived terminal JWTs.
	// Loaded from env TERMINAL_JWT_SECRET; falls back to INTERNAL_SERVICE_KEY if absent.
	jwtSecret []byte
	// subsClient fetches the tenant's subscription so the terminal JWT carries the same
	// entitlements as an SSO session (features/limits/status + demo/owner bypass flags).
	subsClient *subscriptions.Client
	auditSvc   *audit.Service
}

func NewPINAuthHandler(log *zap.Logger, client *ent.Client, jwtSecret []byte, subsClient *subscriptions.Client) *PINAuthHandler {
	return &PINAuthHandler{log: log, client: client, jwtSecret: jwtSecret, subsClient: subsClient}
}

// SetAuditService wires the centralized audit trail (manager step-up events).
func (h *PINAuthHandler) SetAuditService(a *audit.Service) {
	h.auditSvc = a
}

// resolveTerminalEntitlements builds the subscription snapshot + bypass flags embedded in
// the terminal JWT. It derives demo / platform-owner from the local tenant slug (works even
// if subscriptions-api is down) and fetches features/limits/status from subscriptions-api.
func (h *PINAuthHandler) resolveTerminalEntitlements(ctx context.Context, tenantID uuid.UUID) terminalEntitlements {
	te := terminalEntitlements{}
	if t, err := h.client.Tenant.Get(ctx, tenantID); err == nil {
		te.TenantSlug = t.Slug
		te.IsPlatformOwner = t.Slug == "codevertex"
		te.IsDemo = t.Slug == "codevertex-demo"
	}
	if h.subsClient != nil {
		if e := h.subsClient.GetEntitlements(ctx, tenantID.String()); e != nil {
			te.Features = e.Features
			te.Limits = e.Limits
			te.Status = e.Status
			te.BillingMode = e.BillingMode
			if e.IsDemoBypass {
				te.IsDemo = true
			}
		}
	}
	return te
}

// maxFailedAttempts before lockout.
const maxFailedAttempts = 5

// lockoutDuration after maxFailedAttempts consecutive wrong PINs.
const lockoutDuration = 15 * time.Minute

// ── GET /{tenant}/pos/staff — list staff for the PIN selector UI ───────────────

// ListStaff returns minimal staff info for the PIN keypad selector screen.
// Does NOT include pin_hash — only name, user_id, has_pin.
// When ?outlet_id= is provided, the result is filtered to roles appropriate for
// that outlet's use case (e.g. pharmacy shows manager/cashier/receptionist/pharmacist only).
func (h *PINAuthHandler) ListStaff(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.StaffMember.Query().Where(entstaff.TenantID(tid), entstaff.IsActive(true))

	if outletIDStr := r.URL.Query().Get("outlet_id"); outletIDStr != "" {
		if outletUUID, err := uuid.Parse(outletIDStr); err == nil {
			if o, err := h.client.Outlet.Query().Where(entoutlet.ID(outletUUID)).Only(r.Context()); err == nil && o.UseCase != nil {
				if allowed, ok := useCaseRoles[*o.UseCase]; ok {
					q = q.Where(entstaff.RoleIn(allowed...))
				}
			}
		}
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	members, err := q.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type staffItem struct {
		UserID string `json:"user_id"`
		Name   string `json:"name"`
		Role   string `json:"role"`
		HasPIN bool   `json:"has_pin"`
	}
	out := make([]staffItem, 0, len(members))
	for _, m := range members {
		out = append(out, staffItem{
			UserID: m.UserID.String(),
			Name:   m.Name,
			Role:   m.Role,
			HasPIN: m.PinHash != nil,
		})
	}
	jsonOK(w, pagination.NewResponse(out, total, p))
}

// ── POST /{tenant}/pos/auth/pin — validate PIN, return terminal JWT ────────────

type pinLoginInput struct {
	UserID string `json:"user_id"`
	PIN    string `json:"pin"` // 4-6 digit string (never stored raw)
}

func (h *PINAuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input pinLoginInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.UserID == "" || input.PIN == "" {
		jsonError(w, "user_id and pin are required", http.StatusBadRequest)
		return
	}

	userID, err := uuid.Parse(input.UserID)
	if err != nil {
		jsonError(w, "invalid user_id", http.StatusBadRequest)
		return
	}

	member, err := h.client.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.UserID(userID), entstaff.IsActive(true)).
		Only(r.Context())
	if err != nil {
		// Return 401 to avoid user enumeration
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	if member.PinHash == nil {
		jsonError(w, "PIN not configured for this staff member", http.StatusUnauthorized)
		return
	}

	// Check lockout
	if member.PinLockedUntil != nil && time.Now().Before(*member.PinLockedUntil) {
		remaining := time.Until(*member.PinLockedUntil).Round(time.Second)
		jsonError(w, fmt.Sprintf("PIN locked. Try again in %s", remaining), http.StatusTooManyRequests)
		return
	}

	// Validate bcrypt
	if err := bcrypt.CompareHashAndPassword([]byte(*member.PinHash), []byte(input.PIN)); err != nil {
		attempts := member.PinFailedAttempts + 1
		upd := h.client.StaffMember.UpdateOne(member).SetPinFailedAttempts(attempts)
		if attempts >= maxFailedAttempts {
			locked := time.Now().Add(lockoutDuration)
			upd = upd.SetPinLockedUntil(locked)
			h.log.Warn("PIN login locked after failed attempts",
				zap.String("user_id", userID.String()), zap.Int("attempts", attempts))
		}
		_ = upd.Exec(r.Context())
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	// Reset failed attempts on success
	_ = h.client.StaffMember.UpdateOne(member).
		SetPinFailedAttempts(0).
		ClearPinLockedUntil().
		Exec(r.Context())

	// Determine session outlet: prefer the outlet the terminal selected (X-Outlet-ID),
	// fall back to the staff member's home StaffOutlet.
	var sessionOutletID uuid.UUID
	if xOID := r.Header.Get("X-Outlet-ID"); xOID != "" {
		if parsed, err := uuid.Parse(xOID); err == nil {
			sessionOutletID = parsed
		}
	}
	if sessionOutletID == uuid.Nil {
		if so, soErr := h.client.StaffOutlet.Query().
			Where(entstaffoutlet.StaffMemberID(member.ID), entstaffoutlet.IsHomeOutlet(true)).
			First(r.Context()); soErr == nil {
			sessionOutletID = so.OutletID
		}
	}

	// Resolve subscription entitlements + bypass flags so the terminal JWT gates exactly
	// like an SSO session (and exempts demo / platform-owner tenants).
	subEnt := h.resolveTerminalEntitlements(r.Context(), tid)

	// Issue a short-lived terminal JWT (4 hours)
	token, err := issueTerminalJWT(member, tid, sessionOutletID, h.jwtSecret, h.client, r.Context(), subEnt)
	if err != nil {
		h.log.Error("failed to issue terminal JWT", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Load outlet to include use_case and is_hq in the login response so pos-ui
	// can initialise outlet state without an extra round-trip.
	outletUseCase := "hospitality"
	isHQ := false
	outlet, outletErr := h.client.Outlet.Get(r.Context(), sessionOutletID)
	if outletErr == nil && outlet.UseCase != nil {
		outletUseCase = *outlet.UseCase
		isHQ = outlet.IsHq
	}

	jsonOK(w, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int((4 * time.Hour).Seconds()),
		"user": map[string]any{
			"user_id":         member.UserID.String(),
			"name":            member.Name,
			"role":            member.Role,
			"tenant_id":       member.TenantID.String(),
			"tenant_slug":     subEnt.TenantSlug,
			"outlet_id":       sessionOutletID.String(),
			"outlet_use_case": outletUseCase,
			"is_hq_user":      isHQ,
			// Surface the bypass flags so pos-ui's useSubscription exempts demo/platform
			// PIN sessions (it derives isDemo/isPlatformOwner from the user object).
			"is_demo":           subEnt.IsDemo,
			"is_platform_owner": subEnt.IsPlatformOwner,
		},
	})
}

// ── POST /{tenant}/pos/auth/pin/set — manager sets a staff PIN ────────────────

type setPINInput struct {
	UserID string `json:"user_id"`
	PIN    string `json:"pin"` // 4-6 digits
}

func (h *PINAuthHandler) SetPIN(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input setPINInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.UserID == "" || len(input.PIN) < 4 {
		jsonError(w, "user_id and pin (min 4 digits) are required", http.StatusBadRequest)
		return
	}

	userID, err := uuid.Parse(input.UserID)
	if err != nil {
		jsonError(w, "invalid user_id", http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(input.PIN), bcrypt.DefaultCost)
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	hashStr := string(hash)

	member, err := h.client.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.UserID(userID)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "staff member not found", http.StatusNotFound)
		return
	}

	fastHash := pinFastHash(tid, member.UserID, input.PIN)

	if err := h.client.StaffMember.UpdateOne(member).
		SetPinHash(hashStr).
		SetPinFastHash(fastHash).
		SetPinFailedAttempts(0).
		ClearPinLockedUntil().
		Exec(r.Context()); err != nil {
		jsonError(w, "failed to set PIN", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── GET /{tenant}/pos/auth/me — service-level identity enrichment ─────────────
// Called by pos-ui after SSO callback to get POS-specific role + permissions.
// Maps global JWT roles (admin, cashier, etc.) to local POS service roles and
// resolves fine-grained pos.*.* permissions from POSRoleV2 table.

func (h *PINAuthHandler) AuthMe(w http.ResponseWriter, r *http.Request) {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	uid, uidErr := uuid.Parse(claims.Subject)
	if uidErr != nil {
		jsonError(w, "invalid user_id in token", http.StatusBadRequest)
		return
	}

	// Resolve POS role: prefer local StaffMember record, fall back to JWT role mapping
	var posRole, displayName string
	member, memberErr := h.client.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.UserID(uid)).
		Only(r.Context())
	if memberErr == nil {
		posRole = member.Role
		displayName = member.Name
	} else {
		posRole = rbac.MapGlobalRolesToServiceRole(claims.Roles)
		displayName = claims.Email
	}

	perms := resolveRolePermissions(r.Context(), h.client, tid, posRole)

	jsonOK(w, map[string]any{
		"user_id":      claims.Subject,
		"email":        claims.Email,
		"name":         displayName,
		"tenant_id":    claims.TenantID,
		"tenant_slug":  claims.GetTenantSlug(),
		"global_roles": claims.Roles,
		"pos_role":     posRole,
		"permissions":  perms,
	})
}

// Global→POS role mapping lives in rbac.MapGlobalRolesToServiceRole so /auth/me and the
// permission middleware's role-mapped fallback always resolve identically.

// ── POST /{tenant}/pos/auth/pin/identify — PIN-first login (no user_id needed) ──
// The frontend sends only the PIN + outlet_id; we resolve the staff identity via
// pin_fast_hash index, then verify bcrypt before issuing a terminal JWT.

type identifyByPINInput struct {
	PIN      string `json:"pin"`
	OutletID string `json:"outlet_id"`
}

func (h *PINAuthHandler) IdentifyByPIN(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input identifyByPINInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.PIN == "" || input.OutletID == "" {
		jsonError(w, "pin and outlet_id are required", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	// Scan active staff assigned to the requested outlet and bcrypt-compare.
	// PIN uniqueness is per-outlet, so we only compare staff at this outlet.
	candidates, scanErr := h.client.StaffMember.Query().
		Where(
			entstaff.TenantID(tid),
			entstaff.IsActive(true),
			entstaff.PinHashNotNil(),
			entstaff.HasOutletsWith(entstaffoutlet.OutletID(outletID)),
		).
		All(r.Context())
	if scanErr != nil {
		h.log.Error("pin identify: db error", zap.Error(scanErr))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	var member *ent.StaffMember
	for _, c := range candidates {
		if bcrypt.CompareHashAndPassword([]byte(*c.PinHash), []byte(input.PIN)) == nil {
			member = c
			// Backfill pin_fast_hash (user-scoped) for the direct-auth endpoint.
			fastHash := pinFastHash(tid, c.UserID, input.PIN)
			_ = h.client.StaffMember.UpdateOne(c).SetPinFastHash(fastHash).Exec(r.Context())
			break
		}
	}
	if member == nil {
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	if member.PinHash == nil {
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	// Check lockout
	if member.PinLockedUntil != nil && time.Now().Before(*member.PinLockedUntil) {
		remaining := time.Until(*member.PinLockedUntil).Round(time.Second)
		jsonError(w, fmt.Sprintf("PIN locked. Try again in %s", remaining), http.StatusTooManyRequests)
		return
	}

	// Bcrypt verify (primary on fast-hash path, redundant but kept for defence-in-depth)
	if err := bcrypt.CompareHashAndPassword([]byte(*member.PinHash), []byte(input.PIN)); err != nil {
		attempts := member.PinFailedAttempts + 1
		upd := h.client.StaffMember.UpdateOne(member).SetPinFailedAttempts(attempts)
		if attempts >= maxFailedAttempts {
			locked := time.Now().Add(lockoutDuration)
			upd = upd.SetPinLockedUntil(locked)
		}
		_ = upd.Exec(r.Context())
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	// Reset failed attempts on success
	_ = h.client.StaffMember.UpdateOne(member).
		SetPinFailedAttempts(0).
		ClearPinLockedUntil().
		Exec(r.Context())

	// Issue terminal JWT — same shape as Login (carry subscription entitlements + bypass).
	subEnt := h.resolveTerminalEntitlements(r.Context(), tid)
	token, err := issueTerminalJWT(member, tid, outletID, h.jwtSecret, h.client, r.Context(), subEnt)
	if err != nil {
		h.log.Error("failed to issue terminal JWT", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	outletUseCase := "hospitality"
	isHQ := false
	outlet, outletErr := h.client.Outlet.Get(r.Context(), outletID)
	if outletErr == nil && outlet.UseCase != nil {
		outletUseCase = *outlet.UseCase
		isHQ = outlet.IsHq
	}

	// pin_hash is returned ONLY here, to the device the staff member just authenticated on,
	// so it can be cached for offline PIN re-login. It is never exposed by the public staff
	// list (/auth/pin/profile). A 4-digit PIN's bcrypt hash is brute-forceable, so this trades
	// a little secrecy for offline capability — acceptable since exposure is limited to users
	// who have physically signed in on this terminal.
	pinHash := ""
	if member.PinHash != nil {
		pinHash = *member.PinHash
	}
	jsonOK(w, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int((4 * time.Hour).Seconds()),
		"user": map[string]any{
			"user_id":         member.UserID.String(),
			"name":            member.Name,
			"role":            member.Role,
			"tenant_id":       member.TenantID.String(),
			"outlet_id":       outletID.String(),
			"outlet_use_case": outletUseCase,
			"is_hq_user":      isHQ,
			"pin_hash":        pinHash,
		},
	})
}

// ── GET /{tenant}/pos/auth/pin/profile — return staff profiles for PIN selector ─
// Used by pos-ui to populate the PIN selector from IndexedDB for offline fallback.
// Returns name, user_id, roles/permissions (NO pin_hash).
// When ?outlet_id= is provided, result is filtered to use-case-appropriate roles.
func (h *PINAuthHandler) StaffProfiles(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.StaffMember.Query().Where(entstaff.TenantID(tid), entstaff.IsActive(true))

	if outletIDStr := r.URL.Query().Get("outlet_id"); outletIDStr != "" {
		if outletUUID, err := uuid.Parse(outletIDStr); err == nil {
			if o, err := h.client.Outlet.Query().Where(entoutlet.ID(outletUUID)).Only(r.Context()); err == nil && o.UseCase != nil {
				if allowed, ok := useCaseRoles[*o.UseCase]; ok {
					q = q.Where(entstaff.RoleIn(allowed...))
				}
			}
		}
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	members, err := q.WithOutlets(func(soq *ent.StaffOutletQuery) {
		soq.Where(entstaffoutlet.IsHomeOutlet(true)).Limit(1)
	}).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type profile struct {
		UserID   string `json:"user_id"`
		Name     string `json:"name"`
		Role     string `json:"role"`
		TenantID string `json:"tenant_id"`
		OutletID string `json:"outlet_id"`
		HasPIN   bool   `json:"has_pin"`
	}
	out := make([]profile, 0, len(members))
	for _, m := range members {
		homeOutletID := ""
		if len(m.Edges.Outlets) > 0 {
			homeOutletID = m.Edges.Outlets[0].OutletID.String()
		}
		out = append(out, profile{
			UserID:   m.UserID.String(),
			Name:     m.Name,
			Role:     m.Role,
			TenantID: m.TenantID.String(),
			OutletID: homeOutletID,
			HasPIN:   m.PinHash != nil,
		})
	}
	jsonOK(w, pagination.NewResponse(out, total, p))
}
