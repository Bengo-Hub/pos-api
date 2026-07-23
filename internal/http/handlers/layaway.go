package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	"github.com/bengobox/pos-service/internal/modules/staffcredit"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// SaleFinalizer publishes the post-sale fan-out (GL posting, inventory stock backflush, eTIMS
// fiscalisation) for a completed order. Implemented by payments.Service.FinalizeExternalOrder so
// layaway completion syncs identically to a normal sale, without the handler depending on the whole
// payments module surface.
type SaleFinalizer interface {
	FinalizeExternalOrder(order *ent.POSOrder)
}

// LayawayHandler handles layaway plan and payment endpoints.
type LayawayHandler struct {
	log            *zap.Logger
	db             *ent.Client
	staffCredit    *staffcredit.Service
	finalizer      SaleFinalizer
	treasuryClient *treasury.Client
}

func NewLayawayHandler(log *zap.Logger, db *ent.Client) *LayawayHandler {
	return &LayawayHandler{log: log, db: db}
}

// WithTreasuryClient wires the treasury S2S client so Cancel can refund any deposit/installments
// already collected. Optional; nil = the plan is cancelled with no refund attempted (the legacy
// behaviour — money already collected silently stayed uncredited to the customer).
func (h *LayawayHandler) WithTreasuryClient(c *treasury.Client) *LayawayHandler {
	h.treasuryClient = c
	return h
}

// WithStaffCredit wires the staff fund-from-salary provisioner (premium). Optional; nil = staff
// layaways are recorded but never pushed to ERP payroll.
func (h *LayawayHandler) WithStaffCredit(s *staffcredit.Service) *LayawayHandler {
	h.staffCredit = s
	return h
}

// WithFinalizer wires the post-sale fan-out so a completed layaway posts GL/stock/eTIMS. Optional;
// nil = the plan is marked completed and the order created, but nothing is synced downstream (the
// legacy behaviour).
func (h *LayawayHandler) WithFinalizer(f SaleFinalizer) *LayawayHandler {
	h.finalizer = f
	return h
}

// layawayItemInput is one cart line captured on a layaway so completion posts GL/stock/eTIMS with
// the real SKU (not a single opaque LAYAWAY line). Optional — omitting items keeps the total-only
// summary-line behaviour.
type layawayItemInput struct {
	SKU              string  `json:"sku"`
	Name             string  `json:"name"`
	CatalogItemID    string  `json:"catalog_item_id,omitempty"`
	Category         string  `json:"category,omitempty"`
	Quantity         float64 `json:"quantity"`
	UnitPrice        float64 `json:"unit_price"`
	TotalPrice       float64 `json:"total_price"`
	TaxCodeID        string  `json:"tax_code_id,omitempty"`
	TaxKRACode       string  `json:"tax_kra_code,omitempty"`
	TaxRate          float64 `json:"tax_rate,omitempty"`
	TaxAmount        float64 `json:"tax_amount,omitempty"`
	PriceIncludesTax bool    `json:"price_includes_tax,omitempty"`
}

