package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/outletsetting"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/ent/tender"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// POSOrderHandler handles POS order CRUD endpoints.
type POSOrderHandler struct {
	log        *zap.Logger
	client     *ent.Client
	orderSvc   *orders.Service
	subsClient *subscriptions.Client
	auditSvc   *audit.Service
	// rbac backs the per-cashier visibility scoping (pos.orders.view_own) on order reads.
	rbac outletmw.PermissionChecker
	// terminalSecret verifies manager-PIN step-up approval tokens for sensitive actions.
	terminalSecret []byte
}

func NewPOSOrderHandler(log *zap.Logger, client *ent.Client, orderSvc *orders.Service, subsClient *subscriptions.Client) *POSOrderHandler {
	return &POSOrderHandler{log: log, client: client, orderSvc: orderSvc, subsClient: subsClient}
}

// SetAuditService wires the centralized audit trail for void/line-removal events.
func (h *POSOrderHandler) SetAuditService(a *audit.Service) { h.auditSvc = a }

// SetRBAC wires the local RBAC fallback used by the per-cashier visibility scoping.
func (h *POSOrderHandler) SetRBAC(rbac outletmw.PermissionChecker) { h.rbac = rbac }

// ownOrdersPredicate returns the visibility predicate for principals limited to their OWN
// sales (REQ-007): users holding pos.orders.view_own but none of view/change/manage see
// only orders they created, plus shared ACTIVE orders (open / pending_payment) so till
// hand-offs — a cashier settling a waiter's open bill — keep working. Full-view principals
// (and superusers/platform owners, via HasServicePermission's bypass) get no restriction.
func (h *POSOrderHandler) ownOrdersPredicate(r *http.Request) (predicate.POSOrder, bool) {
	if outletmw.HasServicePermission(r, h.rbac, "pos.orders.view", "pos.orders.change", "pos.orders.manage") {
		return nil, false
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil || claims.Subject == "" {
		return nil, false
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil || uid == uuid.Nil {
		return nil, false
	}
	return posorder.Or(
		posorder.UserID(uid),
		posorder.StatusIn(orders.StatusOpen, orders.StatusPendingPayment),
	), true
}

// SetTerminalSecret wires the HMAC secret used to verify manager step-up tokens.
func (h *POSOrderHandler) SetTerminalSecret(s []byte) { h.terminalSecret = s }

// createOrderLineInput is a single line in the order create request body.
type createOrderLineInput struct {
	CatalogItemID uuid.UUID              `json:"catalog_item_id"`
	SKU           string                 `json:"sku"`
	Name          string                 `json:"name"`
	Category      string                 `json:"category,omitempty"` // item category name; drives KDS routing (kitchen vs bar)
	Quantity      float64                `json:"quantity"`
	UnitPrice     float64                `json:"unit_price"`
	TotalPrice    float64                `json:"total_price"`
	CourseNumber  int                    `json:"course_number"` // 0=fire immediately, 1=Starter, 2=Main, 3=Dessert
	Metadata      map[string]interface{} `json:"metadata"`
	// Per-line tax exactly as the till charged it (treasury-enriched catalog), so the server's
	// payable equals what the customer actually paid at the till.
	TaxStatus        string   `json:"tax_status,omitempty"`
	TaxCodeID        string   `json:"tax_code_id,omitempty"`
	PriceIncludesTax bool     `json:"price_includes_tax,omitempty"`
	TaxRate          *float64 `json:"tax_rate,omitempty"`
}

// createOrderInput is the body for POST /pos/orders.
type createOrderInput struct {
	OutletID       string                 `json:"outlet_id"`
	DeviceID       string                 `json:"device_id"`
	OrderNumber    string                 `json:"order_number"`
	ClientReference string                `json:"client_reference,omitempty"` // offline local_id — idempotency anchor
	OfflineCreatedAt *time.Time           `json:"offline_created_at,omitempty"` // device-clock time the sale was rung up offline
	Currency       string                 `json:"currency"`
	Lines          []createOrderLineInput `json:"lines"`
	Metadata       map[string]interface{} `json:"metadata"`
	PrescriptionID *string                `json:"prescription_id,omitempty"`
	AgeVerified    bool                   `json:"age_verified,omitempty"`   // cashier confirmed customer age for age-restricted items
	OrderSubtype   string                 `json:"order_subtype"`            // dine_in | takeaway | room_service | delivery | bar_tab | retail
	TableID        string                 `json:"table_id"`                 // hospitality dine-in table UUID
	CustomerPhone  string                 `json:"customer_phone,omitempty"` // loyalty auto-earn
	CustomerName   string                 `json:"customer_name,omitempty"`
	DiscountAmount float64                `json:"discount_amount,omitempty"`  // order-level discount (e.g. loyalty redemption)
	DiscountReason string                 `json:"discount_reason,omitempty"`  // free-text reason for a manual discount
	OrderTaxAmount float64                `json:"order_tax_amount,omitempty"` // manager quick-edit: order-level tax added on top of per-line tax
	Charges        map[string]float64     `json:"charges,omitempty"`          // manager quick-edit: additional costs (packaging/service/shipping)
	ApprovalToken  string                 `json:"approval_token,omitempty"`   // manager step-up token for an over-limit discount / order adjustment
	Source         string                 `json:"source,omitempty"`           // "pos_terminal" (default) | "back_office" (Add Sale flow)
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

	// Per-cashier scoping (REQ-007): view_own-only principals see their own orders (+ shared
	// active bills). Enforced server-side so direct API calls can't bypass the "My Sales" view.
	if ownPred, scoped := h.ownOrdersPredicate(r); scoped {
		filters = append(filters, ownPred)
	}

	// Business Location: an explicit ?outlet_id= query param wins (the All-Sales location
	// filter). "all" (admins) lists every outlet. Absent → scope to the header outlet context.
	if outletParam := q.Get("outlet_id"); outletParam != "" {
		if !strings.EqualFold(outletParam, "all") {
			if oid, parseErr := uuid.Parse(outletParam); parseErr == nil {
				filters = append(filters, posorder.OutletID(oid))
			}
		}
	} else if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
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
	// staff_id / user_id scope the list to orders created by a specific staff member.
	staffFilter := q.Get("staff_id")
	if staffFilter == "" {
		staffFilter = q.Get("user_id")
	}
	if staffFilter != "" {
		if staffUID, err := uuid.Parse(staffFilter); err == nil {
			filters = append(filters, posorder.UserID(staffUID))
		}
	}
	if orderNum := q.Get("order_number"); orderNum != "" {
		filters = append(filters, posorder.OrderNumberContainsFold(orderNum))
	}
	// Sources: pos_terminal vs back_office.
	if src := q.Get("source"); src != "" && !strings.EqualFold(src, "all") {
		filters = append(filters, posorder.Source(src))
	}
	// Date range on created_at (accepts RFC3339 or YYYY-MM-DD).
	if from := parseDateParam(q.Get("from"), false); from != nil {
		filters = append(filters, posorder.CreatedAtGTE(*from))
	}
	if to := parseDateParam(q.Get("to"), true); to != nil {
		filters = append(filters, posorder.CreatedAtLTE(*to))
	}
	// Customer: match phone or name (contains, case-insensitive).
	if cust := strings.TrimSpace(q.Get("customer")); cust != "" {
		filters = append(filters, posorder.Or(
			posorder.CustomerPhoneContainsFold(cust),
			posorder.CustomerNameContainsFold(cust),
		))
	}
	// Payment status — driven by the stored paid_total column (sum of completed payments,
	// maintained by the payments service) so the filter provably agrees with the per-row
	// badge from derivePaymentStatus: paid = settled in full, partial = 0 < paid < total,
	// due = nothing paid yet. Terminal statuses (refunded/voided/cancelled) show as their
	// own badge and are excluded from all three buckets.
	switch strings.ToLower(q.Get("payment_status")) {
	case "paid":
		filters = append(filters, posorder.Or(
			posorder.Status("completed"),
			posorder.And(
				posorder.StatusNotIn("refunded", "voided", "cancelled"),
				paidCoversTotal(),
			),
		))
	case "partial":
		filters = append(filters, posorder.And(
			posorder.StatusNotIn("completed", "refunded", "voided", "cancelled"),
			posorder.PaidTotalGT(0),
			paidBelowTotal(),
		))
	case "due", "unpaid":
		filters = append(filters, posorder.And(
			posorder.StatusNotIn("completed", "refunded", "voided", "cancelled"),
			posorder.PaidTotalLTE(0),
		))
	}
	// Payment method → resolve tenders of that type for the tenant, then match orders that
	// have a payment on one of them.
	if pm := strings.TrimSpace(q.Get("payment_method")); pm != "" && !strings.EqualFold(pm, "all") {
		tenderIDs, _ := h.client.Tender.Query().
			Where(tender.TenantID(tid), tender.TypeEQ(pm)).
			IDs(r.Context())
		if len(tenderIDs) > 0 {
			filters = append(filters, posorder.HasPaymentsWith(pospayment.TenderIDIn(tenderIDs...)))
		} else {
			// No matching tender → no orders can match this method.
			filters = append(filters, posorder.IDEQ(uuid.Nil))
		}
	}
	// Shipping status — stored in metadata.shipping_status (set by Edit Shipping).
	if ship := strings.TrimSpace(q.Get("shipping_status")); ship != "" && !strings.EqualFold(ship, "all") {
		filters = append(filters, predicate.POSOrder(func(s *sql.Selector) {
			s.Where(sqljson.ValueEQ(posorder.FieldMetadata, ship, sqljson.Path("shipping_status")))
		}))
	}
	// Subscriptions-only: orders flagged as subscription sales in metadata.
	if strings.EqualFold(q.Get("subscriptions"), "true") || q.Get("subscriptions") == "1" {
		filters = append(filters, predicate.POSOrder(func(s *sql.Selector) {
			s.Where(sqljson.ValueEQ(posorder.FieldMetadata, true, sqljson.Path("is_subscription")))
		}))
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

	items := h.enrichOrderList(r.Context(), tid, orderList)
	jsonOK(w, pagination.NewResponse(items, total, p))
}

// orderListItem wraps a POSOrder with the derived columns the All-Sales table needs
// (payment status/method, paid vs due, item count). The embedded *ent.POSOrder promotes
// all original fields + edges, so existing consumers (terminal, drafts) are unaffected.
type orderListItem struct {
	*ent.POSOrder
	ItemCount     int     `json:"item_count"`
	TotalPaid     float64 `json:"total_paid"`
	AmountDue     float64 `json:"amount_due"`
	PaymentStatus string  `json:"payment_status"` // paid | partial | due | refunded | voided | cancelled
	PaymentMethod string  `json:"payment_method"` // dominant tender type, or "multiple"
}

// enrichOrderList computes the derived display columns for a page of orders, resolving
// payment-method labels via a single batched Tender lookup (id → type).
func (h *POSOrderHandler) enrichOrderList(ctx context.Context, tenantID uuid.UUID, list []*ent.POSOrder) []orderListItem {
	// Collect every tender_id referenced by this page's payments, then resolve types once.
	tenderIDSet := map[uuid.UUID]struct{}{}
	for _, o := range list {
		for _, pay := range o.Edges.Payments {
			tenderIDSet[pay.TenderID] = struct{}{}
		}
	}
	tenderType := map[uuid.UUID]string{}
	if len(tenderIDSet) > 0 {
		ids := make([]uuid.UUID, 0, len(tenderIDSet))
		for id := range tenderIDSet {
			ids = append(ids, id)
		}
		if tenders, err := h.client.Tender.Query().Where(tender.IDIn(ids...)).All(ctx); err == nil {
			for _, t := range tenders {
				tenderType[t.ID] = t.Type
			}
		}
	}

	items := make([]orderListItem, 0, len(list))
	for _, o := range list {
		// paid_total is the stored sum of completed payments — the same value the
		// payment-status filter queries against, so badge and filter always agree.
		paid := o.PaidTotal
		methods := map[string]struct{}{}
		for _, pay := range o.Edges.Payments {
			if t, ok := tenderType[pay.TenderID]; ok && t != "" {
				methods[t] = struct{}{}
			}
		}
		due := o.TotalAmount - paid
		if due < 0 {
			due = 0
		}
		items = append(items, orderListItem{
			POSOrder:      o,
			ItemCount:     len(o.Edges.Lines),
			TotalPaid:     paid,
			AmountDue:     due,
			PaymentStatus: derivePaymentStatus(o.Status, o.TotalAmount, paid),
			PaymentMethod: dominantMethod(methods),
		})
	}
	return items
}

// paidCoversTotal matches orders whose stored paid_total settles the full total_amount
// (with the same 1-cent tolerance derivePaymentStatus uses). Zero-total orders are
// excluded — they display as "due", never "paid".
func paidCoversTotal() predicate.POSOrder {
	return predicate.POSOrder(func(s *sql.Selector) {
		s.Where(sql.And(
			sql.GT(s.C(posorder.FieldTotalAmount), 0),
			sql.ExprP(s.C(posorder.FieldPaidTotal)+" + 0.01 >= "+s.C(posorder.FieldTotalAmount)),
		))
	})
}

// paidBelowTotal matches orders whose stored paid_total does NOT settle total_amount.
func paidBelowTotal() predicate.POSOrder {
	return predicate.POSOrder(func(s *sql.Selector) {
		s.Where(sql.ExprP(s.C(posorder.FieldPaidTotal) + " + 0.01 < " + s.C(posorder.FieldTotalAmount)))
	})
}

// derivePaymentStatus maps an order's status + paid amount to a display payment status.
func derivePaymentStatus(status string, total, paid float64) string {
	switch status {
	case "refunded", "voided", "cancelled":
		return status
	}
	if status == "completed" || (total > 0 && paid+0.01 >= total) {
		return "paid"
	}
	if paid > 0 {
		return "partial"
	}
	return "due"
}

// dominantMethod returns the single tender type used, "multiple" if several, "" if none.
func dominantMethod(methods map[string]struct{}) string {
	switch len(methods) {
	case 0:
		return ""
	case 1:
		for m := range methods {
			return m
		}
	}
	return "multiple"
}

// parseDateParam parses a from/to query value as RFC3339 or YYYY-MM-DD. When endOfDay is
// true and a date-only value is given, it snaps to the end of that day (inclusive range).
func parseDateParam(v string, endOfDay bool) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return &t
	}
	return nil
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

	// Tenant (+ own-visibility) is the security boundary for a single-order read. The outlet
	// context is deliberately NOT applied here: the All-Sales list spans outlets
	// (outlet_id=all), and scoping this read to the caller's header/HQ outlet made every
	// cross-outlet row 404 when opened (Sell Details / Return-by-Invoice defect).
	whereArgs := []predicate.POSOrder{posorder.ID(orderID), posorder.TenantID(tid)}
	if ownPred, scoped := h.ownOrdersPredicate(r); scoped {
		whereArgs = append(whereArgs, ownPred)
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

// GetOrderByNumber handles GET /{tenantID}/pos/orders/by-number/{orderNumber} — used by the POS
// "Return by Invoice" flow to look up a prior sale by its order/invoice number.
func (h *POSOrderHandler) GetOrderByNumber(w http.ResponseWriter, r *http.Request) {
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

	orderNumber := chi.URLParam(r, "orderNumber")
	if orderNumber == "" {
		jsonError(w, "order_number required", http.StatusBadRequest)
		return
	}

	// Tenant-scoped only — no outlet predicate (a receipt from any outlet must resolve; see
	// GetOrder) and no view_own narrowing: Return-by-Invoice legitimately looks up sales rung
	// up by OTHER cashiers (a customer returns goods to whoever is at the till). Knowing the
	// exact receipt number is the lookup credential here.
	whereArgs := []predicate.POSOrder{posorder.OrderNumber(orderNumber), posorder.TenantID(tid)}
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
		h.log.Error("get order by number failed", zap.Error(err))
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

		// Metered limit: count this sale against max_orders_per_day. Over limit with no
		// overage opt-in → 402 with the structured limit body so pos-ui opens the extra-usage
		// modal. Exempt tokens and infra errors fail open (ReportUsage returns Allowed=true).
		exempt := false
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			exempt = claims.IsGatingExempt()
		}
		if !exempt {
			if dec := h.subsClient.ReportUsage(r.Context(), tenantID, subscriptions.MetricOrders, "pos-api", 1); !dec.Allowed {
				status := dec.Status
				if status == 0 {
					status = http.StatusPaymentRequired
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(dec.Body)
				return
			}
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

	// Age-restricted items: block unless the cashier has confirmed the customer's
	// age. Enforced server-side (defence in depth — the client age prompt can be
	// bypassed).
	if !input.AgeVerified {
		skus := make([]string, 0, len(input.Lines))
		for _, l := range input.Lines {
			if l.SKU != "" {
				skus = append(skus, l.SKU)
			}
		}
		if len(skus) > 0 {
			ageOverrides, _ := h.client.POSCatalogOverride.Query().
				Where(
					entoverride.TenantID(tid),
					entoverride.InventorySkuIn(skus...),
					entoverride.MinimumAgeGT(0),
				).All(r.Context())
			if len(ageOverrides) > 0 {
				blocked := make([]string, 0, len(ageOverrides))
				for _, o := range ageOverrides {
					blocked = append(blocked, o.InventorySku)
				}
				jsonError(w, "age verification required for: "+strings.Join(blocked, ", "), http.StatusUnprocessableEntity)
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

	// Resolve the outlet's discount/override limit + whether the caller may bypass
	// it (managers/admins), used by both the discount and price-override gates.
	maxPct := 100.0
	if outletID != uuid.Nil {
		if s, sErr := h.client.OutletSetting.Query().Where(outletsetting.OutletID(outletID)).Only(r.Context()); sErr == nil {
			maxPct = s.MaxDiscountPercent
		}
	}
	callerIsManager := overrideRoles[requesterRole(r)]
	if !callerIsManager {
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
			callerIsManager = claims.IsPlatformOwner || hasOverrideRole(claims.Roles)
		}
	}

	// Manual-discount gate: a discount above max_discount_percent requires a
	// manager step-up; over-limit discounts are recorded as order.discount_override.
	if input.DiscountAmount > 0 && !callerIsManager {
		var subtotal float64
		for _, l := range input.Lines {
			subtotal += l.TotalPrice
		}
		discountPct := 0.0
		if subtotal > 0 {
			discountPct = input.DiscountAmount / subtotal * 100
		}
		if discountPct > maxPct+0.001 {
			approverID, valid := uuid.Nil, false
			if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
				approverID, valid = verifyApprovalToken(input.ApprovalToken, "order.discount_override", h.terminalSecret)
			}
			if !valid {
				respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error": "manager approval required: discount exceeds the allowed limit",
					"approval_required": true, "action": "order.discount_override",
				})
				return
			}
			if h.auditSvc != nil {
				oid := outletID
				amt := input.DiscountAmount
				h.auditSvc.Record(r.Context(), audit.Entry{
					TenantID: tid, OutletID: &oid, ActorUserID: userID, ApproverID: &approverID,
					Action: "order.discount_override", EntityType: "pos_order", Reason: input.DiscountReason, Amount: &amt,
					After: map[string]any{"discount_percent": discountPct, "max_percent": maxPct},
				})
			}
		}
	}

	// Order-adjustment gate: order-level tax edits and additional charges (packaging/service/
	// shipping) are a manager/admin quick-edit. Non-managers need a manager step-up token
	// (order.adjustment), mirroring the discount gate; adjustments are audited.
	chargesSum := 0.0
	for _, v := range input.Charges {
		if v > 0 {
			chargesSum += v
		}
	}
	if (input.OrderTaxAmount > 0 || chargesSum > 0) && !callerIsManager {
		approverID, valid := uuid.Nil, false
		if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
			approverID, valid = verifyApprovalToken(input.ApprovalToken, "order.adjustment", h.terminalSecret)
		}
		if !valid {
			respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":             "manager approval required: order tax / additional charges are a manager adjustment",
				"approval_required": true, "action": "order.adjustment",
			})
			return
		}
		if h.auditSvc != nil {
			oid := outletID
			amt := input.OrderTaxAmount + chargesSum
			h.auditSvc.Record(r.Context(), audit.Entry{
				TenantID: tid, OutletID: &oid, ActorUserID: userID, ApproverID: &approverID,
				Action: "order.adjustment", EntityType: "pos_order", Amount: &amt,
				After: map[string]any{"order_tax_amount": input.OrderTaxAmount, "charges": input.Charges},
			})
		}
	}

	// Per-line price-override gate: a line whose unit_price is marked down from its
	// catalog price (metadata.original_price) by more than max_discount_percent
	// requires a manager step-up, recorded as price.override. Markups are allowed.
	if !callerIsManager {
		needApproval := false
		type ovLine struct {
			sku        string
			orig, unit float64
			dev        float64
		}
		var overrides []ovLine
		for _, l := range input.Lines {
			orig := readFloatMeta(l.Metadata, "original_price")
			if !metaBool(l.Metadata, "price_override") || orig <= 0 || l.UnitPrice >= orig {
				continue
			}
			dev := (orig - l.UnitPrice) / orig * 100
			overrides = append(overrides, ovLine{sku: l.SKU, orig: orig, unit: l.UnitPrice, dev: dev})
			if dev > maxPct+0.001 {
				needApproval = true
			}
		}
		if needApproval {
			approverID, valid := uuid.Nil, false
			if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
				approverID, valid = verifyApprovalToken(input.ApprovalToken, "price.override", h.terminalSecret)
			}
			if !valid {
				respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error": "manager approval required: a line price markdown exceeds the allowed limit",
					"approval_required": true, "action": "price.override",
				})
				return
			}
			if h.auditSvc != nil {
				oid := outletID
				for _, o := range overrides {
					amt := o.orig - o.unit
					h.auditSvc.Record(r.Context(), audit.Entry{
						TenantID: tid, OutletID: &oid, ActorUserID: userID, ApproverID: &approverID,
						Action: "price.override", EntityType: "pos_order_line", EntityID: o.sku, Amount: &amt,
						After: map[string]any{"original_price": o.orig, "new_price": o.unit, "deviation_percent": o.dev, "max_percent": maxPct},
					})
				}
			}
		}
	}

	// Min/Max selling-price hard guardrail: a line priced outside the item's configured
	// [min,max] band (carried on the catalog item, echoed by the till in line metadata as
	// min_price / max_price) is blocked unless a manager approves it (price.override). This is
	// absolute — independent of the discount-percent gate above — and covers both under-min
	// markdowns and over-max markups. Managers bypass (override authority).
	if !callerIsManager {
		type bandLine struct {
			sku              string
			price, min, max  float64
		}
		var outOfBand []bandLine
		for _, l := range input.Lines {
			min := readFloatMeta(l.Metadata, "min_price")
			max := readFloatMeta(l.Metadata, "max_price")
			if (min > 0 && l.UnitPrice < min) || (max > 0 && l.UnitPrice > max) {
				outOfBand = append(outOfBand, bandLine{sku: l.SKU, price: l.UnitPrice, min: min, max: max})
			}
		}
		if len(outOfBand) > 0 {
			approverID, valid := uuid.Nil, false
			if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
				approverID, valid = verifyApprovalToken(input.ApprovalToken, "price.override", h.terminalSecret)
			}
			if !valid {
				respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":             "manager approval required: a line price is outside the allowed min/max",
					"approval_required": true, "action": "price.override",
				})
				return
			}
			if h.auditSvc != nil {
				oid := outletID
				for _, o := range outOfBand {
					price := o.price
					h.auditSvc.Record(r.Context(), audit.Entry{
						TenantID: tid, OutletID: &oid, ActorUserID: userID, ApproverID: &approverID,
						Action: "price.override", EntityType: "pos_order_line", EntityID: o.sku, Reason: "out_of_band", Amount: &price,
						After: map[string]any{"unit_price": o.price, "min_price": o.min, "max_price": o.max},
					})
				}
			}
		}
	}

	// Convert handler input to service request
	lines := make([]orders.OrderLineInput, len(input.Lines))
	for i, l := range input.Lines {
		lines[i] = orders.OrderLineInput{
			CatalogItemID:    l.CatalogItemID,
			SKU:              l.SKU,
			Name:             l.Name,
			Category:         l.Category,
			Quantity:         l.Quantity,
			UnitPrice:        l.UnitPrice,
			TotalPrice:       l.TotalPrice,
			CourseNumber:     l.CourseNumber,
			Metadata:         l.Metadata,
			TaxStatus:        l.TaxStatus,
			TaxCodeID:        l.TaxCodeID,
			PriceIncludesTax: l.PriceIncludesTax,
			TaxRate:          l.TaxRate,
		}
	}

	order, err := h.orderSvc.CreateOrder(r.Context(), orders.CreateOrderRequest{
		TenantID:       tid,
		OutletID:       outletID,
		DeviceID:       deviceID,
		UserID:           userID,
		OrderNumber:      input.OrderNumber,
		ClientReference:  input.ClientReference,
		OfflineCreatedAt: input.OfflineCreatedAt,
		Currency:         input.Currency,
		Lines:          lines,
		Metadata:       input.Metadata,
		OrderSubtype:   input.OrderSubtype,
		TableID:        input.TableID,
		CustomerPhone:  input.CustomerPhone,
		CustomerName:   input.CustomerName,
		DiscountAmount: input.DiscountAmount,
		OrderTaxAmount: input.OrderTaxAmount,
		Charges:        input.Charges,
		Source:         input.Source,
	})
	if err != nil {
		if errors.Is(err, orders.ErrInvalidOrderSubtype) {
			jsonError(w, "invalid order_subtype: must be one of dine_in, takeaway, room_service, delivery, bar_tab, retail", http.StatusBadRequest)
			return
		}
		h.log.Error("create order failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(order)
}

// shippingInput is the body for PATCH /pos/orders/{orderID}/shipping (Edit Shipping action).
type shippingInput struct {
	ShippingStatus  string   `json:"shipping_status,omitempty"`  // ordered|packed|shipped|delivered|cancelled
	ShippingAddress string   `json:"shipping_address,omitempty"`
	ShippingAmount  *float64 `json:"shipping_amount,omitempty"`
	TrackingNumber  string   `json:"tracking_number,omitempty"`
	DeliveryPerson  string   `json:"delivery_person,omitempty"`
	DeliveryPhone   string   `json:"delivery_phone,omitempty"`
}

// UpdateShipping handles PATCH /{tenantID}/pos/orders/{orderID}/shipping — the All-Sales
// "Edit Shipping" action. Shipping details live in the order metadata (no dedicated columns);
// the All-Sales "Shipping Status" filter reads metadata.shipping_status. For delivery orders
// the frontend separately dispatches to logistics via the existing assign-rider flow.
func (h *POSOrderHandler) UpdateShipping(w http.ResponseWriter, r *http.Request) {
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
	var input shippingInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	if input.ShippingStatus != "" {
		meta["shipping_status"] = input.ShippingStatus
	}
	if input.ShippingAddress != "" {
		meta["shipping_address"] = input.ShippingAddress
	}
	if input.ShippingAmount != nil {
		meta["shipping_amount"] = *input.ShippingAmount
	}
	if input.TrackingNumber != "" {
		meta["tracking_number"] = input.TrackingNumber
	}
	if input.DeliveryPerson != "" {
		meta["delivery_person"] = input.DeliveryPerson
	}
	if input.DeliveryPhone != "" {
		meta["delivery_phone"] = input.DeliveryPhone
	}

	updated, err := h.client.POSOrder.UpdateOneID(orderID).SetMetadata(meta).Save(r.Context())
	if err != nil {
		h.log.Error("update shipping failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// notifySaleInput optionally redirects the sale notification to a specific phone/email.
type notifySaleInput struct {
	Phone string `json:"phone,omitempty"`
	Email string `json:"email,omitempty"`
}

// NotifySale handles POST /{tenantID}/pos/orders/{orderID}/notify — the All-Sales
// "New Sale Notification" action. Publishes pos.sale.notification_requested for
// notifications-service to deliver the receipt/invoice to the customer.
func (h *POSOrderHandler) NotifySale(w http.ResponseWriter, r *http.Request) {
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
	var input notifySaleInput
	_ = json.NewDecoder(r.Body).Decode(&input) // body optional

	if _, err := h.orderSvc.RequestSaleNotification(r.Context(), tid, orderID, input.Phone, input.Email); err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("notify sale failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"status": "queued"})
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
		Reason        string `json:"reason"`
		ApprovalToken string `json:"approval_token"`
		// VoidCode is the one-time, order-scoped code a manager generated and shared with the
		// cashier (the "manager not around" flow) — an alternative to the live PIN/card step-up.
		VoidCode string `json:"void_code"`
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

	// Capture the manager approver. Two ways:
	//  1) a live step-up approval token (manager scanned a card / typed a PIN at the terminal), or
	//  2) a one-time void code the manager generated remotely and shared with the cashier.
	var approverID *uuid.UUID
	if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
		if aid, valid := verifyApprovalToken(input.ApprovalToken, "order.void", h.terminalSecret); valid {
			approverID = &aid
		} else {
			jsonError(w, "invalid or expired approval", http.StatusForbidden)
			return
		}
	} else if input.VoidCode != "" {
		if aid, valid := h.redeemVoidCode(r.Context(), tid, orderID, input.VoidCode); valid {
			approverID = &aid
		} else {
			jsonError(w, "invalid or expired void code", http.StatusForbidden)
			return
		}
	}

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

	// A finalized sale has already been posted to the ledger and transmitted to KRA eTIMS
	// (pos.sale.finalized). Voiding it here would only flip the status — leaving the GL entry
	// and the eTIMS receipt un-reversed. Such sales must be reversed via a return/refund, which
	// posts the ledger reversal AND transmits an eTIMS credit note (rcptTyCd=R).
	if order.Status == "completed" || order.Status == "paid" || order.Status == "closed" {
		jsonError(w, "a finalized sale cannot be voided — issue a refund/return instead so the ledger and KRA eTIMS are properly reversed", http.StatusConflict)
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

	if h.auditSvc != nil {
		oid := order.OutletID
		amt := order.TotalAmount
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: callerID,
			ApproverID:  approverID,
			Action:      "order.void",
			EntityType:  "pos_order",
			EntityID:    orderID.String(),
			Reason:      input.Reason,
			Amount:      &amt,
			Before:      map[string]any{"status": order.Status, "order_number": order.OrderNumber},
			After:       map[string]any{"status": "voided"},
		})
	}
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
			CatalogItemID:    l.CatalogItemID,
			SKU:              l.SKU,
			Name:             l.Name,
			Category:         l.Category,
			Quantity:         l.Quantity,
			UnitPrice:        l.UnitPrice,
			TotalPrice:       l.TotalPrice,
			CourseNumber:     l.CourseNumber,
			Metadata:         l.Metadata,
			TaxStatus:        l.TaxStatus,
			TaxCodeID:        l.TaxCodeID,
			PriceIncludesTax: l.PriceIncludesTax,
			TaxRate:          l.TaxRate,
		}
	}

	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	result, err := h.orderSvc.AddOrderLines(r.Context(), tid, tenantSlug, orderID, lines)
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

