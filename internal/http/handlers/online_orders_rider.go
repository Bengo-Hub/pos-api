package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/orderlink"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/platform/logistics"
	"github.com/bengobox/pos-service/internal/platform/ordering"
)

// tenantSlugFromRequest resolves the tenant slug for cross-service (S2S) calls: JWT claims
// first, then the httpware tenant context. Empty string when neither carries a slug.
// (Formerly lived in the purchase-orders proxy, which was removed — inventory owns POs.)
func tenantSlugFromRequest(r *http.Request) string {
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		if slug := claims.GetTenantSlug(); slug != "" {
			return slug
		}
	}
	return httpware.GetTenantSlug(r.Context())
}

// riderDeps carries the cross-service dependencies for the WS-D assign-rider flow.
// They are optional: when unset, the rider endpoints return 503 rather than panicking.
type riderDeps struct {
	ordering     *ordering.Client
	logistics    *logistics.Client // direct dispatch for POS-native delivery orders
	logisticsURL string            // LOGISTICS_SERVICE_URL base (e.g. https://logisticsapi.codevertexitsolutions.com)
	apiKey       string            // shared INTERNAL_SERVICE_KEY
}

// SetRiderDeps wires the ordering S2S client + logistics base URL/key + logistics dispatch client
// used by the assign-rider flow. Data ownership: ONLINE orders belong to ordering-backend (pos-api
// delegates their rider assignment there); POS-NATIVE delivery orders are dispatched by pos-api
// calling logistics-api directly. logistics-api always owns the fleet (pos-api proxies the read).
func (h *OnlineOrderHandler) SetRiderDeps(orderingClient *ordering.Client, logisticsClient *logistics.Client, logisticsURL, apiKey string) {
	h.rider = &riderDeps{ordering: orderingClient, logistics: logisticsClient, logisticsURL: logisticsURL, apiKey: apiKey}
}

