package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/bengobox/pos-service/internal/ent"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
)

// PINAuthHandler handles terminal PIN login for cashier/waiter/kitchen staff.
// Staff must use SSO (PKCE) at least once so a StaffMember record exists;
// a manager then sets the PIN via SetPIN so the staff can log in offline.
type PINAuthHandler struct {
	log    *zap.Logger
	client *ent.Client
	// jwtSecret is the HMAC-SHA256 secret used to sign short-lived terminal JWTs.
	// Loaded from env TERMINAL_JWT_SECRET; falls back to INTERNAL_SERVICE_KEY if absent.
	jwtSecret []byte
}

func NewPINAuthHandler(log *zap.Logger, client *ent.Client, jwtSecret []byte) *PINAuthHandler {
	return &PINAuthHandler{log: log, client: client, jwtSecret: jwtSecret}
}

// maxFailedAttempts before lockout.
const maxFailedAttempts = 5

// lockoutDuration after maxFailedAttempts consecutive wrong PINs.
const lockoutDuration = 15 * time.Minute

// ── GET /{tenant}/pos/staff — list staff for the PIN selector UI ───────────────

// ListStaff returns minimal staff info for the PIN keypad selector screen.
// Does NOT include pin_hash — only name, user_id, has_pin.
func (h *PINAuthHandler) ListStaff(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	members, err := h.client.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.IsActive(true)).
		All(r.Context())
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
	jsonOK(w, map[string]any{"data": out, "total": len(out)})
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

	// Issue a short-lived terminal JWT (4 hours)
	token, err := issueTerminalJWT(member, tid, h.jwtSecret, h.client, r.Context())
	if err != nil {
		h.log.Error("failed to issue terminal JWT", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int((4 * time.Hour).Seconds()),
		"user": map[string]any{
			"user_id":   member.UserID.String(),
			"name":      member.Name,
			"role":      member.Role,
			"tenant_id": member.TenantID.String(),
			"outlet_id": member.OutletID.String(),
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

	if err := h.client.StaffMember.UpdateOne(member).
		SetPinHash(hashStr).
		SetPinFailedAttempts(0).
		ClearPinLockedUntil().
		Exec(r.Context()); err != nil {
		jsonError(w, "failed to set PIN", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── GET /{tenant}/pos/auth/pin/profile — return cached staff profiles ─────────
// Used by pos-ui to populate the offline PIN selector from IndexedDB.
// Returns name, user_id, roles/permissions (NO pin_hash).

func (h *PINAuthHandler) StaffProfiles(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	members, err := h.client.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.IsActive(true)).
		All(r.Context())
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
		out = append(out, profile{
			UserID:   m.UserID.String(),
			Name:     m.Name,
			Role:     m.Role,
			TenantID: m.TenantID.String(),
			OutletID: m.OutletID.String(),
			HasPIN:   m.PinHash != nil,
		})
	}
	jsonOK(w, map[string]any{"data": out})
}