type createLayawayInput struct {
	OutletID      string             `json:"outlet_id"`
	CustomerName  string             `json:"customer_name"`
	CustomerPhone string             `json:"customer_phone"`
	CustomerEmail string             `json:"customer_email"`
	TotalAmount   decimal.Decimal    `json:"total_amount"`
	DepositAmount decimal.Decimal    `json:"deposit_amount"`
	Items         []layawayItemInput `json:"items"`
	Notes         string             `json:"notes"`
	DueDate       *string            `json:"due_date"`
	// Party selection: an existing customer (default) or a staff member funded from salary.
	PartyType         string `json:"party_type"`         // customer | staff
	StaffMemberID     string `json:"staff_member_id"`    // required when party_type=staff
	LoyaltyAccountID  string `json:"loyalty_account_id"` // optional (customer picked from loyalty)
	FundFromSalary    bool   `json:"fund_from_salary"`   // staff: recover via ERP payroll deduction
	InstallmentMonths int    `json:"installment_months"` // staff: number of payroll periods to spread over
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
	// A customer layaway must reference a REAL picked/created customer (loyalty account —
	// which is CRM-synced on creation), never a free-typed walk-in (QA reqs 3 + 7). Staff
	// layaways are keyed on staff_member_id instead.
	if input.PartyType != "staff" && input.LoyaltyAccountID == "" {
		jsonError(w, "select an existing customer or add a new one — walk-in layaways are not allowed", http.StatusBadRequest)
		return
	}

	// Outlet: body wins; fall back to the X-Outlet-ID header (the logged-in user's outlet
	// context pos-ui always sends) so the form can default to the user's own branch.
	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		if hv := r.Header.Get("X-Outlet-ID"); hv != "" {
			outletID, err = uuid.Parse(hv)
		}
		if err != nil {
			jsonError(w, "invalid outlet_id", http.StatusBadRequest)
			return
		}
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
	if len(input.Items) > 0 {
		c.SetItems(layawayItemsToJSON(input.Items))
	}

	// Party: staff (funded from salary) vs customer (default). Staff id + loyalty id are snapshots.
	isStaff := input.PartyType == "staff" && input.StaffMemberID != ""
	var staffMemberID uuid.UUID
	if isStaff {
		if smID, perr := uuid.Parse(input.StaffMemberID); perr == nil {
			staffMemberID = smID
			c.SetPartyType("staff").SetStaffMemberID(smID).SetFundFromSalary(input.FundFromSalary)
		} else {
			isStaff = false
		}
	}
	if input.LoyaltyAccountID != "" {
		if laID, perr := uuid.Parse(input.LoyaltyAccountID); perr == nil {
			c.SetLoyaltyAccountID(laID)
		}
	}

	plan, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create layaway plan failed", zap.Error(err))
		jsonError(w, "failed to create layaway plan: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Staff fund-from-salary: push to ERP as a payroll recoverable (premium; best-effort).
	if isStaff && input.FundFromSalary && h.staffCredit != nil {
		if !h.staffCredit.Entitled(r.Context(), tid) {
			jsonError(w, "staff fund-from-salary requires a higher subscription tier", http.StatusForbidden)
			return
		}
		months := input.InstallmentMonths
		if months < 1 {
			months = 1
		}
		installment := remaining.Div(decimal.NewFromInt(int64(months)))
		planID := plan.ID
		outletCopy := outletID
		if _, perr := h.staffCredit.Provision(r.Context(), tid, staffcredit.ProvisionInput{
			OutletID:          &outletCopy,
			StaffMemberID:     staffMemberID,
			Origin:            "layaway",
			LayawayPlanID:     &planID,
			Principal:         remaining,
			InstallmentAmount: installment,
			InstallmentsTotal: months,
		}); perr != nil {
			h.log.Warn("staff-credit provision failed (layaway saved)", zap.Error(perr))
		}
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, plan)
}

// ListStaffCredit handles GET /{tenantID}/pos/staff-credit — the staff fund-from-salary links
// (admin/reconcile view). Optional ?status=active|settled|cancelled.
func (h *LayawayHandler) ListStaffCredit(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	if h.staffCredit == nil {
		jsonOK(w, pagination.NewResponse([]any{}, 0, pagination.Parse(r)))
		return
	}
	p := pagination.Parse(r)
	rows, total, err := h.staffCredit.List(r.Context(), tid, r.URL.Query().Get("status"), p.Limit, p.Offset)
	if err != nil {
		h.log.Error("list staff-credit links failed", zap.Error(err))
		jsonError(w, "failed to list staff credit", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(rows, total, p))
}

// List handles GET /{tenantID}/pos/layaways
// Optional query params: ?status=, ?customer_phone= and ?outlet_id= (branch filter; "all" = every outlet)
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
	if outlet := r.URL.Query().Get("outlet_id"); outlet != "" && !strings.EqualFold(outlet, "all") {
		if oid, perr := uuid.Parse(outlet); perr == nil {
			q = q.Where(layawayplan.OutletID(oid))
		}
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

	// Cashier's choice of how to give back any deposit/installments already collected — cash/
	// mpesa/bank/cheque hands the money back, store_credit keeps it as credit for a future visit.
	// Defaults to cash. This mirrors the sell-returns refund-channel input; there's no "on
	// account" concept here (a layaway is money already IN, not a debt), so no offset_invoice
	// netting logic is needed the way returns.go needs it.
	var body struct {
		RefundChannel string `json:"refund_channel,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // optional body; absent/empty => cash default
	refundChannel := strings.TrimSpace(body.RefundChannel)
	if refundChannel == "" {
		refundChannel = "cash"
	}

	updated, err := plan.Update().SetStatus("cancelled").Save(r.Context())
	if err != nil {
		h.log.Error("cancel layaway plan failed", zap.Error(err))
		jsonError(w, "failed to cancel plan: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Refund whatever deposit/installments were already collected — a clean cancellation must
	// give that money back (or credit it), never just silently keep it. Previously this endpoint
	// only flipped the status; cash/mpesa/bank already collected from the customer had no refund
	// path at all, so the business ended up keeping a paid-for-but-never-delivered deposit.
	// Best-effort: a failed refund never blocks the cancellation (it can be retried/handled
	// manually), but IS surfaced in the response so the till doesn't silently lose track of it.
	var refundWarning string
	if h.treasuryClient != nil && plan.PaidAmount.IsPositive() {
		tenantSlug := chi.URLParam(r, "tenantID")
		amt, _ := plan.PaidAmount.Float64()
		_, refundErr := h.treasuryClient.CreateRefund(r.Context(), tenantSlug, plan.ID.String(), treasury.RefundRequest{
			SourceService:      "pos",
			ReferenceID:        plan.ID.String(),
			ReferenceType:      "pos_layaway_cancel",
			Reference:          plan.ID.String(),
			Amount:             amt,
			Currency:           "KES",
			Reason:             "layaway_cancelled",
			RefundChannel:      refundChannel,
			CustomerIdentifier: plan.CustomerPhone,
			CustomerName:       plan.CustomerName,
			CustomerEmail:      plan.CustomerEmail,
		})
		if refundErr != nil {
			h.log.Error("layaway cancel: treasury refund call failed (non-fatal; can be retried)",
				zap.Error(refundErr), zap.String("plan_id", plan.ID.String()), zap.Float64("amount", amt))
			refundWarning = "Plan cancelled, but the deposit refund failed and needs to be recorded manually: " + refundErr.Error()
		}
	}

	resp := map[string]any{"plan": updated}
	if refundWarning != "" {
		resp["warning"] = refundWarning
	}
	jsonOK(w, resp)
}

// Forfeit handles POST /{tenantID}/pos/layaways/{id}/forfeit
// Marks an unfinished layaway as forfeited (customer abandoned it). Per policy the deposit/payments
// already made are retained; the goods are released back to stock manually. Distinct from cancel
// (a clean cancellation/refund) — forfeited is the "lapsed plan" terminal state.
func (h *LayawayHandler) Forfeit(w http.ResponseWriter, r *http.Request) {
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
	if plan.Status == "completed" || plan.Status == "cancelled" || plan.Status == "forfeited" {
		jsonError(w, "layaway plan is "+plan.Status, http.StatusBadRequest)
		return
	}
	updated, err := plan.Update().SetStatus("forfeited").Save(r.Context())
	if err != nil {
		h.log.Error("forfeit layaway plan failed", zap.Error(err))
		jsonError(w, "failed to forfeit plan: "+err.Error(), http.StatusInternalServerError)
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
			if plan.CustomerPhone != "" {
				return &plan.CustomerPhone
			}
			return nil
		}()).
		SetNillableCustomerName(func() *string {
			if plan.CustomerName != "" {
				return &plan.CustomerName
			}
			return nil
		}()).
		Save(ctx)
	if err != nil {
		h.log.Error("create layaway order failed", zap.Error(err))
		jsonError(w, "failed to create order: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Rebuild the real order lines from the captured cart snapshot so the sale posts GL, backflushes
	// stock, and fiscalises eTIMS with real SKUs. Legacy/total-only plans (no items) keep the single
	// opaque LAYAWAY summary line — GL still posts, but there is nothing to backflush/fiscalise.
	if len(plan.Items) > 0 {
		for _, raw := range plan.Items {
			it := layawayItemFromJSON(raw)
			if it.SKU == "" || it.Quantity <= 0 {
				continue
			}
			lc := h.db.POSOrderLine.Create().
				SetOrderID(order.ID).
				SetCatalogItemID(parseUUIDOrNil(it.CatalogItemID)).
				SetSku(it.SKU).
				SetName(it.Name).
				SetQuantity(it.Quantity).
				SetUnitPrice(it.UnitPrice).
				SetTotalPrice(it.TotalPrice).
				SetPriceIncludesTax(it.PriceIncludesTax).
				SetMetadata(map[string]any{})
			if it.Category != "" {
				lc.SetCategory(it.Category)
			}
			if it.TaxCodeID != "" {
				lc.SetTaxCodeID(it.TaxCodeID)
			}
			if it.TaxKRACode != "" {
				lc.SetTaxKraCode(it.TaxKRACode)
			}
			if it.TaxRate != 0 {
				lc.SetTaxRate(it.TaxRate)
			}
			if it.TaxAmount != 0 {
				lc.SetTaxAmount(it.TaxAmount)
			}
			if _, lerr := lc.Save(ctx); lerr != nil {
				h.log.Error("create layaway order line failed", zap.String("sku", it.SKU), zap.Error(lerr))
			}
		}
	} else {
		// Single line summarising the layaway (legacy total-only plan).
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
	}

	// Link order to plan and mark completed.
	_, err = plan.Update().
		SetOrderID(order.ID).
		SetStatus("completed").
		Save(ctx)
	if err != nil {
		h.log.Error("update layaway plan after completion failed", zap.Error(err))
	}

	// Sync the completed sale downstream (GL posting, inventory backflush, eTIMS) exactly like a
	// normal sale. Best-effort/detached — a completed layaway is never blocked on the fan-out.
	if h.finalizer != nil {
		h.finalizer.FinalizeExternalOrder(order)
	}

	jsonOK(w, map[string]any{"order_id": order.ID, "order_number": orderNumber})
}

// layawayItemsToJSON converts the typed create input into the JSON slice stored on the plan.
func layawayItemsToJSON(items []layawayItemInput) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"sku": it.SKU, "name": it.Name, "catalog_item_id": it.CatalogItemID,
			"category": it.Category, "quantity": it.Quantity, "unit_price": it.UnitPrice,
			"total_price": it.TotalPrice, "tax_code_id": it.TaxCodeID, "tax_kra_code": it.TaxKRACode,
			"tax_rate": it.TaxRate, "tax_amount": it.TaxAmount, "price_includes_tax": it.PriceIncludesTax,
		})
	}
	return out
}

// layawayItemFromJSON reads one stored line snapshot back into the typed struct (numbers arrive as
// float64 from JSON unmarshalling).
func layawayItemFromJSON(m map[string]any) layawayItemInput {
	str := func(k string) string { s, _ := m[k].(string); return s }
	num := func(k string) float64 { f, _ := m[k].(float64); return f }
	b, _ := m["price_includes_tax"].(bool)
	return layawayItemInput{
		SKU: str("sku"), Name: str("name"), CatalogItemID: str("catalog_item_id"),
		Category: str("category"), Quantity: num("quantity"), UnitPrice: num("unit_price"),
		TotalPrice: num("total_price"), TaxCodeID: str("tax_code_id"), TaxKRACode: str("tax_kra_code"),
		TaxRate: num("tax_rate"), TaxAmount: num("tax_amount"), PriceIncludesTax: b,
	}
}

// parseUUIDOrNil parses a UUID string, returning uuid.Nil on empty/invalid input.
func parseUUIDOrNil(s string) uuid.UUID {
	if s == "" {
		return uuid.Nil
	}
	if id, err := uuid.Parse(s); err == nil {
		return id
	}
	return uuid.Nil
}