// ListAvailableRiders handles GET /{tenantID}/pos/online-orders/riders
// Proxies logistics-api GET /api/v1/{tenantSlug}/fleet/members?status=active (read-only).
func (h *OnlineOrderHandler) ListAvailableRiders(w http.ResponseWriter, r *http.Request) {
	if h.rider == nil || h.rider.logisticsURL == "" {
		jsonError(w, "rider service not configured", http.StatusServiceUnavailable)
		return
	}
	slug := tenantSlugFromRequest(r)
	if slug == "" {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}

	upstream := fmt.Sprintf("%s/api/v1/%s/fleet/members?status=active", h.rider.logisticsURL, url.PathEscape(slug))
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, http.NoBody)
	if err != nil {
		h.log.Error("list-riders: build request failed", zap.Error(err))
		jsonError(w, "proxy error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("X-API-Key", h.rider.apiKey)
	if oid := r.Header.Get("X-Outlet-ID"); oid != "" {
		req.Header.Set("X-Outlet-ID", oid)
	}

	resp, err := s2sHTTPClient.Do(req)
	if err != nil {
		h.log.Error("list-riders: upstream call failed", zap.Error(err), zap.String("upstream", upstream))
		jsonError(w, "logistics unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// AssignRider handles POST /{tenantID}/pos/online-orders/{orderID}/assign-rider.
// Two paths by ownership:
//   - ONLINE order (has an OrderLink): DELEGATE to ordering-backend's admin rider endpoint —
//     ordering-backend owns the online-order + rider-assignment flow.
//   - POS-NATIVE delivery order (no OrderLink): dispatch DIRECTLY via logistics-api S2S — create
//     a delivery task on first assignment (id cached on the order), then assign the rider.
func (h *OnlineOrderHandler) AssignRider(w http.ResponseWriter, r *http.Request) {
	if h.rider == nil {
		jsonError(w, "rider service not configured", http.StatusServiceUnavailable)
		return
	}

	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	oid, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid orderID", http.StatusBadRequest)
		return
	}

	var body struct {
		RiderID string `json:"rider_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RiderID == "" {
		jsonError(w, "rider_id is required", http.StatusBadRequest)
		return
	}

	// Confirm the POS order belongs to this tenant.
	order, err := h.db.POSOrder.Get(r.Context(), oid)
	if err != nil || order.TenantID != tid {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	// ── Online order path: delegate to ordering-backend when the order is linked. ──
	if link, linkErr := h.db.OrderLink.Query().Where(orderlink.OrderID(oid)).First(r.Context()); linkErr == nil {
		if h.rider.ordering == nil || !h.rider.ordering.Enabled() {
			jsonError(w, "ordering service not configured", http.StatusServiceUnavailable)
			return
		}
		slug := tenantSlugFromRequest(r)
		if slug == "" {
			jsonError(w, "tenant context required", http.StatusBadRequest)
			return
		}
		if err := h.rider.ordering.AssignRider(r.Context(), slug, link.ExternalOrderID, body.RiderID); err != nil {
			h.log.Error("assign-rider: delegation to ordering-backend failed",
				zap.Error(err), zap.String("external_order_id", link.ExternalOrderID), zap.String("rider_id", body.RiderID))
			jsonError(w, "failed to assign rider", http.StatusBadGateway)
			return
		}
		jsonOK(w, map[string]any{
			"status": "rider_assigned", "order_id": oid.String(),
			"external_order_id": link.ExternalOrderID, "rider_id": body.RiderID,
		})
		return
	}

	// ── POS-native path: dispatch the delivery directly via logistics-api. ──
	if string(order.OrderSubtype) != "delivery" {
		jsonError(w, "order is not a delivery order", http.StatusBadRequest)
		return
	}
	if h.rider.logistics == nil || !h.rider.logistics.Enabled() {
		jsonError(w, "logistics dispatch not configured", http.StatusServiceUnavailable)
		return
	}

	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}

	// Ensure a logistics delivery task exists (create once, cache its id on the order).
	var taskID uuid.UUID
	if s, _ := meta["logistics_task_id"].(string); s != "" {
		taskID, _ = uuid.Parse(s)
	}
	if taskID == uuid.Nil {
		task, cErr := h.rider.logistics.CreateDeliveryTask(r.Context(), tid, buildDeliveryTaskRequest(order))
		if cErr != nil {
			h.log.Error("assign-rider: create logistics task failed", zap.Error(cErr), zap.String("order_id", oid.String()))
			jsonError(w, "failed to create delivery task", http.StatusBadGateway)
			return
		}
		taskID, _ = uuid.Parse(task.ID)
		meta["logistics_task_id"] = task.ID
		if task.TrackingCode != "" {
			meta["tracking_code"] = task.TrackingCode
		}
	}

	if err := h.rider.logistics.AssignTask(r.Context(), tid, taskID, body.RiderID); err != nil {
		h.log.Error("assign-rider: logistics assign failed", zap.Error(err), zap.String("task_id", taskID.String()), zap.String("rider_id", body.RiderID))
		jsonError(w, "failed to assign rider", http.StatusBadGateway)
		return
	}

	// Stamp dispatch state on the order so the dispatch queue + tracking reflect it.
	meta["rider_id"] = body.RiderID
	meta["dispatch_status"] = "rider_assigned"
	if _, uErr := h.db.POSOrder.UpdateOne(order).SetMetadata(meta).Save(r.Context()); uErr != nil {
		h.log.Warn("assign-rider: failed to stamp order metadata", zap.Error(uErr))
	}

	jsonOK(w, map[string]any{
		"status": "rider_assigned", "order_id": oid.String(),
		"logistics_task_id": taskID.String(), "rider_id": body.RiderID,
	})
}

// buildDeliveryTaskRequest maps a POS delivery order onto a logistics delivery task. Customer
// name/phone come from the order; the dropoff address + coords + notes come from order metadata
// (delivery_address / delivery_lat / delivery_lng / delivery_notes), captured at order time.
func buildDeliveryTaskRequest(order *ent.POSOrder) logistics.CreateTaskRequest {
	meta := order.Metadata
	str := func(k string) string {
		if meta == nil {
			return ""
		}
		s, _ := meta[k].(string)
		return s
	}
	num := func(k string) float64 {
		if meta == nil {
			return 0
		}
		if f, ok := meta[k].(float64); ok {
			return f
		}
		return 0
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return logistics.CreateTaskRequest{
		ExternalReference: order.ID.String(),
		SourceService:     "pos",
		TaskType:          "delivery",
		Priority:          1,
		DropoffAddress:    str("delivery_address"),
		DropoffLat:        num("delivery_lat"),
		DropoffLng:        num("delivery_lng"),
		DropoffContact:    deref(order.CustomerName),
		CustomerName:      deref(order.CustomerName),
		CustomerPhone:     deref(order.CustomerPhone),
		Instructions:      str("delivery_notes"),
		Metadata: map[string]any{
			"order_number": order.OrderNumber,
			"outlet_id":    order.OutletID.String(),
			"pos_order_id": order.ID.String(),
		},
	}
}

// ListDeliveryDispatch handles GET /{tenantID}/pos/online-orders/dispatch
// Returns POS-native delivery orders (order_subtype=delivery, not cancelled/voided) so the POS
// dispatch queue can list them and assign riders. Online (ordering-backed) deliveries are listed
// separately via their own source filter.
func (h *OnlineOrderHandler) ListDeliveryDispatch(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.POSOrder.Query().Where(
		posorder.TenantID(tid),
		posorder.OrderSubtypeEQ(posorder.OrderSubtypeDelivery),
		posorder.StatusNotIn("cancelled", "voided"),
		notCollectedFilter(), // drop delivered/collected orders to History
	)
	if outletID := r.Header.Get("X-Outlet-ID"); outletID != "" {
		if oid, perr := uuid.Parse(outletID); perr == nil {
			q = q.Where(posorder.OutletID(oid))
		}
	}
	orders, err := q.Order(ent.Desc(posorder.FieldCreatedAt)).Limit(100).All(r.Context())
	if err != nil {
		h.log.Error("list delivery dispatch failed", zap.Error(err))
		jsonError(w, "failed to list delivery orders", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": orders, "total": len(orders)})
}
