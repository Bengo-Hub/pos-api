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

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entfacility "github.com/bengobox/pos-service/internal/ent/facility"
	"github.com/bengobox/pos-service/internal/ent/outletsetting"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/ent/tender"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/inventory"
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
	// inventoryClient propagates order-line price corrections to the inventory catalog
	// (EditOrderLine's update_catalog_price option). Optional — nil skips propagation.
	inventoryClient *inventory.Client
}

func NewPOSOrderHandler(log *zap.Logger, client *ent.Client, orderSvc *orders.Service, subsClient *subscriptions.Client) *POSOrderHandler {
	return &POSOrderHandler{log: log, client: client, orderSvc: orderSvc, subsClient: subsClient}
}

// SetAuditService wires the centralized audit trail for void/line-removal events.
func (h *POSOrderHandler) SetAuditService(a *audit.Service) { h.auditSvc = a }

// SetInventoryClient wires the inventory S2S client used to propagate order-line price
// corrections to the catalog (EditOrderLine's update_catalog_price option).
func (h *POSOrderHandler) SetInventoryClient(c *inventory.Client) { h.inventoryClient = c }

// SetRBAC wires the local RBAC fallback used by the per-cashier visibility scoping.
func (h *POSOrderHandler) SetRBAC(rbac outletmw.PermissionChecker) { h.rbac = rbac }

