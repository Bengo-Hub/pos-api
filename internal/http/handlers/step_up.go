package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	entstaffoutlet "github.com/bengobox/pos-service/internal/ent/staffoutlet"
)

// overrideRoles are the POS roles permitted to approve sensitive actions
// (voids, discounts, price overrides, large refunds, post-send line removals).
var overrideRoles = map[string]bool{
	"admin": true, "manager": true, "pos_admin": true, "store_manager": true,
	"owner": true, "super_admin": true, "superuser": true,
}

// approvalClaims is the short-lived, single-action token issued after a manager
// PIN step-up and consumed by the sensitive-action handler.
type approvalClaims struct {
	Action        string `json:"action"`
	ApproverUser  string `json:"approver_user_id"`
	ApproverStaff string `json:"approver_staff_id"`
	OutletID      string `json:"outlet_id"`
	jwt.RegisteredClaims
}

// issueApprovalToken signs a ~2-minute HS256 token authorizing a single action.
func issueApprovalToken(action string, approverUserID, approverStaffID, outletID uuid.UUID, secret []byte) (string, error) {
	now := time.Now()
	claims := approvalClaims{
		Action:        action,
		ApproverUser:  approverUserID.String(),
		ApproverStaff: approverStaffID.String(),
		OutletID:      outletID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "pos-stepup",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Minute)),
			ID:        uuid.New().String(),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// verifyApprovalToken validates an approval token for the given action and
// returns the approver's user id. ok is false when the token is missing,
// invalid, expired, or issued for a different action.
func verifyApprovalToken(tokenStr, action string, secret []byte) (approverUserID uuid.UUID, ok bool) {
	if tokenStr == "" {
		return uuid.Nil, false
	}
	claims := &approvalClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return secret, nil
	})
	if err != nil || !token.Valid || claims.Action != action {
		return uuid.Nil, false
	}
	id, parseErr := uuid.Parse(claims.ApproverUser)
	if parseErr != nil {
		return uuid.Nil, false
	}
	return id, true
}

type stepUpInput struct {
	Action   string `json:"action"`
	PIN      string `json:"pin"`
	OutletID string `json:"outlet_id"`
}

// StepUp handles POST /{tenantID}/pos/auth/pin/step-up.
// A manager re-enters their PIN to authorize a single sensitive action; the
// response carries a short-lived approval_token the action handler consumes.
func (h *PINAuthHandler) StepUp(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var input stepUpInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.PIN == "" || input.OutletID == "" || input.Action == "" {
		jsonError(w, "action, pin and outlet_id are required", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	candidates, scanErr := h.client.StaffMember.Query().
		Where(
			entstaff.TenantID(tid),
			entstaff.IsActive(true),
			entstaff.PinHashNotNil(),
			entstaff.HasOutletsWith(entstaffoutlet.OutletID(outletID)),
		).
		All(r.Context())
	if scanErr != nil {
		h.log.Error("step-up: db error", zap.Error(scanErr))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	var member *ent.StaffMember
	for _, c := range candidates {
		if c.PinHash != nil && bcrypt.CompareHashAndPassword([]byte(*c.PinHash), []byte(input.PIN)) == nil {
			member = c
			break
		}
	}
	if member == nil {
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if !overrideRoles[member.Role] {
		jsonError(w, "this PIN is not authorized to approve", http.StatusForbidden)
		return
	}

	token, err := issueApprovalToken(input.Action, member.UserID, member.ID, outletID, h.jwtSecret)
	if err != nil {
		h.log.Error("step-up: token issue failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.auditSvc != nil {
		oid := outletID
		staffID := member.ID
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:     tid,
			OutletID:     &oid,
			ActorUserID:  member.UserID,
			ActorStaffID: &staffID,
			ApproverID:   &member.UserID,
			Action:       "auth.step_up",
			EntityType:   "approval",
			EntityID:     input.Action,
			Reason:       "manager approval for " + input.Action,
		})
	}

	jsonOK(w, map[string]any{
		"approval_token": token,
		"approver_name":  member.Name,
		"expires_in":     120,
	})
}
