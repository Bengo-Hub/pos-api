package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// POSOrderHandler handles POS order CRUD endpoints.
type POSOrderHandler struct {
	log        *zap.Logger
	client     *ent.Client
	orderSvc   *orders.Service
	subsClient *subscriptions.Client
}

func NewPOSOrderHandler(log *zap.Logger, client *ent.Client, orderSvc *orders.Service, subsClient *subscriptions.Client) *POSOrderHandler {
	return &POSOrderHandler{log: log, client: client, orderSvc: orderSvc, subsClient: subsClient}
}

// createOrderLineInput is a single line in the order create request body.
type createOrderLineInput struct {
	CatalogItemID uuid.UUID `json:"catalog_item_id"`
	SKU           string    `json:"sku"`
	Name          string    `json:"name"`
	Quantity      float64   `json:"quantity"`
	UnitPrice     float64   `json:"unit_price"`
	TotalPrice    float64   `json:"total_price"`
}

// createOrderInput is the body for POST /pos/orders.
type createOrderInput struct {
	OutletID    uuid.UUID              `json:"outlet_id"`
	DeviceID    uuid.UUID              `json:"device_id"`
	OrderNumber string                 `json:"order_number"`
	Currency    string                 `json:"currency"`
	Lines       []createOrderLineInput `json:"lines"`
	Metadata    map[string]interface{} `json:"metadata"`
}

// updateStatusInput is the body for PATCH /pos/orders/{id}/status.
type updateStatusInput struct {
	Status string `json:"status"`
}

// ListOrders handles GET /{tenantID}/pos/orders
// Optional query params: status, limit, offset
func (h *POSOrderHandler) ListOrders(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	filters := []predicate.POSOrder{posorder.TenantID(tid)}
	if status := q.Get("status"); status != "" {
		filters = append(filters, posorder.Status(status))
	}

	orders, err := h.client.POSOrder.Query().
		Where(filters...).
		WithLines().
		WithPayments().
		Order(ent.Desc(posorder.FieldCreatedAt)).
		All(r.Context())
	if err != nil {
		h.log.Error("list orders failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]interface{}{"orders": orders, "total": len(orders)})
}

// GetOrder handles GET /{tenantID}/pos/orders/{orderID}
func (h *POSOrderHandler) GetOrder(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		WithLines(func(q *ent.POSOrderLineQuery) { q.WithModifiers() }).
		WithPayments().
		WithEvents().
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("get order failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, order)
}

// CreateOrder handles POST /{tenantID}/pos/orders
func (h *POSOrderHandler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	// Subscription enforcement: skip for platform owners, block expired tenants.
	if !httpware.IsPlatformOwner(r.Context()) && h.subsClient != nil {
		bearerToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tenantSlug := ""
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			tenantSlug = claims.GetTenantSlug()
		}
		if !h.subsClient.IsSubscriptionActive(r.Context(), tenantID, tenantSlug, bearerToken) {
			jsonError(w, "active subscription required", http.StatusPaymentRequired)
			return
		}
	}

	var input createOrderInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Get user ID from auth claims
	var userID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims.Subject != "" {
		userID, _ = uuid.Parse(claims.Subject)
	}

	// Convert handler input to service request
	lines := make([]orders.OrderLineInput, len(input.Lines))
	for i, l := range input.Lines {
		lines[i] = orders.OrderLineInput{
			CatalogItemID: l.CatalogItemID,
			SKU:           l.SKU,
			Name:          l.Name,
			Quantity:      l.Quantity,
			UnitPrice:     l.UnitPrice,
			TotalPrice:    l.TotalPrice,
		}
	}

	order, err := h.orderSvc.CreateOrder(r.Context(), orders.CreateOrderRequest{
		TenantID:    tid,
		OutletID:    input.OutletID,
		DeviceID:    input.DeviceID,
		UserID:      userID,
		OrderNumber: input.OrderNumber,
		Currency:    input.Currency,
		Lines:       lines,
		Metadata:    input.Metadata,
	})
	if err != nil {
		h.log.Error("create order failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(order)
}

// UpdateStatus handles PATCH /{tenantID}/pos/orders/{orderID}/status
func (h *POSOrderHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	var input updateStatusInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Status == "" {
		jsonError(w, "status is required", http.StatusBadRequest)
		return
	}

	updated, err := h.orderSvc.UpdateStatus(r.Context(), tid, orderID, input.Status)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("update order status failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	jsonOK(w, updated)
}

// helpers

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
