package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/layawaypayment"
	"github.com/bengobox/pos-service/internal/ent/layawayplan"
	"github.com/bengobox/pos-service/internal/ent/posorder"
)

// LayawayHandler handles layaway plan and payment endpoints.
type LayawayHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewLayawayHandler(log *zap.Logger, db *ent.Client) *LayawayHandler {
	return &LayawayHandler{log: log, db: db}
}

type createLayawayInput struct {
	OutletID      string          `json:"outlet_id"`
	CustomerName  string          `json:"customer_name"`
	CustomerPhone string          `json:"customer_phone"`
	CustomerEmail string          `json:"customer_email"`
	TotalAmount   decimal.Decimal `json:"total_amount"`
	DepositAmount decimal.Decimal `json:"deposit_amount"`
	Notes         string          `json:"notes"`
	DueDate       *string         `json:"due_date"`
}

// Create handles POST /{tenantID}/pos/layaways
func (h *LayawayHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createLayawayInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.CustomerName == "" {
		jsonError(w, "customer_name is required", http.StatusBadRequest)
		return
	}
	if input.TotalAmount.IsZero() {
		jsonError(w, "total_amount is required", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	remaining := input.TotalAmount.Sub(input.DepositAmount)

	c := h.db.LayawayPlan.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetCustomerName(input.CustomerName).
		SetTotalAmount(input.TotalAmount).
		SetDepositAmount(input.DepositAmount).
		SetPaidAmount(input.DepositAmount).
		SetRemainingAmount(remaining)

	if input.CustomerPhone != "" {
		c.SetCustomerPhone(input.CustomerPhone)
	}
	if input.CustomerEmail != "" {
		c.SetCustomerEmail(input.CustomerEmail)
	}
	if input.Notes != "" {
		c.SetNotes(input.Notes)
	}

	plan, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create layaway plan failed", zap.Error(err))
		jsonError(w, "failed to create layaway plan: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, plan)
}

// List handles GET /{tenantID}/pos/layaways
// Optional query params: ?status= and ?customer_phone=
func (h *LayawayHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.LayawayPlan.Query().Where(layawayplan.TenantID(tid))

	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(layawayplan.Status(status))
	}
	if phone := r.URL.Query().Get("customer_phone"); phone != "" {
		q = q.Where(layawayplan.CustomerPhone(phone))
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	plans, err := q.Order(ent.Desc(layawayplan.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list layaway plans failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(plans, total, p))
}

// Get handles GET /{tenantID}/pos/layaways/{id}
// Includes all LayawayPayments for this plan.
func (h *LayawayHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	planID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid layaway plan id", http.StatusBadRequest)
		return
	}

	plan, err := h.db.LayawayPlan.Query().
		Where(layawayplan.ID(planID), layawayplan.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "layaway plan not found", http.StatusNotFound)
			return
		}
		h.log.Error("get layaway plan failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	payments, err := h.db.LayawayPayment.Query().
		Where(layawaypayment.LayawayPlanID(planID)).
		Order(ent.Asc(layawaypayment.FieldPaidAt)).
		All(r.Context())
	if err != nil {
		h.log.Error("get layaway payments failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"plan": plan, "payments": payments})
}

type recordLayawayPaymentInput struct {
	Amount        decimal.Decimal `json:"amount"`
	PaymentMethod string          `json:"payment_method"`
	Reference     string          `json:"reference"`
	Notes         string          `json:"notes"`
}

// RecordPayment handles POST /{tenantID}/pos/layaways/{id}/payments
// Creates a payment, updates paid_amount/remaining_amount on the plan, and marks completed if remaining<=0.
func (h *LayawayHandler) RecordPayment(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	planID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid layaway plan id", http.StatusBadRequest)
		return
	}

	var input recordLayawayPaymentInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Amount.IsZero() {
		jsonError(w, "amount is required", http.StatusBadRequest)
		return
	}

	plan, err := h.db.LayawayPlan.Query().
		Where(layawayplan.ID(planID), layawayplan.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "layaway plan not found", http.StatusNotFound)
			return
		}
		h.log.Error("get layaway plan failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if plan.Status == "cancelled" || plan.Status == "completed" {
		jsonError(w, "layaway plan is "+plan.Status, http.StatusBadRequest)
		return
	}

	// Create the payment record
	pc := h.db.LayawayPayment.Create().
		SetLayawayPlanID(planID).
		SetTenantID(tid).
		SetAmount(input.Amount)
	if input.PaymentMethod != "" {
		pc.SetPaymentMethod(input.PaymentMethod)
	}
	if input.Reference != "" {
		pc.SetReference(input.Reference)
	}
	if input.Notes != "" {
		pc.SetNotes(input.Notes)
	}

	payment, err := pc.Save(r.Context())
	if err != nil {
		h.log.Error("create layaway payment failed", zap.Error(err))
		jsonError(w, "failed to record payment: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Update plan balances
	newPaid := plan.PaidAmount.Add(input.Amount)
	newRemaining := plan.RemainingAmount.Sub(input.Amount)

	updater := plan.Update().
		SetPaidAmount(newPaid).
		SetRemainingAmount(newRemaining)

	if newRemaining.LessThanOrEqual(decimal.Zero) {
		updater.SetStatus("completed")
	}

	updatedPlan, err := updater.Save(r.Context())
	if err != nil {
		h.log.Error("update layaway plan balances failed", zap.Error(err))
		jsonError(w, "payment recorded but plan update failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"payment": payment, "plan": updatedPlan})
}

// Cancel handles POST /{tenantID}/pos/layaways/{id}/cancel
func (h *LayawayHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	planID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid layaway plan id", http.StatusBadRequest)
		return
	}

	plan, err := h.db.LayawayPlan.Query().
		Where(layawayplan.ID(planID), layawayplan.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "layaway plan not found", http.StatusNotFound)
			return
		}
		h.log.Error("get layaway plan failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if plan.Status == "cancelled" {
		jsonError(w, "layaway plan is already cancelled", http.StatusBadRequest)
		return
	}

	updated, err := plan.Update().SetStatus("cancelled").Save(r.Context())
	if err != nil {
		h.log.Error("cancel layaway plan failed", zap.Error(err))
		jsonError(w, "failed to cancel plan: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, updated)
}

// Complete handles POST /{tenantID}/pos/layaways/{id}/complete
// Creates a POSOrder from the layaway plan and marks it completed.
// Requires remaining_amount <= 0; idempotent if order_id already set.
func (h *LayawayHandler) Complete(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	planID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid layaway plan id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	plan, err := h.db.LayawayPlan.Query().
		Where(layawayplan.ID(planID), layawayplan.TenantID(tid)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "layaway plan not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if plan.Status == "cancelled" || plan.Status == "forfeited" {
		jsonError(w, "layaway plan is "+plan.Status, http.StatusConflict)
		return
	}

	if plan.RemainingAmount.GreaterThan(decimal.Zero) {
		jsonError(w, "layaway has outstanding balance", http.StatusConflict)
		return
	}

	// Idempotent: if the order was already created, return it.
	if plan.OrderID != nil {
		jsonOK(w, map[string]any{"order_id": plan.OrderID})
		return
	}

	// Get operator from request (optional — layaway completions may be system-triggered).
	userID := uuid.Nil
	if raw := r.Header.Get("X-User-ID"); raw != "" {
		if uid, err := uuid.Parse(raw); err == nil {
			userID = uid
		}
	}

	orderNumber := fmt.Sprintf("LAY-%s-%d", planID.String()[:8], time.Now().UnixMilli())
	totalFloat := plan.TotalAmount.InexactFloat64()

	order, err := h.db.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(plan.OutletID).
		SetDeviceID(uuid.Nil).
		SetUserID(userID).
		SetOrderNumber(orderNumber).
		SetStatus("completed").
		SetSubtotal(totalFloat).
		SetTaxTotal(0).
		SetDiscountTotal(0).
		SetTotalAmount(totalFloat).
		SetCurrency("KES").
		SetOrderSubtype(posorder.OrderSubtypeRetail).
		SetMetadata(map[string]any{"source": "layaway", "layaway_plan_id": planID.String()}).
		SetNillableCustomerPhone(func() *string {
			if plan.CustomerPhone != "" { return &plan.CustomerPhone }
			return nil
		}()).
		SetNillableCustomerName(func() *string {
			if plan.CustomerName != "" { return &plan.CustomerName }
			return nil
		}()).
		Save(ctx)
	if err != nil {
		h.log.Error("create layaway order failed", zap.Error(err))
		jsonError(w, "failed to create order: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Single line summarising the layaway.
	_, err = h.db.POSOrderLine.Create().
		SetOrderID(order.ID).
		SetCatalogItemID(uuid.Nil).
		SetSku("LAYAWAY").
		SetName("Layaway: " + plan.CustomerName).
		SetQuantity(1).
		SetUnitPrice(totalFloat).
		SetTotalPrice(totalFloat).
		SetMetadata(map[string]any{}).
		Save(ctx)
	if err != nil {
		h.log.Error("create layaway order line failed", zap.Error(err))
	}

	// Link order to plan and mark completed.
	_, err = plan.Update().
		SetOrderID(order.ID).
		SetStatus("completed").
		Save(ctx)
	if err != nil {
		h.log.Error("update layaway plan after completion failed", zap.Error(err))
	}

	jsonOK(w, map[string]any{"order_id": order.ID, "order_number": orderNumber})
}
