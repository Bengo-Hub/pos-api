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

// ResolveHeldItem handles POST /{tenantID}/pos/held-items/{id}/claim and .../void.
// claim = a new customer took the item (optionally into claimed_order_id); void = discarded
// (unclaimed at shift end). Both are terminal states that clear it from the active pool.
func (h *POSOrderHandler) resolveHeldItem(w http.ResponseWriter, r *http.Request, toStatus string) {
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
		ClaimedOrderID string `json:"claimed_order_id,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&input)

	callerID := uuid.Nil
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		callerID, _ = uuid.Parse(claims.Subject)
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

	upd := held.Update().SetStatus(toStatus).SetResolvedByUserID(callerID).SetResolvedAt(time.Now())
	if input.Reason != "" {
		upd = upd.SetReason(input.Reason)
	}
	if toStatus == "claimed" && input.ClaimedOrderID != "" {
		if coid, perr := uuid.Parse(input.ClaimedOrderID); perr == nil {
			upd = upd.SetClaimedOrderID(coid)
		}
	}
	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("resolve held item failed", zap.Error(err), zap.String("to", toStatus))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.auditSvc != nil {
		oid := held.OutletID
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID: tid, OutletID: &oid, ActorUserID: callerID,
			Action: "held_item." + toStatus, EntityType: "held_item", EntityID: id.String(), Reason: input.Reason,
		})
	}
	jsonOK(w, updated)
}

// ClaimHeldItem handles POST /{tenantID}/pos/held-items/{id}/claim.
func (h *POSOrderHandler) ClaimHeldItem(w http.ResponseWriter, r *http.Request) {
	h.resolveHeldItem(w, r, "claimed")
}

// VoidHeldItem handles POST /{tenantID}/pos/held-items/{id}/void.
func (h *POSOrderHandler) VoidHeldItem(w http.ResponseWriter, r *http.Request) {
	h.resolveHeldItem(w, r, "voided")
}
