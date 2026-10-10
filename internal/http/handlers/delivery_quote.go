package handlers

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/platform/logistics"
)

// Till deliveries are priced by logistics-api's delivery areas and policy (the one pricing
// rule online checkout also uses). pos-api only asks for the quote and charges it; it keeps
// no fee tables of its own.

// DeliveryQuoter prices a delivery from an outlet to a point.
type DeliveryQuoter interface {
	QuoteDelivery(ctx context.Context, tenantID, outletID uuid.UUID, lat, lng, orderTotal float64) (*logistics.Quote, error)
	DeliveryAreas(ctx context.Context, tenantID, outletID uuid.UUID) ([]logistics.DeliveryArea, error)
}

// deliveryQuoteTimeout keeps a slow quote from holding up the till.
const deliveryQuoteTimeout = 4 * time.Second

// deliveryPoint reads the dropoff pin the terminal stores on a delivery order.
func deliveryPoint(meta map[string]interface{}) (float64, float64, bool) {
	lat, okLat := metaFloat(meta["delivery_lat"])
	lng, okLng := metaFloat(meta["delivery_lng"])
	if !okLat || !okLng || (lat == 0 && lng == 0) {
		return 0, 0, false
	}
	return lat, lng, true
}

func metaFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

// applyDeliveryQuote prices a pinned delivery order and sets charges.shipping to the quoted
// fee, recording the quote in metadata.delivery_quote. Returns quoted=true when the shipping
// charge came from the quote. ok=false means a response was written (outside the delivery
// areas). If logistics cannot be reached the order keeps whatever charge the cashier typed,
// which then goes through the usual manager adjustment gate.
func (h *POSOrderHandler) applyDeliveryQuote(w http.ResponseWriter, r *http.Request, tenantID, outletID uuid.UUID, input *createOrderInput) (quoted, ok bool) {
	if h.deliveryQuoter == nil || input.OrderSubtype != "delivery" {
		return false, true
	}
	lat, lng, has := deliveryPoint(input.Metadata)
	if !has {
		return false, true
	}
	var subtotal float64
	for _, l := range input.Lines {
		subtotal += l.TotalPrice
	}
	ctx, cancel := context.WithTimeout(r.Context(), deliveryQuoteTimeout)
	defer cancel()
	q, err := h.deliveryQuoter.QuoteDelivery(ctx, tenantID, outletID, lat, lng, subtotal)
	if err != nil {
		h.log.Warn("delivery quote unavailable; keeping the typed shipping charge", zap.Error(err))
		return false, true
	}
	if !q.Serviceable {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "this address is outside the delivery areas",
			"code":   "delivery_not_serviceable",
			"reason": q.Reason,
		})
		return false, false
	}
	if input.Charges == nil {
		input.Charges = map[string]float64{}
	}
	input.Charges["shipping"] = q.Fee
	if input.Metadata == nil {
		input.Metadata = map[string]interface{}{}
	}
	snap := map[string]interface{}{
		"fee": q.Fee, "free": q.Free, "method": q.Method, "currency": q.Currency,
		"distance_km": q.DistanceKm, "quoted_at": time.Now().UTC().Format(time.RFC3339),
	}
	if q.Zone != nil {
		snap["zone_id"] = q.Zone.ID
		snap["zone_name"] = q.Zone.Name
	}
	input.Metadata["delivery_quote"] = snap
	return true, true
}

// DeliveryQuote handles GET /{tenantID}/pos/delivery-quote?lat=&lng=&outlet_id=&order_total=
// so the terminal can show the fee before the sale is saved. The fee charged is re-quoted on
// save, so this preview can never be used to set a price.
func (h *POSOrderHandler) DeliveryQuote(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	if h.deliveryQuoter == nil {
		jsonError(w, "delivery pricing not configured", http.StatusServiceUnavailable)
		return
	}
	qv := r.URL.Query()
	lat, errLat := strconv.ParseFloat(qv.Get("lat"), 64)
	lng, errLng := strconv.ParseFloat(qv.Get("lng"), 64)
	if errLat != nil || errLng != nil {
		jsonError(w, "lat and lng are required", http.StatusBadRequest)
		return
	}
	outletID, _ := uuid.Parse(qv.Get("outlet_id"))
	total, _ := strconv.ParseFloat(qv.Get("order_total"), 64)
	ctx, cancel := context.WithTimeout(r.Context(), deliveryQuoteTimeout)
	defer cancel()
	q, err := h.deliveryQuoter.QuoteDelivery(ctx, tid, outletID, lat, lng, total)
	if err != nil {
		h.log.Warn("delivery quote preview failed", zap.Error(err))
		jsonError(w, "delivery pricing unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonOK(w, q)
}

// DeliveryAreas handles GET /{tenantID}/pos/delivery-areas?outlet_id= : the tenant's named
// delivery areas with their centre and fee, for the terminal's area picker.
func (h *POSOrderHandler) DeliveryAreas(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	if h.deliveryQuoter == nil {
		jsonOK(w, map[string]any{"areas": []logistics.DeliveryArea{}, "configured": false})
		return
	}
	outletID, _ := uuid.Parse(r.URL.Query().Get("outlet_id"))
	ctx, cancel := context.WithTimeout(r.Context(), deliveryQuoteTimeout)
	defer cancel()
	areas, err := h.deliveryQuoter.DeliveryAreas(ctx, tid, outletID)
	if err != nil {
		h.log.Warn("delivery areas unavailable", zap.Error(err))
		jsonError(w, "delivery areas unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonOK(w, map[string]any{"areas": areas, "configured": true})
}
