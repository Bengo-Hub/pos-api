package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entordervoidcode "github.com/bengobox/pos-service/internal/ent/ordervoidcode"
)

// Generic (non-order-scoped) manager approval codes — the SAME one-time-code mechanism as the
// order-scoped void/complimentary codes (OrderVoidCode table, bcrypt hash, single-use, TTL), reused
// for approvals that happen BEFORE an order exists: an over-limit discount / price override / order
// adjustment at checkout, or an out-of-stock override. A manager generates the code from their own
// login (present or remote); the cashier enters it in the ApprovalDialog "Code" tab.
//
// These carry a sentinel order_id = uuid.Nil (the column is NOT NULL) to mark "not tied to a
// specific order", so no schema/migration change is needed — the exact same generate/redeem
// machinery works, just scoped by tenant+outlet+action instead of tenant+order+action.

// actionApprovalCodes lists the pre-order actions a generic code may authorize. Gating on this set
// stops a manager minting an open-ended code for an unexpected action.
var actionApprovalCodes = map[string]bool{
	"order.discount_override": true,
	"order.adjustment":        true,
	"price.override":          true,
	"catalog.oos_override":    true,
}

type generateActionCodeInput struct {
	Action     string `json:"action"`
	OutletID   string `json:"outlet_id,omitempty"`
	TTLMinutes int    `json:"ttl_minutes,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// GenerateActionApprovalCode handles POST /{tenantID}/pos/approval-codes.
// Manager (override role) mints a one-time code authorizing a pre-order action. Mirrors
// generateOrderApprovalCode but without an order (sentinel order_id).
func (h *POSOrderHandler) GenerateActionApprovalCode(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	// Same authority the live PIN/card step-up requires — this is what makes the code trustworthy.
	if !claims.IsPlatformOwner && !hasOverrideRole(claims.Roles) {
		jsonError(w, "only a manager can authorize this action", http.StatusForbidden)
		return
	}
	approverID, _ := uuid.Parse(claims.Subject)

	var input generateActionCodeInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !actionApprovalCodes[input.Action] {
		jsonError(w, "unsupported approval action", http.StatusBadRequest)
		return
	}
	// A code MUST be pinned to the outlet it authorizes — a manager managing multiple branches
	// mints one per branch. Previously an empty/unparsable outlet_id silently produced a
	// NULL-outlet code that redeemActionApprovalCode treated as valid at ANY outlet tenant-wide,
	// which is how the live/PIN and code approval paths diverged (PIN correctly outlet-scoped,
	// code accidentally not).
	outletID, oerr := uuid.Parse(input.OutletID)
	if oerr != nil || outletID == uuid.Nil {
		jsonError(w, "outlet_id is required to generate an approval code", http.StatusBadRequest)
		return
	}

	code, err := randomNumericCode(6)
	if err != nil {
		h.log.Error("action-approval-code: rng failed", zap.String("action", input.Action), zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		h.log.Error("action-approval-code: hash failed", zap.String("action", input.Action), zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	ttl := voidCodeTTL
	if input.TTLMinutes > 0 && input.TTLMinutes <= 240 {
		ttl = time.Duration(input.TTLMinutes) * time.Minute
	}

	create := h.client.OrderVoidCode.Create().
		SetTenantID(tid).
		SetOrderID(uuid.Nil). // sentinel — not tied to a specific order
		SetAction(input.Action).
		SetCodeHash(string(hash)).
		SetApproverUserID(approverID).
		SetApproverName(claims.Email).
		SetOutletID(outletID).
		SetExpiresAt(time.Now().Add(ttl))
	if input.Reason != "" {
		create = create.SetReason(input.Reason)
	}
	if _, err := create.Save(r.Context()); err != nil {
		h.log.Error("action-approval-code: create failed", zap.String("action", input.Action), zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			ActorUserID: approverID,
			ApproverID:  &approverID,
			Action:      input.Action + "_code_issued",
			EntityType:  "pos_approval_code",
			Reason:      input.Reason,
		})
	}

	jsonOK(w, map[string]any{
		"code":          code,
		"action":        input.Action,
		"expires_at":    time.Now().Add(ttl).Format(time.RFC3339),
		"expires_in":    int(ttl.Seconds()),
		"approver_name": claims.Email,
	})
}

type verifyActionCodeInput struct {
	Action   string `json:"action"`
	Code     string `json:"code"`
	OutletID string `json:"outlet_id,omitempty"`
}

// VerifyActionApprovalCode handles POST /{tenantID}/pos/approval-codes/verify.
// Redeems (consumes) a generic code for a client-side gate that has no server action of its own —
// the out-of-stock override. Server actions with their own endpoint (order create's discount/price/
// adjustment) redeem inline instead, so the code is consumed exactly once by the real action.
func (h *POSOrderHandler) VerifyActionApprovalCode(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var input verifyActionCodeInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !actionApprovalCodes[input.Action] {
		jsonError(w, "unsupported approval action", http.StatusBadRequest)
		return
	}
	var outletID uuid.UUID
	if oid, perr := uuid.Parse(input.OutletID); perr == nil {
		outletID = oid
	}
	approver, ok := redeemActionApprovalCode(r.Context(), h.client, h.log, tid, outletID, input.Action, input.Code)
	if !ok {
		jsonError(w, "invalid or expired approval code", http.StatusForbidden)
		return
	}
	jsonOK(w, map[string]any{"approved": true, "approver_user_id": approver})
}

// redeemActionApprovalCode validates a one-time NON-order-scoped code (order_id = uuid.Nil) for a
// tenant+action+outlet, marks it single-use, and returns the approver. Same bcrypt/single-use
// semantics as redeemOrderApprovalCode — this is the reused verification, just keyed on the
// order-less sentinel instead of a specific order.
//
// The outlet match is EXACT and mandatory: a code only redeems at the outlet it was generated
// for (GenerateActionApprovalCode requires outlet_id, so every code on/after that fix carries
// one). A caller with no outlet context (outletID == uuid.Nil) can never redeem anything — there
// is deliberately no "tenant-wide" fallback here, since a manager physically at one branch
// authorizing a specific override must not have that same code usable at a different branch.
func redeemActionApprovalCode(ctx context.Context, client *ent.Client, log *zap.Logger, tid, outletID uuid.UUID, action, code string) (approver uuid.UUID, ok bool) {
	if code == "" || action == "" || outletID == uuid.Nil {
		return uuid.Nil, false
	}
	q := client.OrderVoidCode.Query().
		Where(
			entordervoidcode.TenantID(tid),
			entordervoidcode.OrderID(uuid.Nil),
			entordervoidcode.Action(action),
			entordervoidcode.OutletID(outletID),
			entordervoidcode.UsedAtIsNil(),
			entordervoidcode.ExpiresAtGT(time.Now()),
		)
	candidates, err := q.All(ctx)
	if err != nil {
		log.Warn("action-approval-code: lookup failed", zap.String("action", action), zap.Error(err))
		return uuid.Nil, false
	}
	for _, c := range candidates {
		if bcrypt.CompareHashAndPassword([]byte(c.CodeHash), []byte(code)) == nil {
			n, uerr := client.OrderVoidCode.Update().
				Where(entordervoidcode.ID(c.ID), entordervoidcode.UsedAtIsNil()).
				SetUsedAt(time.Now()).Save(ctx)
			if uerr != nil || n == 0 {
				return uuid.Nil, false
			}
			return c.ApproverUserID, true
		}
	}
	return uuid.Nil, false
}
