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

	"github.com/bengobox/pos-service/internal/ent/orderlink"
	"github.com/bengobox/pos-service/internal/platform/ordering"
)

// riderDeps carries the cross-service dependencies for the WS-D assign-rider flow.
// They are optional: when unset, the rider endpoints return 503 rather than panicking.
type riderDeps struct {
	ordering     *ordering.Client
	logisticsURL string // LOGISTICS_SERVICE_URL base (e.g. https://logisticsapi.codevertexitsolutions.com)
	apiKey       string // shared INTERNAL_SERVICE_KEY
}

// SetRiderDeps wires the ordering S2S client + logistics base URL/key used by the
// assign-rider (WS-D) endpoints. Data ownership: ordering-backend owns the order +
// rider-assignment flow; logistics-api owns the fleet. pos-api proxies the fleet
// read and DELEGATES the assignment write to ordering-backend.
func (h *OnlineOrderHandler) SetRiderDeps(orderingClient *ordering.Client, logisticsURL, apiKey string) {
	h.rider = &riderDeps{ordering: orderingClient, logisticsURL: logisticsURL, apiKey: apiKey}
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

// AssignRider handles POST /{tenantID}/pos/online-orders/{orderID}/assign-rider
// for a delivery online order. It resolves the external (ordering) order id via the
// OrderLink, then DELEGATES to the canonical ordering-backend admin endpoint
// (PUT /api/v1/{tenantSlug}/admin/orders/{externalOrderID}/rider). pos-api never calls
// logistics /tasks assign directly — ordering-backend owns the rider-assignment flow.
func (h *OnlineOrderHandler) AssignRider(w http.ResponseWriter, r *http.Request) {
	if h.rider == nil || h.rider.ordering == nil || !h.rider.ordering.Enabled() {
		jsonError(w, "ordering service not configured", http.StatusServiceUnavailable)
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

	// Confirm the POS order belongs to this tenant before delegating.
	order, err := h.db.POSOrder.Get(r.Context(), oid)
	if err != nil || order.TenantID != tid {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	// Resolve the external (ordering-backend) order id via the OrderLink.
	link, err := h.db.OrderLink.Query().
		Where(orderlink.OrderID(oid)).
		First(r.Context())
	if err != nil {
		jsonError(w, "order is not linked to an online order", http.StatusBadRequest)
		return
	}

	slug := tenantSlugFromRequest(r)
	if slug == "" {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}

	if err := h.rider.ordering.AssignRider(r.Context(), slug, link.ExternalOrderID, body.RiderID); err != nil {
		h.log.Error("assign-rider: delegation to ordering-backend failed",
			zap.Error(err),
			zap.String("external_order_id", link.ExternalOrderID),
			zap.String("rider_id", body.RiderID),
		)
		jsonError(w, "failed to assign rider", http.StatusBadGateway)
		return
	}

	jsonOK(w, map[string]any{
		"status":            "rider_assigned",
		"order_id":          oid.String(),
		"external_order_id": link.ExternalOrderID,
		"rider_id":          body.RiderID,
	})
}
