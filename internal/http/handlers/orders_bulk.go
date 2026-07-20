package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/poslinemodifier"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderevent"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
)

// bulkOrderIDCap bounds one bulk request so a single call can't hold a request-scoped
// transaction loop open indefinitely (mirrors the export caps elsewhere).
const bulkOrderIDCap = 200

// bulkSkippedItem is one order the bulk operation did NOT apply to, with a machine-readable
// reason: invalid_id | not_found | not_draft | not_owner | already_voided | finalized |
// internal_error. Skips are informational — a bulk call never fails because some ids were
// already gone (idempotent replays are expected from flaky back-office connections).
type bulkSkippedItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// ── pure decision logic (unit-tested without an ent client) ─────────────────────

// draftDeleteSkipReason returns a non-empty machine reason when this order may NOT be
// hard-deleted as a draft by the caller. Mirrors DeleteDraft's guards EXACTLY:
//   - only draft-status orders are deletable (a finalized sale carries ledger + KRA eTIMS
//     state and must be voided/returned instead) → "not_draft";
//   - pos.orders.manage (canDeleteAny) deletes ANY draft, any other order-write principal
//     only their OWN (order.user_id == caller) → "not_owner".
func draftDeleteSkipReason(status string, orderUserID, callerID uuid.UUID, canDeleteAny bool) string {
	if status != "draft" {
		return "not_draft"
	}
	if !canDeleteAny && orderUserID != callerID {
		return "not_owner"
	}
	return ""
}

// voidSkipReason returns a non-empty machine reason when an order in this status cannot be
// voided. Mirrors VoidOrder's guards EXACTLY: an already-voided order is a no-op
// ("already_voided"), and a finalized sale (completed/paid/closed) has been posted to the
// ledger and transmitted to KRA eTIMS — it must be reversed via a return/refund that posts
// the ledger reversal AND an eTIMS credit note, never a status flip ("finalized").
func voidSkipReason(status string) string {
	switch status {
	case "voided":
		return "already_voided"
	case "completed", "paid", "closed":
		return "finalized"
	}
	return ""
}

// ── shared side-effect helpers (used by the single and bulk endpoints) ──────────

// applyVoid performs the void side effects for ONE order exactly as the single VoidOrder
// endpoint does: status flip + voided_reason/by/at, voiding the order's still-open KDS
// tickets, and the centralized audit entry. Eligibility (voidSkipReason) must be checked
// by the caller first.
func (h *POSOrderHandler) applyVoid(ctx context.Context, tid uuid.UUID, order *ent.POSOrder, callerID uuid.UUID, approverID *uuid.UUID, reason string) (*ent.POSOrder, error) {
	now := time.Now()
	updated, err := order.Update().
		SetStatus("voided").
		SetVoidedReason(reason).
		SetVoidedBy(callerID).
		SetVoidedAt(now).
		Save(ctx)
	if err != nil {
		return nil, err
	}

	h.log.Info("order voided",
		zap.Stringer("order_id", order.ID),
		zap.Stringer("voided_by", callerID),
		zap.String("reason", reason),
	)

	// The kitchen must stop preparing a voided bill: void any of its still-open KDS tickets so
	// they drop off the live board (previously they lingered until staff voided each by hand).
	h.orderSvc.VoidKDSTicketsForOrder(ctx, tid, order.ID)

	if h.auditSvc != nil {
		oid := order.OutletID
		amt := order.TotalAmount
		h.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: callerID,
			ApproverID:  approverID,
			Action:      "order.void",
			EntityType:  "pos_order",
			EntityID:    order.ID.String(),
			Reason:      reason,
			Amount:      &amt,
			Before:      map[string]any{"status": order.Status, "order_number": order.OrderNumber},
			After:       map[string]any{"status": "voided"},
		})
	}
	return updated, nil
}

