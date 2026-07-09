package handlers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entordervoidcode "github.com/bengobox/pos-service/internal/ent/ordervoidcode"
	"github.com/bengobox/pos-service/internal/ent/posorder"
)

// voidCodeTTL is how long a manager-generated one-time approval code stays valid (void,
// complimentary, or any future sensitive action sharing this mechanism).
const voidCodeTTL = 30 * time.Minute

// generateVoidCodeInput is the manager's request to authorize a sensitive action on a specific
// order. Reused for every action generateOrderApprovalCode issues a code for.
type generateVoidCodeInput struct {
	Reason     string `json:"reason,omitempty"`
	TTLMinutes int    `json:"ttl_minutes,omitempty"`
}

// approvalStatusError lets an order-status guard specify the HTTP status code the generalized
// generateOrderApprovalCode helper should respond with (guards default to 400 otherwise).
type approvalStatusError struct {
	msg  string
	code int
}

func (e *approvalStatusError) Error() string { return e.msg }

func newApprovalStatusError(code int, msg string) error {
	return &approvalStatusError{msg: msg, code: code}
}

// GenerateVoidCode handles POST /{tenantID}/pos/orders/{orderID}/void-code.
// A manager (an override role, enforced by the route's pos.orders.void gate) generates a one-time,
// order-scoped code they can SHARE with a waiter/cashier to authorize voiding this bill when the
// manager is not physically present to scan a card or enter a PIN. The plaintext code is returned
// once; only its hash is stored.
func (h *POSOrderHandler) GenerateVoidCode(w http.ResponseWriter, r *http.Request) {
	h.generateOrderApprovalCode(w, r, "order.void", func(order *ent.POSOrder) error {
		switch order.Status {
		case "voided":
			return newApprovalStatusError(http.StatusBadRequest, "order is already voided")
		case "completed", "paid", "closed":
			return newApprovalStatusError(http.StatusConflict, "a finalized sale cannot be voided — issue a refund/return instead")
		}
		return nil
	})
}

// GenerateComplimentaryCode handles POST /{tenantID}/pos/orders/{orderID}/complimentary-code.
// A manager generates a one-time, order-scoped code a cashier can use to authorize closing THIS
// bill via the Complimentary/no-charge tender when the manager isn't physically at the terminal
// to step up with a PIN/card (the same "manager not around" alternative Void already has).
// Manager-only (handler re-checks role — same authority the void code requires).
func (h *POSOrderHandler) GenerateComplimentaryCode(w http.ResponseWriter, r *http.Request) {
	h.generateOrderApprovalCode(w, r, "order.complimentary", func(order *ent.POSOrder) error {
		switch order.Status {
		case "voided":
			return newApprovalStatusError(http.StatusBadRequest, "order is voided")
		case "completed", "paid", "closed":
			return newApprovalStatusError(http.StatusConflict, "this order is already settled")
		}
		return nil
	})
}

