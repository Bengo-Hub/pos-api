package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	enthelditem "github.com/bengobox/pos-service/internal/ent/helditem"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
)

// SetAsideLine handles POST /{tenantID}/pos/orders/{orderID}/lines/{lineID}/set-aside.
// "Upsell / set aside": the item was already made but ordered by mistake (e.g. Mango Juice instead
// of Orange Juice). Instead of voiding it (which needs manager approval and wastes the item), the
// waiter sets it aside into a per-outlet held-items pool. It's removed from THIS bill and held for
// a future customer who wants exactly that item. Unclaimed held items must be voided before the
// waiter closes their shift (enforced in CloseSession). No manager approval is needed to set aside.
func (h *POSOrderHandler) SetAsideLine(w http.ResponseWriter, r *http.Request) {
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
	lineID, err := uuid.Parse(chi.URLParam(r, "lineID"))
	if err != nil {
		jsonError(w, "invalid line id", http.StatusBadRequest)
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&input)

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	order, err := h.client.POSOrder.Query().Where(posorder.ID(orderID), posorder.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	if order.Status == "voided" || order.Status == "completed" {
		jsonError(w, "cannot modify a "+order.Status+" order", http.StatusBadRequest)
		return
	}
	line, err := h.client.POSOrderLine.Query().Where(posorderline.ID(lineID), posorderline.OrderID(orderID)).Only(r.Context())
	if err != nil {
		jsonError(w, "line not found", http.StatusNotFound)
		return
	}
	if line.VoidedQty != nil {
		jsonError(w, "line already removed", http.StatusBadRequest)
		return
	}

	unit := line.UnitPrice
	if unit == 0 && line.Quantity > 0 {
		unit = line.TotalPrice / line.Quantity
	}

	// Remove the line from the bill (reuse the soft-void so it stays auditable + totals adjust).
	now := time.Now()
	if _, err := line.Update().
		SetVoidedQty(line.Quantity).
		SetVoidedReason("set_aside: " + input.Reason).
		SetVoidedBy(callerID).
		SetVoidedAt(now).
		Save(r.Context()); err != nil {
		h.log.Error("set-aside: void line failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	newSubtotal := order.Subtotal - line.TotalPrice
	if newSubtotal < 0 {
		newSubtotal = 0
	}
	newTotal := order.TotalAmount - line.TotalPrice
	if newTotal < 0 {
		newTotal = 0
	}
	if _, err := order.Update().SetSubtotal(newSubtotal).SetTotalAmount(newTotal).Save(r.Context()); err != nil {
		h.log.Error("set-aside: adjust totals failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Create the held item. Stamp the caller's open shift session (best-effort) so the shift-close
	// guard can flag unclaimed items.
	create := h.client.HeldItem.Create().
		SetTenantID(tid).
		SetOutletID(order.OutletID).
		SetSourceOrderID(orderID).
		SetSourceLineID(lineID).
		SetName(line.Name).
		SetSku(line.Sku).
		SetQuantity(line.Quantity).
		SetUnitPrice(unit).
		SetReason(input.Reason).
		SetHeldByUserID(callerID)
	if line.CatalogItemID != uuid.Nil {
		create = create.SetCatalogItemID(line.CatalogItemID.String())
	}
	if sess, sErr := h.client.POSDeviceSession.Query().
		Where(posdevicesession.TenantID(tid), posdevicesession.UserID(callerID), posdevicesession.SessionStatus("open")).
		Order(ent.Desc(posdevicesession.FieldOpenedAt)).First(r.Context()); sErr == nil {
		create = create.SetShiftSessionID(sess.ID)
	}
	held, err := create.Save(r.Context())
	if err != nil {
		h.log.Error("set-aside: create held item failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.auditSvc != nil {
		oid := order.OutletID
		amt := line.TotalPrice
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID: tid, OutletID: &oid, ActorUserID: callerID,
			Action: "order.line_set_aside", EntityType: "pos_order_line", EntityID: lineID.String(),
			Reason: input.Reason, Amount: &amt,
			Before: map[string]any{"sku": line.Sku, "name": line.Name, "quantity": line.Quantity},
		})
	}

	jsonOK(w, map[string]any{"held_item": held})
}

// ListHeldItems handles GET /{tenantID}/pos/held-items?status=held&outlet_id=&mine=true
// Lists the outlet's held (set-aside) items so a waiter can claim one for a new customer or void
// unclaimed ones. Defaults to status=held.
func (h *POSOrderHandler) ListHeldItems(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.client.HeldItem.Query().Where(enthelditem.TenantID(tid))
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "held"
	}
	q = q.Where(enthelditem.Status(status))
	if outletParam := r.URL.Query().Get("outlet_id"); outletParam != "" {
		if oid, perr := uuid.Parse(outletParam); perr == nil {
			q = q.Where(enthelditem.OutletID(oid))
		}
	} else if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			q = q.Where(enthelditem.OutletID(oid))
		}
	}
	if r.URL.Query().Get("mine") == "true" {
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			if uid, perr := uuid.Parse(claims.Subject); perr == nil {
				q = q.Where(enthelditem.HeldByUserID(uid))
			}
		}
	}
	items, err := q.Order(ent.Desc(enthelditem.FieldCreatedAt)).Limit(200).All(r.Context())
	if err != nil {
		h.log.Error("list held items failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": items, "total": len(items)})
}

