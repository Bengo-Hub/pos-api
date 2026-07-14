package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

type editLineInput struct {
	UnitPrice *float64 `json:"unit_price"`
	Quantity  *float64 `json:"quantity"`
	Reason    string   `json:"reason"`
	// UpdateCatalogPrice additionally propagates the new unit price to the inventory
	// catalog (item guardrails/tier rows + recipe selling price) and any local POS
	// catalog override, so the correction applies to future sales too — not just this
	// order. Best-effort: the line edit succeeds even if the propagation fails.
	UpdateCatalogPrice bool `json:"update_catalog_price"`
}

// EditOrderLine handles PATCH /{tenantID}/pos/orders/{orderID}/lines/{lineID}.
//
// Manager/admin corrective tool: directly edits a persisted line's unit price and/or
// quantity — e.g. the till rang up a sale at a stale cached price and someone would
// otherwise need a raw database fix. Recomputes the order's headline totals and writes
// a full before/after entry to the central audit trail. The route is gated on
// pos.orders.manage (router.go), so any caller who reaches this handler already holds
// manager authority — no additional step-up token is required, mirroring
// UpdateOrderPayment/VoidOrderPayment rather than the optional-step-up VoidOrderLine.
func (h *POSOrderHandler) EditOrderLine(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}
	lineID, err := uuid.Parse(chi.URLParam(r, "lineID"))
	if err != nil {
		jsonError(w, "invalid line_id", http.StatusBadRequest)
		return
	}
	var input editLineInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Reason == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}
	if input.UnitPrice == nil && input.Quantity == nil {
		jsonError(w, "unit_price or quantity is required", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	result, err := h.orderSvc.EditOrderLine(r.Context(), tid, orderID, lineID, orders.EditOrderLineInput{
		UnitPrice: input.UnitPrice,
		Quantity:  input.Quantity,
	})
	if err != nil {
		h.log.Warn("edit order line failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if h.auditSvc != nil {
		oid := result.Order.OutletID
		amt := result.Line.TotalPrice - result.BeforeTotal
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: callerID,
			Action:      "order.line_price_edit",
			EntityType:  "pos_order_line",
			EntityID:    lineID.String(),
			Reason:      input.Reason,
			Amount:      &amt,
			Before: map[string]any{
				"sku": result.Line.Sku, "name": result.Line.Name,
				"unit_price": result.BeforePrice, "quantity": result.BeforeQty, "total_price": result.BeforeTotal,
			},
			After: map[string]any{
				"unit_price": result.Line.UnitPrice, "quantity": result.Line.Quantity, "total_price": result.Line.TotalPrice,
			},
		})
	}

	// Optional catalog propagation: repoint the item's price in inventory (guardrails,
	// tier rows, recipe selling price) and any local POS override carrying its own price,
	// so the correction outlives this one order. Best-effort — the line edit already
	// committed; a propagation failure is reported in the response, never a 4xx/5xx.
	catalogUpdated := false
	if input.UpdateCatalogPrice && input.UnitPrice != nil && result.Line.Sku != "" {
		if h.inventoryClient != nil {
			if invErr := h.inventoryClient.SetItemPrice(r.Context(), tid.String(), result.Line.Sku, *input.UnitPrice); invErr != nil {
				h.log.Warn("edit order line: inventory price propagation failed",
					zap.String("sku", result.Line.Sku), zap.Error(invErr))
			} else {
				catalogUpdated = true
			}
		}
		// A local override row with its own selling_price would keep outranking the
		// inventory price at the terminal — keep it in step.
		if n, ovErr := h.client.POSCatalogOverride.Update().
			Where(
				entoverride.TenantID(tid),
				entoverride.InventorySku(result.Line.Sku),
				entoverride.SellingPriceNotNil(),
			).
			SetSellingPrice(*input.UnitPrice).
			Save(r.Context()); ovErr != nil {
			h.log.Warn("edit order line: catalog override price sync failed",
				zap.String("sku", result.Line.Sku), zap.Error(ovErr))
		} else if n > 0 {
			catalogUpdated = true
		}
	}

	jsonOK(w, map[string]any{"order": result.Order, "line": result.Line, "catalog_price_updated": catalogUpdated})
}
