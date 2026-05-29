package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
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
	CatalogItemID uuid.UUID              `json:"catalog_item_id"`
	SKU           string                 `json:"sku"`
	Name          string                 `json:"name"`
	Quantity      float64                `json:"quantity"`
	UnitPrice     float64                `json:"unit_price"`
	TotalPrice    float64                `json:"total_price"`
	CourseNumber  int                    `json:"course_number"` // 0=fire immediately, 1=Starter, 2=Main, 3=Dessert
	Metadata      map[string]interface{} `json:"metadata"`
}

// createOrderInput is the body for POST /pos/orders.
type createOrderInput struct {
	OutletID       string                 `json:"outlet_id"`
	DeviceID       string                 `json:"device_id"`
	OrderNumber    string                 `json:"order_number"`
	Currency       string                 `json:"currency"`
	Lines          []createOrderLineInput `json:"lines"`
	Metadata       map[string]interface{} `json:"metadata"`
	PrescriptionID *string                `json:"prescription_id,omitempty"`
	OrderSubtype   string                 `json:"order_subtype"` // dine_in | takeaway | room_service | delivery | bar_tab
	TableID        string                 `json:"table_id"`      // hospitality dine-in table UUID
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
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			filters = append(filters, posorder.OutletID(oid))
		}
	}
	if status := q.Get("status"); status != "" {
		statuses := strings.Split(status, ",")
		if len(statuses) > 1 {
			filters = append(filters, posorder.StatusIn(statuses...))
		} else {
			filters = append(filters, posorder.Status(statuses[0]))
		}
	}
	// staff_id scopes the list to orders created by a specific staff member (view_own roles).
	if staffIDStr := q.Get("staff_id"); staffIDStr != "" {
		if staffUID, err := uuid.Parse(staffIDStr); err == nil {
			filters = append(filters, posorder.UserID(staffUID))
		}
	}

	p := pagination.Parse(r)
	baseQ := h.client.POSOrder.Query().Where(filters...)
	total, _ := baseQ.Clone().Count(r.Context())
	orderList, err := baseQ.WithLines().WithPayments().Order(ent.Desc(posorder.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list orders failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(orderList, total, p))
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

	whereArgs := []predicate.POSOrder{posorder.ID(orderID), posorder.TenantID(tid)}
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			whereArgs = append(whereArgs, posorder.OutletID(oid))
		}
	}
	order, err := h.client.POSOrder.Query().
		Where(whereArgs...).
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

	// Prescription-only items: block unless a prescription_id is provided.
	if input.PrescriptionID == nil || *input.PrescriptionID == "" {
		skus := make([]string, 0, len(input.Lines))
		for _, l := range input.Lines {
			if l.SKU != "" {
				skus = append(skus, l.SKU)
			}
		}
		if len(skus) > 0 {
			rxOverrides, _ := h.client.POSCatalogOverride.Query().
				Where(
					entoverride.TenantID(tid),
					entoverride.InventorySkuIn(skus...),
					entoverride.RequiresPrescriptionEQ(true),
				).All(r.Context())
			if len(rxOverrides) > 0 {
				blocked := make([]string, 0, len(rxOverrides))
				for _, o := range rxOverrides {
					blocked = append(blocked, o.InventorySku)
				}
				jsonError(w, "prescription required for: "+strings.Join(blocked, ", "), http.StatusUnprocessableEntity)
				return
			}
		}
	}

	// Parse optional UUID fields — fall back to zero UUID if missing/invalid.
	outletID, _ := uuid.Parse(input.OutletID)
	deviceID, _ := uuid.Parse(input.DeviceID)

	// If outlet_id not in body, try the X-Outlet-ID header set by pos-ui.
	if outletID == uuid.Nil {
		if hv := r.Header.Get("X-Outlet-ID"); hv != "" {
			outletID, _ = uuid.Parse(hv)
		}
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
			CourseNumber:  l.CourseNumber,
			Metadata:      l.Metadata,
		}
	}

	order, err := h.orderSvc.CreateOrder(r.Context(), orders.CreateOrderRequest{
		TenantID:     tid,
		OutletID:     outletID,
		DeviceID:     deviceID,
		UserID:       userID,
		OrderNumber:  input.OrderNumber,
		Currency:     input.Currency,
		Lines:        lines,
		Metadata:     input.Metadata,
		OrderSubtype: input.OrderSubtype,
		TableID:      input.TableID,
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

// VoidOrder handles PATCH /{tenantID}/pos/orders/{orderID}/void
// Requires pos.orders.void permission (admin/manager only).
func (h *POSOrderHandler) VoidOrder(w http.ResponseWriter, r *http.Request) {
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

	var input struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Reason == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	if order.Status == "voided" {
		jsonError(w, "order is already voided", http.StatusBadRequest)
		return
	}

	now := time.Now()
	updated, err := order.Update().
		SetStatus("voided").
		SetVoidedReason(input.Reason).
		SetVoidedBy(callerID).
		SetVoidedAt(now).
		Save(r.Context())
	if err != nil {
		h.log.Error("void order failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("order voided",
		zap.Stringer("order_id", orderID),
		zap.Stringer("voided_by", callerID),
		zap.String("reason", input.Reason),
	)
	jsonOK(w, updated)
}

// AddOrderLines handles POST /{tenantID}/pos/orders/{orderID}/lines
// Appends new items to an existing open order and notifies KDS stations.
func (h *POSOrderHandler) AddOrderLines(w http.ResponseWriter, r *http.Request) {
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

	var input struct {
		Lines []createOrderLineInput `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Lines) == 0 {
		jsonError(w, "lines are required", http.StatusBadRequest)
		return
	}

	lines := make([]orders.OrderLineInput, len(input.Lines))
	for i, l := range input.Lines {
		lines[i] = orders.OrderLineInput{
			CatalogItemID: l.CatalogItemID,
			SKU:           l.SKU,
			Name:          l.Name,
			Quantity:      l.Quantity,
			UnitPrice:     l.UnitPrice,
			TotalPrice:    l.TotalPrice,
			CourseNumber:  l.CourseNumber,
			Metadata:      l.Metadata,
		}
	}

	result, err := h.orderSvc.AddOrderLines(r.Context(), tid, orderID, lines)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("add order lines failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	jsonOK(w, result)
}

// CaptureSerial handles POST /{tenantID}/pos/orders/{orderID}/lines/{lineID}/serials
// Captures a serial number for a tracked item on an order line.
func (h *POSOrderHandler) CaptureSerial(w http.ResponseWriter, r *http.Request) {
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

	var input struct {
		SerialNumber string `json:"serial_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SerialNumber == "" {
		jsonError(w, "serial_number is required", http.StatusBadRequest)
		return
	}

	// Verify the line belongs to this order + tenant
	line, err := h.client.POSOrderLine.Query().
		Where(posorderline.ID(lineID), posorderline.OrderID(orderID)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order line not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Create a SerialNumberLog entry for audit trail
	_, err = h.client.SerialNumberLog.Create().
		SetTenantID(tid).
		SetOrderLineID(lineID).
		SetSerialNumber(input.SerialNumber).
		SetItemSku(line.Sku).
		Save(r.Context())
	if err != nil {
		h.log.Error("capture serial failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"order_line_id": lineID, "serial_number": input.SerialNumber})
}

// FireCourse handles POST /{tenantID}/pos/orders/{orderID}/fire-course
// Marks a course as fired: sets order.fired_courses = course, then creates KDS tickets
// for all lines whose course_number == course (items with lower courses already fired,
// course_number=0 items fire at order creation).
func (h *POSOrderHandler) FireCourse(w http.ResponseWriter, r *http.Request) {
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

	var input struct {
		Course int `json:"course"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Course < 1 || input.Course > 9 {
		jsonError(w, "course must be 1–9", http.StatusBadRequest)
		return
	}

	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		WithLines().
		Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	if input.Course <= order.FiredCourses {
		jsonError(w, "course already fired", http.StatusConflict)
		return
	}

	// Update the order's fired_courses watermark
	updated, err := order.Update().SetFiredCourses(input.Course).Save(r.Context())
	if err != nil {
		h.log.Error("fire-course: update failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Trigger KDS ticket creation for all lines belonging to the fired course
	if err := h.orderSvc.FireCourseKDS(r.Context(), tid, order, input.Course); err != nil {
		h.log.Warn("fire-course: KDS ticket creation partially failed", zap.Error(err))
	}

	jsonOK(w, map[string]any{
		"order_id":      orderID,
		"fired_courses": updated.FiredCourses,
		"course":        input.Course,
	})
}