// hardDeleteDraft removes ONE draft order and all its child rows (line modifiers, lines,
// stray payments, events) in a single transaction — the exact deletion DeleteDraft performs.
// Eligibility (draftDeleteSkipReason) must be checked by the caller first.
func (h *POSOrderHandler) hardDeleteDraft(ctx context.Context, orderID uuid.UUID) error {
	tx, err := h.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	lineIDs, err := tx.POSOrderLine.Query().Where(posorderline.OrderID(orderID)).IDs(ctx)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("query lines: %w", err)
	}
	if len(lineIDs) > 0 {
		if _, err := tx.POSLineModifier.Delete().Where(poslinemodifier.LineIDIn(lineIDs...)).Exec(ctx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete modifiers: %w", err)
		}
	}
	if _, err := tx.POSOrderLine.Delete().Where(posorderline.OrderID(orderID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete lines: %w", err)
	}
	if _, err := tx.POSPayment.Delete().Where(pospayment.OrderID(orderID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete payments: %w", err)
	}
	if _, err := tx.POSOrderEvent.Delete().Where(posorderevent.OrderID(orderID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete events: %w", err)
	}
	if err := tx.POSOrder.DeleteOneID(orderID).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete order: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// recordDraftDeleted writes the audit-log entry for one hard-deleted draft (shared by the
// single and bulk delete endpoints).
func (h *POSOrderHandler) recordDraftDeleted(ctx context.Context, tid uuid.UUID, order *ent.POSOrder, callerID uuid.UUID) {
	h.log.Info("draft deleted",
		zap.Stringer("order_id", order.ID),
		zap.Stringer("deleted_by", callerID),
		zap.String("order_number", order.OrderNumber),
	)
	if h.auditSvc != nil {
		oid := order.OutletID
		amt := order.TotalAmount
		h.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: callerID,
			Action:      "order.draft_deleted",
			EntityType:  "pos_order",
			EntityID:    order.ID.String(),
			Amount:      &amt,
			Before:      map[string]any{"status": order.Status, "order_number": order.OrderNumber},
			After:       map[string]any{"deleted": true},
		})
	}
}

// ── bulk endpoints ──────────────────────────────────────────────────────────────

// BulkDeleteDrafts handles POST /{tenantID}/pos/orders/bulk-delete — permanently removes a
// batch of DRAFT (saved-but-unpaid / parked) sales. Per-order rules are IDENTICAL to the
// single DELETE /pos/orders/{orderID} (DeleteDraft): only drafts are deletable, and a caller
// without pos.orders.manage may delete only their OWN drafts. Idempotent: already-deleted /
// missing / ineligible ids are reported under "skipped" with a reason, never as an error, so
// a replayed bulk request converges instead of failing.
//
//	@Summary  Bulk-delete draft (parked) sales
//	@Tags     orders
//	@Accept   json
//	@Produce  json
//	@Param    body  body  bulkDeleteOrdersInput  true  "IDs of the draft orders to delete"
//	@Success  200  {object}  bulkDeleteOrdersResult
//	@Router   /{tenantID}/pos/orders/bulk-delete [post]
func (h *POSOrderHandler) BulkDeleteDrafts(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	var input bulkDeleteOrdersInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.OrderIDs) == 0 {
		jsonError(w, "order_ids is required", http.StatusBadRequest)
		return
	}
	if len(input.OrderIDs) > bulkOrderIDCap {
		jsonError(w, fmt.Sprintf("too many order_ids (max %d per request)", bulkOrderIDCap), http.StatusBadRequest)
		return
	}

	// Resolved ONCE for the whole batch — same RBAC boundary as the single DeleteDraft.
	canDeleteAny := outletmw.HasServicePermission(r, h.rbac, "pos.orders.manage")

	deleted := 0
	skipped := make([]bulkSkippedItem, 0)
	for _, raw := range input.OrderIDs {
		orderID, perr := uuid.Parse(raw)
		if perr != nil {
			skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: "invalid_id"})
			continue
		}
		order, qerr := h.client.POSOrder.Query().
			Where(posorder.ID(orderID), posorder.TenantID(tid)).
			Only(r.Context())
		if qerr != nil {
			if ent.IsNotFound(qerr) {
				// Already deleted (or never existed) — idempotent success, reported as a skip.
				skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: "not_found"})
				continue
			}
			h.log.Error("bulk delete drafts: query order failed", zap.Error(qerr))
			skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: "internal_error"})
			continue
		}
		if reason := draftDeleteSkipReason(order.Status, order.UserID, callerID, canDeleteAny); reason != "" {
			skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: reason})
			continue
		}
		if derr := h.hardDeleteDraft(r.Context(), orderID); derr != nil {
			h.log.Error("bulk delete drafts: delete failed", zap.Stringer("order_id", orderID), zap.Error(derr))
			skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: "internal_error"})
			continue
		}
		h.recordDraftDeleted(r.Context(), tid, order, callerID)
		deleted++
	}

	jsonOK(w, bulkDeleteOrdersResult{Deleted: deleted, Skipped: skipped})
}