// ownOrdersPredicate returns the visibility predicate for principals limited to their OWN
// sales (REQ-007): users holding pos.orders.view_own but none of view/change/manage see
// only orders they created, plus shared ACTIVE orders (open / pending_payment) so till
// hand-offs — a cashier settling a waiter's open bill — keep working. Full-view principals
// (and superusers/platform owners, via HasServicePermission's bypass) get no restriction.
func (h *POSOrderHandler) ownOrdersPredicate(r *http.Request) (predicate.POSOrder, bool) {
	// Shared with the All-Sales export (report_all_sales.go) so list and export scope identically.
	return ownOrdersScope(r, h.rbac)
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

// lineModifierWire is the shape pos-ui sends under metadata.modifiers — one entry per
// selected modifier option, resolved to its catalog name/price at selection time (see
// terminal-context.tsx SelectedModifierDetail). Kept private to this file; orders.Service
// only knows the resolved orders.LineModifierInput shape.
type lineModifierWire struct {
	GroupID         string  `json:"group_id"`
	GroupName       string  `json:"group_name"`
	OptionID        string  `json:"option_id"`
	OptionName      string  `json:"option_name"`
	PriceAdjustment float64 `json:"price_adjustment"`
}

// parseLineModifiers decodes metadata["modifiers"] into structured LineModifierInput rows.
// Best-effort: a malformed/missing entry is skipped rather than failing the whole order —
// the price is already baked into the line's unit_price/total_price regardless, so a
// modifier that fails to parse only loses its stock-deduction/audit row, not the sale.
func parseLineModifiers(meta map[string]interface{}) []orders.LineModifierInput {
	raw, ok := meta["modifiers"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var wire []lineModifierWire
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return nil
	}
	out := make([]orders.LineModifierInput, 0, len(wire))
	for _, w := range wire {
		optID, err := uuid.Parse(w.OptionID)
		if err != nil {
			continue
		}
		out = append(out, orders.LineModifierInput{
			GroupID:         w.GroupID,
			GroupName:       w.GroupName,
			OptionID:        optID,
			OptionName:      w.OptionName,
			PriceAdjustment: w.PriceAdjustment,
		})
	}
	return out
}

// createOrderInput is the body for POST /pos/orders.
type createOrderInput struct {
	OutletID         string                 `json:"outlet_id"`
	DeviceID         string                 `json:"device_id"`
	OrderNumber      string                 `json:"order_number"`
	ClientReference  string                 `json:"client_reference,omitempty"`   // offline local_id — idempotency anchor
	OfflineCreatedAt *time.Time             `json:"offline_created_at,omitempty"` // device-clock time the sale was rung up offline
	Currency         string                 `json:"currency"`
	Lines            []createOrderLineInput `json:"lines"`
	Metadata         map[string]interface{} `json:"metadata"`
	PrescriptionID   *string                `json:"prescription_id,omitempty"`
	AgeVerified      bool                   `json:"age_verified,omitempty"`   // cashier confirmed customer age for age-restricted items
	OrderSubtype     string                 `json:"order_subtype"`            // dine_in | takeaway | room_service | delivery | bar_tab | retail
	TableID          string                 `json:"table_id"`                 // hospitality dine-in table UUID
	CustomerPhone    string                 `json:"customer_phone,omitempty"` // loyalty auto-earn
	CustomerName     string                 `json:"customer_name,omitempty"`
	DiscountAmount   float64                `json:"discount_amount,omitempty"`  // order-level discount (e.g. loyalty redemption)
	DiscountReason   string                 `json:"discount_reason,omitempty"`  // free-text reason for a manual discount
	OrderTaxAmount   float64                `json:"order_tax_amount,omitempty"` // manager quick-edit: order-level tax added on top of per-line tax
	Charges          map[string]float64     `json:"charges,omitempty"`          // manager quick-edit: additional costs (packaging/service/shipping)
	ApprovalToken    string                 `json:"approval_token,omitempty"`   // manager step-up token for an over-limit discount / order adjustment
	ApprovalCode     string                 `json:"approval_code,omitempty"`    // manager-generated one-time code (alternative to a live step-up token)
	Source           string                 `json:"source,omitempty"`           // "pos_terminal" (default) | "back_office" (Add Sale flow)
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

	filters := []predicate.POSOrder{posorder.TenantID(tid)}

	// Per-cashier scoping (REQ-007): view_own-only principals see their own orders (+ shared
	// active bills). Enforced server-side so direct API calls can't bypass the "My Sales" view.
	if ownPred, scoped := h.ownOrdersPredicate(r); scoped {
		filters = append(filters, ownPred)
	}

	// Every user-facing filter (outlet, status, staff, invoice search, source, effective-date
	// range, customer, payment status/method, shipping, subscriptions, total range, KDS station,
	// category) is built by allSalesOrderFilters — shared verbatim with the All-Sales export
	// (report_all_sales.go AllSalesDocument) so the exported document always contains exactly
	// the rows this list shows.
	loc := tenantLocation(r.Context(), h.client, tid)
	filters = append(filters, allSalesOrderFilters(r, h.client, tid, loc)...)

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
	PaymentStatus string  `json:"payment_status"` // paid | partial | due | overdue | refunded | voided | cancelled
	PaymentMethod string  `json:"payment_method"` // dominant tender type, or "multiple"
	// Sell-return rollup (rejected returns excluded): lets the list flag returned
	// sales (red return arrow) and show the returned amount without N+1 lookups.
	ReturnCount  int     `json:"return_count"`
	ReturnTotal  float64 `json:"return_total"`
	ReturnStatus string  `json:"return_status,omitempty"` // pending | approved | completed (most-advanced across the order's returns)
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

	// Batched sell-return rollup: one query for the whole page (mirrors the tender batch
	// above). Rejected returns are excluded — a rejected request never happened financially.
	type returnAgg struct {
		count  int
		total  float64
		status string
	}
	returnRank := map[posreturn.Status]int{posreturn.StatusPending: 1, posreturn.StatusApproved: 2, posreturn.StatusCompleted: 3}
	returnsByOrder := map[uuid.UUID]*returnAgg{}
	if len(list) > 0 {
		orderIDs := make([]uuid.UUID, 0, len(list))
		for _, o := range list {
			orderIDs = append(orderIDs, o.ID)
		}
		rets, err := h.client.POSReturn.Query().
			Where(
				posreturn.TenantID(tenantID),
				posreturn.OrderIDIn(orderIDs...),
				posreturn.StatusNEQ(posreturn.StatusRejected),
			).
			All(ctx)
		if err != nil {
			h.log.Warn("order list return rollup failed", zap.Error(err))
		}
		for _, ret := range rets {
			agg := returnsByOrder[ret.OrderID]
			if agg == nil {
				agg = &returnAgg{}
				returnsByOrder[ret.OrderID] = agg
			}
			agg.count++
			agg.total += ret.RefundAmount
			if returnRank[ret.Status] > returnRank[posreturn.Status(agg.status)] {
				agg.status = string(ret.Status)
			}
		}
	}

	items := make([]orderListItem, 0, len(list))
	for _, o := range list {
		// paid_total is the stored sum of completed payments — the same value the
		// payment-status filter queries against, so badge and filter always agree.
		paid := o.PaidTotal
		// The displayed method comes from what was actually SETTLED. The terminal stamps the real
		// method on payment_data.method (the tender is a shared generic row, so its type is not the
		// method); fall back to the tender type only for legacy per-method-tender setups.
		methods := map[string]struct{}{}
		for _, pay := range o.Edges.Payments {
			if !strings.EqualFold(pay.Status, "completed") {
				continue // only settled tenders define the method shown against a paid sale
			}
			m := ""
			if pay.PaymentData != nil {
				m, _ = pay.PaymentData["method"].(string)
			}
			if m == "" {
				m = tenderType[pay.TenderID]
			}
			if m != "" {
				methods[m] = struct{}{}
			}
		}
		due := o.TotalAmount - paid
		if due < 0 {
			due = 0
		}
		// Badge mirrors the "overdue" filter: a still-owing sale past its stamped
		// payment_due_date reads "overdue" instead of due/partial. A credit sale that was
		// later settled in full at the till reads "paid" (never overdue).
		ps := derivePaymentStatus(o.Status, o.TotalAmount, paid, isOnAccount(o.Metadata))
		if (ps == "due" || ps == "partial") && isOrderOverdue(o.Metadata) {
			ps = "overdue"
		}
		item := orderListItem{
			POSOrder:      o,
			ItemCount:     len(o.Edges.Lines),
			TotalPaid:     paid,
			AmountDue:     due,
			PaymentStatus: ps,
			PaymentMethod: dominantMethod(methods),
		}
		if agg := returnsByOrder[o.ID]; agg != nil {
			item.ReturnCount = agg.count
			item.ReturnTotal = agg.total
			item.ReturnStatus = agg.status
		}
		items = append(items, item)
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

// pastPaymentDueDate matches orders whose metadata.payment_due_date (RFC3339, stamped on
// credit sales from the customer's payment period) is in the past. RFC3339 strings compare
// lexicographically, so a plain string comparison is chronologically correct; orders without
// a due date never match.
func pastPaymentDueDate() predicate.POSOrder {
	now := time.Now().Format(time.RFC3339)
	return predicate.POSOrder(func(s *sql.Selector) {
		s.Where(sqljson.ValueLT(posorder.FieldMetadata, now, sqljson.Path("payment_due_date")))
	})
}

// onAccountOrder matches credit sales settled on account (metadata.on_account, stamped by
// recordCreditSale) — at the till they read "paid", but their money is a treasury AR debt.
func onAccountOrder() predicate.POSOrder {
	return predicate.POSOrder(func(s *sql.Selector) {
		s.Where(sqljson.ValueEQ(posorder.FieldMetadata, true, sqljson.Path("on_account")))
	})
}

// isOrderOverdue reports whether an order is past its stamped payment_due_date. Used to
// upgrade the display payment status to "overdue" for still-owing or on-account sales.
func isOrderOverdue(meta map[string]any) bool {
	raw, ok := meta["payment_due_date"].(string)
	if !ok || raw == "" {
		return false
	}
	due, err := time.Parse(time.RFC3339, raw)
	return err == nil && due.Before(time.Now())
}

func isOnAccount(meta map[string]any) bool {
	v, ok := meta["on_account"].(bool)
	return ok && v
}

// derivePaymentStatus maps an order's status + paid amount to a display payment status.
// onAccount marks a credit sale: completion means the goods left, NOT that cash was banked —
// paid_total excludes the on-account tender, so the sale reads due/partial (and "overdue"
// past its due date, upgraded by the caller) until the money is actually collected.
func derivePaymentStatus(status string, total, paid float64, onAccount bool) string {
	switch status {
	case "refunded", "voided", "cancelled":
		return status
	}
	if total > 0 && paid+0.01 >= total {
		return "paid"
	}
	if onAccount {
		if paid > 0 {
			return "partial"
		}
		return "due"
	}
	if status == "completed" {
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

// parseDateParam parses a from/to query value as RFC3339 or YYYY-MM-DD. Date-only
// values are interpreted in loc (the tenant timezone) so the day boundary matches
// the tenant's wall clock; when endOfDay is true a date-only value snaps to the end
// of that local day (inclusive range).
func parseDateParam(v string, endOfDay bool, loc *time.Location) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if loc == nil {
		loc = time.UTC
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t
	}
	// datetime-local (HTML <input type="datetime-local">) — carries a wall-clock time but no
	// zone, so interpret it in the outlet/tenant timezone. Seconds are optional. When a time is
	// present we DON'T snap `to` to end-of-day (the caller asked for a precise minute boundary).
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return &t
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", v, loc); err == nil {
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

	// Resolve the outlet's discount/override limits + pricing policy + whether the caller
	// may bypass them (managers/admins), used by the discount and price-override gates.
	maxPct := 100.0
	maxAmount := 0.0                 // 0 = no absolute-amount limit
	allowAboveBase := true           // cashiers may raise a line price above base (up-sell)
	requireApprovalBelowBase := true // selling below base needs a manager step-up
	if outletID != uuid.Nil {
		if s, sErr := h.client.OutletSetting.Query().Where(outletsetting.OutletID(outletID)).Only(r.Context()); sErr == nil {
			maxPct = s.MaxDiscountPercent
			maxAmount = s.MaxDiscountAmount
			allowAboveBase = s.AllowPriceAboveBase
			requireApprovalBelowBase = s.RequireApprovalBelowBase
		}
	}
	callerIsManager := overrideRoles[requesterRole(r)]
	if !callerIsManager {
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
			callerIsManager = claims.IsPlatformOwner || hasOverrideRole(claims.Roles)
		}
	}

	// Manual-discount gate: a discount above max_discount_percent OR above the absolute
	// max_discount_amount (when configured) requires a manager step-up; over-limit
	// discounts are recorded as order.discount_override.
	if input.DiscountAmount > 0 && !callerIsManager {
		var subtotal float64
		for _, l := range input.Lines {
			subtotal += l.TotalPrice
		}
		discountPct := 0.0
		if subtotal > 0 {
			discountPct = input.DiscountAmount / subtotal * 100
		}
		overAmount := maxAmount > 0 && input.DiscountAmount > maxAmount+0.001
		if discountPct > maxPct+0.001 || overAmount {
			approverID, valid := uuid.Nil, false
			if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
				approverID, valid = verifyApprovalToken(input.ApprovalToken, "order.discount_override", h.terminalSecret)
			}
			if !valid && input.ApprovalCode != "" {
				approverID, valid = redeemActionApprovalCode(r.Context(), h.client, h.log, tid, outletID, "order.discount_override", input.ApprovalCode)
			}
			if !valid {
				respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":             "manager approval required: discount exceeds the allowed limit",
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
					After: map[string]any{"discount_percent": discountPct, "max_percent": maxPct, "max_amount": maxAmount},
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
		if !valid && input.ApprovalCode != "" {
			approverID, valid = redeemActionApprovalCode(r.Context(), h.client, h.log, tid, outletID, "order.adjustment", input.ApprovalCode)
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

	// Per-line price-override gate, driven by the outlet's PRICING POLICY:
	//  - below base (metadata.original_price): needs a manager step-up while
	//    require_approval_below_base is ON (the default) — cashiers never markdown on
	//    their own authority; toggling it OFF allows free markdowns.
	//  - above base: allowed by default (allow_price_above_base — the retail/pharmacy
	//    negotiated up-sell); toggling it OFF makes markups need the same step-up.
	// The outlet's max_discount_percent governs the separate ORDER-level discount gate
	// above, not per-line edits. The gate keys off original_price alone — a client
	// "forgetting" the price_override flag no longer bypasses it.
	if !callerIsManager {
		needApproval := false
		type ovLine struct {
			sku        string
			orig, unit float64
			dev        float64
		}
		var overrides []ovLine
		for _, l := range input.Lines {
			// Non-billable lines are zeroed server-side by design — a zero price is not a markdown.
			if metaBool(l.Metadata, "non_billable") {
				continue
			}
			orig := readFloatMeta(l.Metadata, "original_price")
			if orig <= 0 {
				continue
			}
			below := l.UnitPrice < orig-0.005
			above := l.UnitPrice > orig+0.005
			if (below && requireApprovalBelowBase) || (above && !allowAboveBase) {
				dev := (orig - l.UnitPrice) / orig * 100
				overrides = append(overrides, ovLine{sku: l.SKU, orig: orig, unit: l.UnitPrice, dev: dev})
				needApproval = true
			}
		}
		if needApproval {
			approverID, valid := uuid.Nil, false
			if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
				approverID, valid = verifyApprovalToken(input.ApprovalToken, "price.override", h.terminalSecret)
			}
			if !valid && input.ApprovalCode != "" {
				approverID, valid = redeemActionApprovalCode(r.Context(), h.client, h.log, tid, outletID, "price.override", input.ApprovalCode)
			}
			if !valid {
				respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":             "manager approval required: selling below the preset price needs a manager",
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
			sku             string
			price, min, max float64
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
			if !valid && input.ApprovalCode != "" {
				approverID, valid = redeemActionApprovalCode(r.Context(), h.client, h.log, tid, outletID, "price.override", input.ApprovalCode)
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
			Modifiers:        parseLineModifiers(l.Metadata),
			TaxStatus:        l.TaxStatus,
			TaxCodeID:        l.TaxCodeID,
			PriceIncludesTax: l.PriceIncludesTax,
			TaxRate:          l.TaxRate,
		}
	}

	order, err := h.orderSvc.CreateOrder(r.Context(), orders.CreateOrderRequest{
		TenantID:         tid,
		OutletID:         outletID,
		DeviceID:         deviceID,
		UserID:           userID,
		OrderNumber:      input.OrderNumber,
		ClientReference:  input.ClientReference,
		OfflineCreatedAt: input.OfflineCreatedAt,
		Currency:         input.Currency,
		Lines:            lines,
		Metadata:         input.Metadata,
		OrderSubtype:     input.OrderSubtype,
		TableID:          input.TableID,
		CustomerPhone:    input.CustomerPhone,
		CustomerName:     input.CustomerName,
		DiscountAmount:   input.DiscountAmount,
		OrderTaxAmount:   input.OrderTaxAmount,
		Charges:          input.Charges,
		Source:           input.Source,
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

	h.autoAssignFacilityBookingsForOrder(r.Context(), order, lines)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(order)
}

// autoAssignFacilityBookingsForOrder is the "ring up + auto-assign" side effect of a completed
// sale: for each order line that resolves to a Facility (Facility.InventoryItemID ==
// line.CatalogItemID — the same inventory SERVICE item link the Facilities admin form writes),
// it creates a FacilityBooking consuming line.Quantity seats/guests against that facility for
// today, linked back via PosOrderID. This is how selling a co-working day-pass at the till
// becomes a tracked, capacity-counted assignment with no separate front-desk step.
//
// Deliberately best-effort and non-blocking: a completed, PAID sale must never be rejected or
// rolled back because of a bookkeeping side-table write. If this oversells a shared facility it
// logs a warning (an ops signal) rather than failing the sale — strict capacity rejection only
// happens on the explicit "Assign Space" flow (HotelHandler.BookFacility), which front-desk uses
// for time-boxed reservations. No-ops silently when the tenant lacks the facility_booking
// feature or no line matches a Facility (the overwhelmingly common case for ordinary sales).
func (h *POSOrderHandler) autoAssignFacilityBookingsForOrder(ctx context.Context, order *ent.POSOrder, lines []orders.OrderLineInput) {
	claims, ok := authclient.ClaimsFromContext(ctx)
	if !ok || !claims.HasFeature(subscriptions.FeatureFacilityBooking) {
		return
	}
	for _, line := range lines {
		if line.CatalogItemID == uuid.Nil || line.Quantity <= 0 {
			continue
		}
		fac, err := h.client.Facility.Query().
			Where(
				entfacility.TenantID(order.TenantID),
				entfacility.InventoryItemID(line.CatalogItemID),
				entfacility.IsActive(true),
			).
			Only(ctx)
		if err != nil {
			continue // not a bookable-space line — true for nearly every order line
		}

		seats := int(line.Quantity)
		if seats < 1 {
			seats = 1
		}
		guestName := "Walk-in"
		if order.CustomerName != nil && *order.CustomerName != "" {
			guestName = *order.CustomerName
		}
		phone := ""
		if order.CustomerPhone != nil {
			phone = *order.CustomerPhone
		}
		sessionDate := time.Now()

		booking, err := h.client.FacilityBooking.Create().
			SetTenantID(order.TenantID).
			SetFacilityID(fac.ID).
			SetOutletID(order.OutletID).
			SetGuestName(guestName).
			SetPhone(phone).
			SetSessionDate(sessionDate).
			SetGuestsCount(seats).
			SetSeats(seats).
			SetAmount(line.TotalPrice).
			SetBookedBy(order.UserID).
			SetPosOrderID(order.ID).
			SetNotes("Auto-assigned from POS sale " + order.OrderNumber).
			Save(ctx)
		if err != nil {
			h.log.Warn("auto-assign facility booking failed",
				zap.Error(err), zap.String("order_id", order.ID.String()), zap.String("facility_id", fac.ID.String()))
			continue
		}

		if fac.BookingMode == entfacility.BookingModeShared && fac.Capacity > 0 {
			booked := 0
			for _, b := range sameDayConfirmedFacilityBookings(ctx, h.client, order.TenantID, fac.ID, sessionDate) {
				s := b.Seats
				if s < 1 {
					s = 1
				}
				booked += s
			}
			if booked > fac.Capacity {
				h.log.Warn("facility oversold by auto-assigned booking",
					zap.String("facility_id", fac.ID.String()), zap.String("booking_id", booking.ID.String()),
					zap.Int("booked_seats", booked), zap.Int("capacity", fac.Capacity))
			}
		}
	}
}

// shippingInput is the body for PATCH /pos/orders/{orderID}/shipping (Edit Shipping action).
type shippingInput struct {
	ShippingStatus  string   `json:"shipping_status,omitempty"` // ordered|packed|shipped|delivered|cancelled
	ShippingAddress string   `json:"shipping_address,omitempty"`
	ShippingDetails string   `json:"shipping_details,omitempty"` // courier/vehicle/instructions free text
	ShippingAmount  *float64 `json:"shipping_amount,omitempty"`
	TrackingNumber  string   `json:"tracking_number,omitempty"`
	DeliveredTo     string   `json:"delivered_to,omitempty"`
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
	if input.ShippingDetails != "" {
		meta["shipping_details"] = input.ShippingDetails
	}
	if input.TrackingNumber != "" {
		meta["tracking_number"] = input.TrackingNumber
	}
	if input.DeliveredTo != "" {
		meta["delivered_to"] = input.DeliveredTo
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

	// The kitchen must stop preparing a voided bill: void any of its still-open KDS tickets so
	// they drop off the live board (previously they lingered until staff voided each by hand).
	h.orderSvc.VoidKDSTicketsForOrder(r.Context(), tid, orderID)

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
			Modifiers:        parseLineModifiers(l.Metadata),
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