// generateOrderApprovalCode is the shared one-time-code issuance logic behind GenerateVoidCode and
// GenerateComplimentaryCode (and any future manager-approved sensitive action). action scopes the
// stored code (OrderVoidCode.Action) so a void code can never redeem a complimentary sale or vice
// versa; statusGuard lets each action reject orders in the wrong state before minting a code.
func (h *POSOrderHandler) generateOrderApprovalCode(w http.ResponseWriter, r *http.Request, action string, statusGuard func(*ent.POSOrder) error) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order id", http.StatusBadRequest)
		return
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	// Only a manager (override role) may authorize this action — same authority the live PIN/card
	// step-up requires. This is what makes the shared code trustworthy.
	if !claims.IsPlatformOwner && !hasOverrideRole(claims.Roles) {
		jsonError(w, "only a manager can authorize this action", http.StatusForbidden)
		return
	}
	approverID, _ := uuid.Parse(claims.Subject)

	var input generateVoidCodeInput
	_ = json.NewDecoder(r.Body).Decode(&input) // body is optional

	// Confirm the order exists and belongs to the tenant, and let the caller's status guard
	// confirm it's still in a state this action can apply to.
	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	if statusGuard != nil {
		if gerr := statusGuard(order); gerr != nil {
			code := http.StatusBadRequest
			if se, ok := gerr.(*approvalStatusError); ok {
				code = se.code
			}
			jsonError(w, gerr.Error(), code)
			return
		}
	}

	code, err := randomNumericCode(6)
	if err != nil {
		h.log.Error("approval-code: rng failed", zap.String("action", action), zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		h.log.Error("approval-code: hash failed", zap.String("action", action), zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	ttl := voidCodeTTL
	if input.TTLMinutes > 0 && input.TTLMinutes <= 240 {
		ttl = time.Duration(input.TTLMinutes) * time.Minute
	}
	approverName := claims.Email

	create := h.client.OrderVoidCode.Create().
		SetTenantID(tid).
		SetOrderID(orderID).
		SetAction(action).
		SetCodeHash(string(hash)).
		SetApproverUserID(approverID).
		SetApproverName(approverName).
		SetExpiresAt(time.Now().Add(ttl))
	if order.OutletID != uuid.Nil {
		create = create.SetOutletID(order.OutletID)
	}
	if input.Reason != "" {
		create = create.SetReason(input.Reason)
	}
	if _, err := create.Save(r.Context()); err != nil {
		h.log.Error("approval-code: create failed", zap.String("action", action), zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.auditSvc != nil {
		oid := order.OutletID
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: approverID,
			ApproverID:  &approverID,
			Action:      action + "_code_issued",
			EntityType:  "pos_order",
			EntityID:    orderID.String(),
			Reason:      input.Reason,
		})
	}

	jsonOK(w, map[string]any{
		"code":          code,
		"order_number":  order.OrderNumber,
		"expires_at":    time.Now().Add(ttl).Format(time.RFC3339),
		"expires_in":    int(ttl.Seconds()),
		"approver_name": approverName,
	})
}

// redeemVoidCode validates a one-time "order.void" code — kept as a thin wrapper around the
// generalized redeemOrderApprovalCode so the existing VoidOrder call site is unchanged.
func (h *POSOrderHandler) redeemVoidCode(ctx context.Context, tid, orderID uuid.UUID, code string) (approver uuid.UUID, ok bool) {
	return redeemOrderApprovalCode(ctx, h.client, h.log, tid, orderID, "order.void", code)
}

// redeemOrderApprovalCode validates a one-time code for a specific order+action, marks it used
// (single-use), and returns the approver's user id. ok is false when no matching, unexpired,
// unused code for that exact action verifies — a void code can never redeem a complimentary sale
// or vice versa. A free function (not a method) so both POSOrderHandler (void) and PaymentHandler
// (complimentary) can call it without depending on each other's handler type.
func redeemOrderApprovalCode(ctx context.Context, client *ent.Client, log *zap.Logger, tid, orderID uuid.UUID, action, code string) (approver uuid.UUID, ok bool) {
	if code == "" {
		return uuid.Nil, false
	}
	candidates, err := client.OrderVoidCode.Query().
		Where(
			entordervoidcode.TenantID(tid),
			entordervoidcode.OrderID(orderID),
			entordervoidcode.Action(action),
			entordervoidcode.UsedAtIsNil(),
			entordervoidcode.ExpiresAtGT(time.Now()),
		).
		All(ctx)
	if err != nil {
		log.Warn("approval-code: lookup failed", zap.String("action", action), zap.Error(err))
		return uuid.Nil, false
	}
	for _, c := range candidates {
		if bcrypt.CompareHashAndPassword([]byte(c.CodeHash), []byte(code)) == nil {
			// Mark used (single-use). Best-effort — even if the stamp fails, the action proceeds
			// once; a concurrent second redemption is guarded by re-checking UsedAtIsNil below.
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

// randomNumericCode returns a cryptographically-random n-digit numeric string (zero-padded).
func randomNumericCode(n int) (string, error) {
	max := big.NewInt(1)
	for i := 0; i < n; i++ {
		max.Mul(max, big.NewInt(10))
	}
	v, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", n, v), nil
}

// ensure ent import is referenced even if the file evolves.
var _ = (*ent.OrderVoidCode)(nil)