// bulkDeleteOrdersInput is the body for POST /pos/orders/bulk-delete.
type bulkDeleteOrdersInput struct {
	OrderIDs []string `json:"order_ids"`
}

// bulkDeleteOrdersResult reports how many drafts were removed and which ids were skipped (and why).
type bulkDeleteOrdersResult struct {
	Deleted int               `json:"deleted"`
	Skipped []bulkSkippedItem `json:"skipped"`
}

// BulkVoidOrders handles POST /{tenantID}/pos/orders/bulk-void — voids a batch of unsettled
// orders in one back-office action. Route-gated to pos.orders.manage (a manager/admin
// authority — the bulk surface never needs the terminal step-up token because the caller IS
// the manager, exactly like the back-office single void). Per-order eligibility is IDENTICAL
// to the single PATCH /pos/orders/{orderID}/void: already-voided orders are skipped
// (idempotent) and finalized sales (completed/paid/closed) are skipped — those carry ledger +
// KRA eTIMS state and must be reversed via a return/refund instead.
//
//	@Summary  Bulk-void unsettled orders
//	@Tags     orders
//	@Accept   json
//	@Produce  json
//	@Param    body  body  bulkVoidOrdersInput  true  "IDs of the orders to void + the shared void reason"
//	@Success  200  {object}  bulkVoidOrdersResult
//	@Router   /{tenantID}/pos/orders/bulk-void [post]
func (h *POSOrderHandler) BulkVoidOrders(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	var input bulkVoidOrdersInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Reason == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}
	if len(input.OrderIDs) == 0 {
		jsonError(w, "order_ids is required", http.StatusBadRequest)
		return
	}
	if len(input.OrderIDs) > bulkOrderIDCap {
		jsonError(w, fmt.Sprintf("too many order_ids (max %d per request)", bulkOrderIDCap), http.StatusBadRequest)
		return
	}

	voided := 0
	skipped := make([]bulkSkippedItem, 0)
	for _, raw := range input.OrderIDs {
		orderID, perr := uuid.Parse(raw)
		if perr != nil {
			skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: "invalid_id"})
			continue
		}
		order, qerr := h.client.POSOrder.Query().
			Where(posorder.ID(orderID), posorder.TenantID(tid)).
			Only(r.Context())
		if qerr != nil {
			if ent.IsNotFound(qerr) {
				skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: "not_found"})
				continue
			}
			h.log.Error("bulk void orders: query order failed", zap.Error(qerr))
			skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: "internal_error"})
			continue
		}
		if reason := voidSkipReason(order.Status); reason != "" {
			skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: reason})
			continue
		}
		if _, verr := h.applyVoid(r.Context(), tid, order, callerID, nil, input.Reason); verr != nil {
			h.log.Error("bulk void orders: void failed", zap.Stringer("order_id", orderID), zap.Error(verr))
			skipped = append(skipped, bulkSkippedItem{ID: raw, Reason: "internal_error"})
			continue
		}
		voided++
	}

	jsonOK(w, bulkVoidOrdersResult{Voided: voided, Skipped: skipped})
}

// bulkVoidOrdersInput is the body for POST /pos/orders/bulk-void.
type bulkVoidOrdersInput struct {
	OrderIDs []string `json:"order_ids"`
	Reason   string   `json:"reason"`
}

// bulkVoidOrdersResult reports how many orders were voided and which ids were skipped (and why).
type bulkVoidOrdersResult struct {
	Voided  int               `json:"voided"`
	Skipped []bulkSkippedItem `json:"skipped"`
}