// ClaimHeldItem handles POST /{tenantID}/pos/held-items/{id}/claim.
// A customer at another (or the same) table wants the set-aside item: the claim MERGES it into
// their active order — a real order line is appended to the target order and its totals grow by
// the line price. No KDS ticket fires (the item is already prepared; only the AddLines service
// path fires KDS). Mirrors SetAsideLine's price-only totals adjustment, so tax stays booked on
// the source order and the two sides net out.
func (h *POSOrderHandler) ClaimHeldItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid held item id", http.StatusBadRequest)
		return
	}
	var input struct {
		Reason         string `json:"reason,omitempty"`
		ClaimedOrderID string `json:"claimed_order_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&input)
	targetOrderID, perr := uuid.Parse(input.ClaimedOrderID)
	if input.ClaimedOrderID == "" || perr != nil {
		jsonErrorWithCode(w, "claimed_order_id is required — pick the order the item is merged into", "claimed_order_required", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	held, err := h.client.HeldItem.Query().Where(enthelditem.ID(id), enthelditem.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "held item not found", http.StatusNotFound)
		return
	}
	if held.Status != "held" {
		jsonError(w, "held item is already "+held.Status, http.StatusConflict)
		return
	}

	order, err := h.client.POSOrder.Query().Where(posorder.ID(targetOrderID), posorder.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "target order not found", http.StatusNotFound)
		return
	}
	// The item is physically at THIS outlet — it can only be merged into an order there.
	if order.OutletID != held.OutletID {
		jsonError(w, "target order belongs to a different outlet", http.StatusBadRequest)
		return
	}
	if order.Status != "draft" && order.Status != "open" {
		jsonErrorWithCode(w, "target order is "+order.Status+" — items can only be merged into an open order", "order_not_open", http.StatusBadRequest)
		return
	}

	lineTotal := held.UnitPrice * held.Quantity
	catalogItemID := uuid.Nil
	if held.CatalogItemID != "" {
		if cid, cerr := uuid.Parse(held.CatalogItemID); cerr == nil {
			catalogItemID = cid
		}
	}
	sku := held.Sku
	if sku == "" {
		sku = "HELD" // schema requires a non-empty sku
	}

	tx, err := h.client.Tx(r.Context())
	if err != nil {
		h.log.Error("claim held item: begin tx failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Direct ent insert (NOT orders.AddLines): the service path fires a KDS ticket for new lines,
	// but a claimed item was already prepared — the kitchen must not make it again.
	line, err := tx.POSOrderLine.Create().
		SetOrderID(order.ID).
		SetCatalogItemID(catalogItemID).
		SetSku(sku).
		SetName(held.Name).
		SetQuantity(held.Quantity).
		SetUnitPrice(held.UnitPrice).
		SetTotalPrice(lineTotal).
		SetMetadata(map[string]any{"claimed_from_held_item": held.ID.String()}).
		Save(r.Context())
	if err != nil {
		_ = tx.Rollback()
		h.log.Error("claim held item: create line failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	updatedOrder, err := tx.POSOrder.UpdateOneID(order.ID).
		SetSubtotal(order.Subtotal + lineTotal).
		SetTotalAmount(order.TotalAmount + lineTotal).
		Save(r.Context())
	if err != nil {
		_ = tx.Rollback()
		h.log.Error("claim held item: adjust order totals failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	upd := tx.HeldItem.UpdateOneID(held.ID).
		SetStatus("claimed").
		SetClaimedOrderID(order.ID).
		SetResolvedByUserID(callerID).
		SetResolvedAt(time.Now())
	if input.Reason != "" {
		upd = upd.SetReason(input.Reason)
	}
	updatedHeld, err := upd.Save(r.Context())
	if err != nil {
		_ = tx.Rollback()
		h.log.Error("claim held item: update held item failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("claim held item: commit failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.auditSvc != nil {
		oid := held.OutletID
		amt := lineTotal
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID: tid, OutletID: &oid, ActorUserID: callerID,
			Action: "held_item.claimed", EntityType: "held_item", EntityID: id.String(), Reason: input.Reason,
			Amount: &amt,
			After:  map[string]any{"claimed_order_id": order.ID.String(), "order_line_id": line.ID.String()},
		})
	}
	jsonOK(w, map[string]any{
		"held_item":  updatedHeld,
		"order_line": line,
		"order": map[string]any{
			"id":           updatedOrder.ID,
			"subtotal":     updatedOrder.Subtotal,
			"total_amount": updatedOrder.TotalAmount,
		},
	})
}

// VoidHeldItem handles POST /{tenantID}/pos/held-items/{id}/void.
// Discarding an unclaimed set-aside item writes off prepared stock, so it is manager-gated (the
// end-of-shift "last resort"): callers without an override role must present an approval_token
// from a manager PIN/card step-up for action "held_item.void".
func (h *POSOrderHandler) VoidHeldItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid held item id", http.StatusBadRequest)
		return
	}
	var input struct {
		Reason        string `json:"reason,omitempty"`
		ApprovalToken string `json:"approval_token,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&input)

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	// Manager gate — mirrors GenerateVoidCode: managers/override roles self-approve, everyone
	// else needs a live step-up approval token for this exact action.
	var approverID *uuid.UUID
	if !(claims.IsPlatformOwner || hasOverrideRole(claims.Roles)) {
		if input.ApprovalToken == "" || len(h.terminalSecret) == 0 {
			jsonErrorWithCode(w, "voiding a held item requires manager approval", "approval_required", http.StatusForbidden)
			return
		}
		aid, valid := verifyApprovalToken(input.ApprovalToken, "held_item.void", h.terminalSecret)
		if !valid {
			jsonError(w, "invalid or expired approval", http.StatusForbidden)
			return
		}
		approverID = &aid
	} else if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
		if aid, valid := verifyApprovalToken(input.ApprovalToken, "held_item.void", h.terminalSecret); valid {
			approverID = &aid
		}
	}

	held, err := h.client.HeldItem.Query().Where(enthelditem.ID(id), enthelditem.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "held item not found", http.StatusNotFound)
		return
	}
	if held.Status != "held" {
		jsonError(w, "held item is already "+held.Status, http.StatusConflict)
		return
	}

	upd := held.Update().SetStatus("voided").SetResolvedByUserID(callerID).SetResolvedAt(time.Now())
	if input.Reason != "" {
		upd = upd.SetReason(input.Reason)
	}
	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("void held item failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.auditSvc != nil {
		oid := held.OutletID
		amt := held.UnitPrice * held.Quantity
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID: tid, OutletID: &oid, ActorUserID: callerID, ApproverID: approverID,
			Action: "held_item.voided", EntityType: "held_item", EntityID: id.String(), Reason: input.Reason,
			Amount: &amt,
		})
	}
	jsonOK(w, updated)
}
