// Package payments provides the payment service layer for POS operations.
package payments

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcommissionrule "github.com/bengobox/pos-service/internal/ent/commissionrule"
	entloyalty "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	outletsettingpredicate "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entcatalogoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	staffmemberent "github.com/bengobox/pos-service/internal/ent/staffmember"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tableassignment"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/staffcredit"
	"github.com/bengobox/pos-service/internal/modules/treasury"
	"github.com/bengobox/pos-service/internal/payref"
	"github.com/bengobox/pos-service/internal/platform/events"
	"github.com/bengobox/pos-service/internal/platform/marketflow"
)

// Shared "Walk-in" customer used to attribute transactions that need a customer but have none
// (e.g. an on-account sale rung up without selecting a customer). marketflow CRM is the source of
// truth; the contact is upserted there and treasury keys the AR balance on this identifier.
const (
	walkInPhone = "+000000000000"
	walkInName  = "Walk-in Customer"
)

// PaymentStatus defines valid payment states.
const (
	StatusPending   = "pending"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusRefunded  = "refunded"
)

// isCashMethod returns true for tender types that settle immediately without a treasury gateway
// round-trip: cash, manual M-Pesa code entry, room charge, and external card-terminal/PDQ swipes
// (the standalone machine has already approved the card, so there is no online gateway step).
func isCashMethod(method string) bool {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "cash", "manual", "room_charge", "card_manual", "pdq", "card_terminal":
		return true
	default:
		return false
	}
}

// treasuryMethodForImmediate maps an immediate-settle POS tender onto the payment_method treasury
// records. Cash-equivalents (cash/manual M-Pesa code/room charge) are recorded as "cash"; external
// card-terminal swipes are recorded as "card_manual" so treasury tags the provider correctly.
func treasuryMethodForImmediate(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "card_manual", "pdq", "card_terminal":
		return "card_manual"
	default:
		return "cash"
	}
}

// TenderOnAccount is the credit-sale ("sell on account") tender: no money is taken at the till;
// the amount is posted to the customer's AR balance in treasury instead.
const TenderOnAccount = "on_account"

// RecordPaymentRequest holds input for recording a POS payment.
type RecordPaymentRequest struct {
	TenantID      uuid.UUID
	TenantSlug    string
	OrderID       uuid.UUID
	TenderID      uuid.UUID
	TenderMethod  string // cash | card | mpesa | manual | room_charge | etc.
	Amount        float64
	Currency      string
	Reference     string // external reference (optional; set by treasury callback for digital)
	ExternalRef   string // cashier-entered ref for manual/paybill payments (stored on local payment)
	IntentID      string // treasury payment_intent_id (set for digital payments)
	PublicBaseURL string // used to construct initiateUrl for digital payments
}

// CreateIntentResult is returned to the caller when a digital payment intent is created.
type CreateIntentResult struct {
	PaymentIntentID string
	InitiateURL     string // proxy URL that treasury-ui calls when user selects gateway
	IsCash          bool   // true → payment is already settled, no further modal needed
}

// Service provides payment business logic.
type Service struct {
	client          *ent.Client
	orderSvc        *orders.Service
	treasuryClient  *treasury.Client
	inventoryClient *inventory.Client
	publisher       *events.Publisher
	marketflow      *marketflow.Client
	staffCredit     *staffcredit.Service
	log             *zap.Logger
	defaultCurrency string
}

// NewService creates a new payment service.
func NewService(client *ent.Client, orderSvc *orders.Service, defaultCurrency string, log *zap.Logger) *Service {
	if defaultCurrency == "" {
		defaultCurrency = "KES"
	}
	return &Service{
		client:          client,
		orderSvc:        orderSvc,
		log:             log.Named("payments.service"),
		defaultCurrency: defaultCurrency,
	}
}

// SetTreasuryClient injects the treasury S2S client after construction (avoids circular init).
func (s *Service) SetTreasuryClient(c *treasury.Client) {
	s.treasuryClient = c
}

// SetInventoryClient injects the inventory S2S client for consumption backflush.
func (s *Service) SetInventoryClient(c *inventory.Client) {
	s.inventoryClient = c
}

// SetPublisher injects the event publisher for pos.sale.finalized and related events.
func (s *Service) SetPublisher(p *events.Publisher) {
	s.publisher = p
}

// SetStaffCredit injects the staff fund-from-salary provisioner so a staff credit sale routes the
// debt to ERP payroll instead of customer AR. Optional; nil = staff credit sales fall back to AR.
func (s *Service) SetStaffCredit(sc *staffcredit.Service) {
	s.staffCredit = sc
}

// SetMarketFlowClient injects the marketflow CRM client, used to resolve the shared "Walk-in"
// customer when an on-account sale has no customer attached.
func (s *Service) SetMarketFlowClient(c *marketflow.Client) {
	s.marketflow = c
}

// CreatePaymentIntent creates a treasury payment intent and returns the intent ID + initiateUrl.
//
// Cash/manual/room_charge: creates a "cash" intent → treasury settles it immediately → records
// the local payment as completed and returns IsCash=true.
//
// Digital (card, mpesa, etc.): creates a "pending" intent → returns intent ID + initiateUrl for
// the pos-ui to open TreasuryPaymentModal. The local payment is recorded as pending and will be
// completed by the treasury.payment.success NATS subscriber.
func (s *Service) CreatePaymentIntent(ctx context.Context, req RecordPaymentRequest) (*CreateIntentResult, error) {
	if s.treasuryClient == nil {
		return nil, fmt.Errorf("payments: treasury client not configured")
	}

	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(req.OrderID), posorder.TenantID(req.TenantID)).
		WithPayments().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: order not found: %w", err)
	}

	if order.Status == orders.StatusCancelled || order.Status == orders.StatusVoided {
		return nil, fmt.Errorf("payments: cannot pay %s order", order.Status)
	}

	// Never trust a non-positive client amount: settling a bill from a list
	// (e.g. "Settle Bill" in My Bills) can pass amount=0 when the in-memory total
	// is stale. Derive the charge from the order's outstanding balance so we never
	// send a zero-amount intent to treasury (which 500s on it).
	if req.Amount <= 0 {
		req.Amount = s.outstandingBalance(ctx, order)
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("payments: order has no outstanding balance to charge")
	}

	currency := req.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}

	// Credit sale (sell on account): no payment is taken now. Post the amount to the customer's AR
	// balance in treasury (which enforces the credit limit), then settle the order on account.
	if strings.EqualFold(req.TenderMethod, TenderOnAccount) {
		return s.recordCreditSale(ctx, order, req, currency)
	}

	cash := isCashMethod(req.TenderMethod)

	paymentMethod := "pending"
	if cash {
		paymentMethod = treasuryMethodForImmediate(req.TenderMethod)
	}

	intentReq := treasury.CreateIntentRequest{
		SourceService: "pos",
		ReferenceID:   payref.Build("POS", req.TenantSlug, req.TenantID, req.OrderID),
		ReferenceType: "pos_order",
		Amount:        req.Amount,
		Currency:      currency,
		PaymentMethod: paymentMethod,
		Description:   fmt.Sprintf("POS order %s", order.OrderNumber),
		OutletID:      order.OutletID.String(),
		// entity_id lets consumers recover the order UUID now that reference_id is a prefixed,
		// service-identifiable string rather than the bare order UUID.
		Metadata: map[string]any{"service": "pos", "entity_id": req.OrderID.String()},
	}
	// Carry the cashier-entered external reference (card terminal approval code / M-Pesa code) so
	// treasury records it on the immediate-settle PaymentTransaction instead of a synthetic ref.
	if cash && req.ExternalRef != "" {
		intentReq.Metadata["external_ref"] = req.ExternalRef
	}

	intent, err := s.treasuryClient.CreateIntent(ctx, req.TenantSlug, req.OrderID.String(), intentReq)
	if err != nil {
		return nil, fmt.Errorf("payments: create treasury intent: %w", err)
	}

	result := &CreateIntentResult{
		PaymentIntentID: intent.ResolvedID(),
		IsCash:          cash,
	}

	if !cash {
		// Prefer treasury's returned initiate_url (public, no auth required).
		// Fall back to the pos-api proxy only when treasury doesn't return one.
		if intent.InitiateURL != "" {
			result.InitiateURL = intent.InitiateURL
		} else {
			result.InitiateURL = fmt.Sprintf("%s/api/v1/%s/pos/payments/initiate", req.PublicBaseURL, req.TenantSlug)
		}

		// Record local payment as pending — will be completed by treasury NATS subscriber
		_, err = s.client.POSPayment.Create().
			SetOrderID(req.OrderID).
			SetTenderID(req.TenderID).
			SetAmount(req.Amount).
			SetCurrency(currency).
			SetStatus(StatusPending).
			SetPaymentData(map[string]any{"method": req.TenderMethod}).
			SetNillableExternalReference(nilIfEmpty(intent.ResolvedID())).
			Save(ctx)
		if err != nil {
			// Must NOT swallow this: without the local pending POSPayment row, treasury's
			// payment.succeeded callback can't match the intent back to an order (it looks the order up
			// by this row's external_reference), leaving the order stuck unpaid. Fail loudly so the
			// caller can retry — each retry creates a fresh intent, and the unconfirmed one expires.
			return nil, fmt.Errorf("payments: record pending payment: %w", err)
		}
		return result, nil
	}

	// Cash / manual: record as completed immediately.
	// For manual payments use the cashier-entered ref; otherwise store the treasury intent ID.
	cashRef := intent.ResolvedID()
	if req.ExternalRef != "" {
		cashRef = req.ExternalRef
	}
	_, err = s.client.POSPayment.Create().
		SetOrderID(req.OrderID).
		SetTenderID(req.TenderID).
		SetAmount(req.Amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{"method": req.TenderMethod}).
		SetNillableExternalReference(nilIfEmpty(cashRef)).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: record cash payment: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order)
	return result, nil
}

// recordCreditSale settles an order "on account" (credit sale). It posts the amount to the
// customer's AR balance in treasury FIRST — treasury enforces the customer credit limit and
// rejects over-limit sales — and only on success records a completed on-account payment and
// completes the order (so goods never leave without the debt being recorded). Requires a customer.
func (s *Service) recordCreditSale(ctx context.Context, order *ent.POSOrder, req RecordPaymentRequest, currency string) (*CreateIntentResult, error) {
	// Staff fund-from-salary: when the order metadata flags a staff-funded credit sale, route the
	// debt to ERP payroll (a StaffPurchaseDeduction) instead of customer AR. Still records the
	// on-account payment + completes the order so goods never leave without the debt recorded.
	if staffID, months, ok := staffCreditFromOrder(order); ok && s.staffCredit != nil && s.staffCredit.Entitled(ctx, req.TenantID) {
		if months < 1 {
			months = 1
		}
		principal := decimal.NewFromFloat(req.Amount)
		install := principal.Div(decimal.NewFromInt(int64(months)))
		orderID := order.ID
		outletID := order.OutletID
		if _, perr := s.staffCredit.Provision(ctx, req.TenantID, staffcredit.ProvisionInput{
			OutletID:          &outletID,
			StaffMemberID:     staffID,
			Origin:            "credit_sale",
			POSOrderID:        &orderID,
			Principal:         principal,
			InstallmentAmount: install,
			InstallmentsTotal: months,
		}); perr != nil {
			s.log.Warn("staff-credit provision failed (credit sale)", zap.Error(perr))
		} else {
			if _, err := s.client.POSPayment.Create().
				SetOrderID(req.OrderID).SetTenderID(req.TenderID).SetAmount(req.Amount).
				SetCurrency(currency).SetStatus(StatusCompleted).
				SetPaymentData(map[string]any{"method": TenderOnAccount, "fund_from_salary": true}).
				SetNillableExternalReference(nilIfEmpty(order.OrderNumber)).
				Save(ctx); err != nil {
				return nil, fmt.Errorf("payments: record staff on-account payment: %w", err)
			}
			s.completeOrderIfFullyPaid(ctx, order)
			return &CreateIntentResult{IsCash: true}, nil
		}
	}

	if s.treasuryClient == nil {
		return nil, fmt.Errorf("payments: treasury client not configured")
	}
	phone := ""
	if order.CustomerPhone != nil {
		phone = *order.CustomerPhone
	}
	name := ""
	if order.CustomerName != nil {
		name = *order.CustomerName
	}
	// A credit sale MUST be booked against a real customer account — never the shared "Walk-in" ghost
	// (it commingles unrelated debts onto one row that can never be collected or reconciled). Reject
	// when no customer was selected; the pos-ui also hides Credit Sale until a customer is chosen.
	if strings.TrimSpace(phone) == "" && strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("payments: credit sale requires a selected customer")
	}
	// A STAFF credit sale that falls through to AR (fund-from-salary off or not entitled) has no
	// customer phone — key the treasury debtor on the staff member id so each staff member gets a
	// distinct, reconcilable AR row instead of an empty identifier.
	if staffID, _, isStaff := staffCreditFromOrderParty(order); isStaff && strings.TrimSpace(phone) == "" {
		phone = "staff:" + staffID.String()
	}
	// Resolve the canonical AR key — the selected customer's marketflow CRM contact (the SAME source
	// the return path uses), via the loyalty account for this phone. Sending both the CRM id and the
	// phone lets treasury net the credit sale, its returns and its opening balance on ONE customer row.
	crmContactID := s.ResolveCrmContactID(ctx, req.TenantID, phone)

	if _, err := s.treasuryClient.RecordCreditSale(ctx, req.TenantSlug, treasury.CreditSaleRequest{
		CrmContactID:       crmContactID,
		CustomerIdentifier: phone,
		CustomerName:       name,
		POSOrderID:         order.ID.String(),
		Reference:          order.OrderNumber,
		Amount:             req.Amount,
		Currency:           currency,
		UserID:             order.UserID.String(),
	}); err != nil {
		return nil, fmt.Errorf("payments: credit sale rejected: %w", err)
	}

	if _, err := s.client.POSPayment.Create().
		SetOrderID(req.OrderID).
		SetTenderID(req.TenderID).
		SetAmount(req.Amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{"method": TenderOnAccount}).
		SetNillableExternalReference(nilIfEmpty(order.OrderNumber)).
		Save(ctx); err != nil {
		return nil, fmt.Errorf("payments: record on-account payment: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order)
	return &CreateIntentResult{IsCash: true}, nil
}

// ResolveCrmContactID returns the marketflow CRM contact id for a customer phone (via the loyalty
// account), or "" when none is linked. This is the canonical treasury AR key — the same resolution
// the return path uses — so a credit sale, its returns and its opening balance all land on one row.
func (s *Service) ResolveCrmContactID(ctx context.Context, tenantID uuid.UUID, phone string) string {
	if strings.TrimSpace(phone) == "" {
		return ""
	}
	acc, err := s.client.LoyaltyAccount.Query().
		Where(entloyalty.TenantID(tenantID), entloyalty.CustomerPhone(phone)).
		First(ctx)
	if err != nil || acc == nil || acc.CrmContactID == nil {
		return ""
	}
	return acc.CrmContactID.String()
}

// staffCreditFromOrderParty extracts the staff BILL-TO party from an order's metadata
// (party_type=staff + staff_member_id), regardless of the fund-from-salary flag. Used to key the
// treasury AR debtor when a staff credit sale falls through to AR.
func staffCreditFromOrderParty(order *ent.POSOrder) (staffID uuid.UUID, fund bool, ok bool) {
	if order == nil || order.Metadata == nil {
		return uuid.Nil, false, false
	}
	sid, _ := order.Metadata["staff_member_id"].(string)
	if sid == "" {
		return uuid.Nil, false, false
	}
	id, err := uuid.Parse(sid)
	if err != nil {
		return uuid.Nil, false, false
	}
	fund, _ = order.Metadata["fund_from_salary"].(bool)
	return id, fund, true
}

// staffCreditFromOrder extracts the staff fund-from-salary intent from an order's metadata
// (fund_from_salary + staff_member_id [+ installment_months]), set by the credit-sale UI.
func staffCreditFromOrder(order *ent.POSOrder) (staffID uuid.UUID, months int, ok bool) {
	id, fund, ok := staffCreditFromOrderParty(order)
	if !ok || !fund {
		return uuid.Nil, 0, false
	}
	switch v := order.Metadata["installment_months"].(type) {
	case float64:
		months = int(v)
	case int:
		months = v
	}
	return id, months, true
}

// ConfirmPaymentByIntentID is called by the treasury.payment.succeeded NATS subscriber.
// It finds the pending local payment for the given treasury intent ID and marks it completed,
// then completes the order if fully paid. settledAmount is the amount treasury ACTUALLY
// captured (0 = unknown): when it differs from the pending row's opening amount, the row is
// corrected so paid_total only ever counts money that was really collected — an intent
// opened for the full total but partially captured must not flip the order to paid.
func (s *Service) ConfirmPaymentByIntentID(ctx context.Context, tenantID uuid.UUID, intentID string, settledAmount float64) error {
	payments, err := s.client.POSPayment.Query().
		Where(pospayment.ExternalReference(intentID)).
		WithOrder().
		All(ctx)
	if err != nil || len(payments) == 0 {
		return fmt.Errorf("payments: no pending payment found for intent %s", intentID)
	}

	for _, p := range payments {
		if p.Status == StatusCompleted {
			continue
		}
		upd := s.client.POSPayment.UpdateOne(p).SetStatus(StatusCompleted)
		if settledAmount > 0 && settledAmount != p.Amount {
			s.log.Warn("treasury settled amount differs from pending payment — correcting local row",
				zap.String("payment_id", p.ID.String()),
				zap.Float64("pending_amount", p.Amount),
				zap.Float64("settled_amount", settledAmount))
			upd = upd.SetAmount(settledAmount)
		}
		// Mark the row gateway-settled so it can never be edited/voided at the till —
		// reversing gateway money must go through the refund flow.
		data := p.PaymentData
		if data == nil {
			data = map[string]any{}
		}
		data["settled_via"] = "treasury_gateway"
		upd = upd.SetPaymentData(data)
		if _, err := upd.Save(ctx); err != nil {
			return fmt.Errorf("payments: confirm payment %s: %w", p.ID, err)
		}
		if p.Edges.Order != nil {
			s.completeOrderIfFullyPaid(ctx, p.Edges.Order)
		}
	}
	return nil
}

// FailPaymentByIntentID marks a pending payment as failed (called by treasury.payment.failed subscriber).
func (s *Service) FailPaymentByIntentID(ctx context.Context, intentID string) error {
	payments, err := s.client.POSPayment.Query().
		Where(pospayment.ExternalReference(intentID)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("payments: query by intent: %w", err)
	}
	for _, p := range payments {
		if p.Status != StatusPending {
			continue
		}
		if _, err := s.client.POSPayment.UpdateOne(p).SetStatus(StatusFailed).Save(ctx); err != nil {
			return fmt.Errorf("payments: fail payment %s: %w", p.ID, err)
		}
	}
	return nil
}

// RecordPayment is the legacy direct-record path (cash tender with no treasury intent).
// Kept for internal use and backward compatibility with existing POS terminal flows.
func (s *Service) RecordPayment(ctx context.Context, req RecordPaymentRequest) (*ent.POSPayment, error) {
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(req.OrderID), posorder.TenantID(req.TenantID)).
		WithPayments().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: order not found: %w", err)
	}

	if order.Status == orders.StatusCancelled || order.Status == orders.StatusVoided {
		return nil, fmt.Errorf("payments: cannot pay %s order", order.Status)
	}

	// Fall back to the order's outstanding balance when no positive amount is given.
	if req.Amount <= 0 {
		req.Amount = s.outstandingBalance(ctx, order)
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("payments: order has no outstanding balance to charge")
	}

	currency := req.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}

	payment, err := s.client.POSPayment.Create().
		SetOrderID(req.OrderID).
		SetTenderID(req.TenderID).
		SetAmount(req.Amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetNillableExternalReference(nilIfEmpty(req.Reference)).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: record failed: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order)
	return payment, nil
}

// ListOrderPayments returns all payments for an order.
func (s *Service) ListOrderPayments(ctx context.Context, tenantID, orderID uuid.UUID) ([]*ent.POSPayment, error) {
	exists, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		Exist(ctx)
	if err != nil || !exists {
		return nil, fmt.Errorf("payments: order not found")
	}

	return s.client.POSPayment.Query().
		Where(pospayment.OrderID(orderID)).
		All(ctx)
}

// outstandingBalance returns the order total minus all completed payments (>= 0).
func (s *Service) outstandingBalance(ctx context.Context, order *ent.POSOrder) float64 {
	payments, err := s.client.POSPayment.Query().
		Where(pospayment.OrderID(order.ID), pospayment.Status(StatusCompleted)).
		All(ctx)
	if err != nil {
		return order.TotalAmount
	}
	var paid float64
	for _, p := range payments {
		paid += p.Amount
	}
	if remaining := order.TotalAmount - paid; remaining > 0 {
		return remaining
	}
	return 0
}

// RecomputePaidTotal recalculates an order's paid_total from its COMPLETED payments and
// persists it on the order. It is the single write path for paid_total — called after every
// payment mutation (record, confirm, void/edit) — so the payment-status filter, the row
// badge, and completeOrderIfFullyPaid all read one consistent value. Returns the new sum.
func (s *Service) RecomputePaidTotal(ctx context.Context, orderID uuid.UUID) (float64, error) {
	var agg []struct {
		Sum float64 `json:"sum"`
	}
	err := s.client.POSPayment.Query().
		Where(pospayment.OrderID(orderID), pospayment.Status(StatusCompleted)).
		Aggregate(ent.Sum(pospayment.FieldAmount)).
		Scan(ctx, &agg)
	if err != nil {
		return 0, fmt.Errorf("payments: sum completed payments: %w", err)
	}
	paid := 0.0
	if len(agg) > 0 {
		paid = agg[0].Sum
	}
	if err := s.client.POSOrder.UpdateOneID(orderID).SetPaidTotal(paid).Exec(ctx); err != nil {
		return paid, fmt.Errorf("payments: store paid_total: %w", err)
	}
	return paid, nil
}

// completeOrderIfFullyPaid recomputes the order's paid_total and marks the order completed
// when its completed payments cover the total.
//
// NOTE: this used to take an additionalAmount that was ADDED ON TOP of the queried sum —
// but every caller had already persisted the payment row before calling, so the amount was
// double-counted and any partial payment >= half the total wrongly completed the order
// (root cause of "PAID badge with a positive Sell Due" / broken Partial filtering).
func (s *Service) completeOrderIfFullyPaid(ctx context.Context, order *ent.POSOrder) {
	totalPaid, err := s.RecomputePaidTotal(ctx, order.ID)
	if err != nil {
		s.log.Warn("recompute paid_total failed", zap.String("order_id", order.ID.String()), zap.Error(err))
		return
	}

	if totalPaid+0.01 >= order.TotalAmount {
		if err := s.orderSvc.ValidateStatusTransition(order.Status, orders.StatusCompleted); err == nil {
			updated, updateErr := s.client.POSOrder.UpdateOne(order).
				SetStatus(orders.StatusCompleted).
				Save(ctx)
			if updateErr != nil {
				s.log.Warn("failed to complete order after full payment",
					zap.String("order_id", order.ID.String()),
					zap.Error(updateErr))
				return
			}
			s.publishSaleFinalized(ctx, updated)
			s.calcCommissions(ctx, updated)
			// Free the table once the bill is settled, regardless of which flow
			// (waiter My Bills, cashier orders page, or async digital confirmation)
			// closed it — mirrors the manual ReleaseTable endpoint.
			s.releaseTableForOrder(ctx, order.ID)
		}
	}
}

// releaseTableForOrder frees any table occupied by this order: it closes the
// active table assignment and sets the table back to available. No-op when the
// order isn't tied to a table (takeaway, delivery, retail).
func (s *Service) releaseTableForOrder(ctx context.Context, orderID uuid.UUID) {
	asgns, err := s.client.TableAssignment.Query().
		Where(tableassignment.OrderID(orderID), tableassignment.ReleasedAtIsNil()).
		All(ctx)
	if err != nil || len(asgns) == 0 {
		return
	}
	now := time.Now()
	for _, a := range asgns {
		if _, uerr := s.client.TableAssignment.UpdateOne(a).SetReleasedAt(now).Save(ctx); uerr != nil {
			s.log.Warn("release table: close assignment failed", zap.Error(uerr))
		}
		if _, uerr := s.client.Table.Update().
			Where(enttable.ID(a.TableID)).
			SetStatus("available").
			Save(ctx); uerr != nil {
			s.log.Warn("release table: set available failed", zap.Error(uerr))
		}
	}
}

// publishSaleFinalized emits pos.sale.finalized to the NATS outbox.
// treasury-api consumes this for ledger posting; inventory-api consumes it for stock backflush.
func (s *Service) publishSaleFinalized(ctx context.Context, order *ent.POSOrder) {
	if s.publisher == nil {
		return
	}

	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(order.ID)).
		WithModifiers().
		All(ctx)
	if err != nil {
		s.log.Warn("sale.finalized: failed to load lines", zap.String("order_id", order.ID.String()), zap.Error(err))
	}

	// Resolve modifier -> inventory modifier-option id once, so the backflush payload
	// can deduct modifier ingredient stock (previously dropped — modifier ingredients leaked).
	modifierOptionByID := map[uuid.UUID]*uuid.UUID{}
	for _, l := range lines {
		for _, m := range l.Edges.Modifiers {
			if _, seen := modifierOptionByID[m.ModifierID]; seen {
				continue
			}
			if mod, mErr := s.client.Modifier.Get(ctx, m.ModifierID); mErr == nil {
				modifierOptionByID[m.ModifierID] = mod.InventoryModifierOptionID
			} else {
				modifierOptionByID[m.ModifierID] = nil
			}
		}
	}

	// Resolve default warehouse from outlet setting for inventory backflush routing.
	warehouseID := ""
	outletSlug := ""
	if outlet, oErr := s.client.Outlet.Get(ctx, order.OutletID); oErr == nil {
		outletSlug = outlet.TenantSlug
		if setting, sErr := s.client.OutletSetting.Query().
			Where(outletsettingpredicate.OutletID(order.OutletID)).
			Only(ctx); sErr == nil && setting.DefaultWarehouseID != nil {
			warehouseID = setting.DefaultWarehouseID.String()
		}
	}

	// Resolve per-unit item COST so treasury can post per-sale Cost of Goods Sold
	// (DR 5400 / CR 1500). pos-api stores no cost on the order line; the authoritative
	// per-unit cost is the inventory-synced value cached on POSCatalogOverride.metadata["cost_price"]
	// (keyed by tenant + inventory_sku, see catalog/inventory_events.go and reports_profitability.go).
	// Resolved here as a SKU->cost map in one query. Missing cost => 0 (never blocks the sale).
	costBySKU, uomBySKU := s.resolveLineCosts(ctx, order.TenantID, lines)

	items := make([]map[string]any, 0, len(lines))
	costTotal := 0.0
	for _, l := range lines {
		costAmount := costBySKU[l.Sku] // per-unit cost; 0 when not available
		lineCost := costAmount * l.Quantity
		costTotal += lineCost
		item := map[string]any{
			"sku":         l.Sku,
			"name":        l.Name,
			"quantity":    l.Quantity,
			"unit_price":  l.UnitPrice,
			"total_price": l.TotalPrice,
			// The item's inventory stock unit (synced via catalog events); "" when unknown.
			// inventory-api converts non-stock-unit quantities (incl. the bottle
			// content-per-unit bridge) before deducting.
			"uom_code":           uomBySKU[l.Sku],
			"price_includes_tax": l.PriceIncludesTax,
			// COGS support (additive): per-unit cost and line cost (cost x qty). 0 when unknown.
			"cost_amount": costAmount,
			"line_cost":   lineCost,
		}
		// Include pre-computed tax breakdown if available — treasury uses this verbatim for eTIMS.
		if l.TaxCodeID != "" {
			item["tax_code_id"] = l.TaxCodeID
		}
		if l.TaxKraCode != "" {
			item["tax_kra_code"] = l.TaxKraCode
		}
		if l.TaxRate != nil {
			item["tax_rate"] = *l.TaxRate
		}
		if l.TaxAmount != nil {
			item["tax_amount"] = *l.TaxAmount
		}
		// Attach captured serial(s) so inventory flips the specific sold unit(s) to "sold" in its
		// per-unit serial registry (serial-tracked items: electronics, equipment).
		if l.SerialNumber != nil && *l.SerialNumber != "" {
			item["serials"] = []string{*l.SerialNumber}
		}
		// Attach modifiers so inventory can backflush their ingredient consumption.
		if len(l.Edges.Modifiers) > 0 {
			mods := make([]map[string]any, 0, len(l.Edges.Modifiers))
			for _, m := range l.Edges.Modifiers {
				mod := map[string]any{
					"modifier_id": m.ModifierID.String(),
					"name":        m.Name,
					"quantity":    l.Quantity,
				}
				if optID := modifierOptionByID[m.ModifierID]; optID != nil {
					mod["inventory_modifier_option_id"] = optID.String()
				}
				mods = append(mods, mod)
			}
			item["modifiers"] = mods
		}
		items = append(items, item)
	}

	// Determine the SELLING SCHEME from the order's tenders so treasury routes the ledger correctly.
	// A credit ("on account") sale has ALREADY posted the amount to the customer's AR balance in
	// treasury (recordCreditSale); treasury's sale.finalized subscriber must therefore NOT also post
	// a cash receipt for it (that double-counted credit sales as cash). We surface the scheme, the
	// on-account amount, and a per-tender breakdown for accurate GL posting.
	sellingScheme := "cash"
	onAccountAmount := 0.0
	tenderBreakdown := make([]map[string]any, 0)
	if pays, perr := s.client.POSPayment.Query().Where(pospayment.OrderID(order.ID)).All(ctx); perr == nil {
		for _, p := range pays {
			tType := ""
			if t, tErr := s.client.Tender.Get(ctx, p.TenderID); tErr == nil {
				tType = t.Type
			}
			tenderBreakdown = append(tenderBreakdown, map[string]any{
				"type":   tType,
				"amount": p.Amount,
				"status": p.Status,
			})
			if strings.EqualFold(tType, TenderOnAccount) && p.Status == StatusCompleted {
				onAccountAmount += p.Amount
			}
		}
	}
	if onAccountAmount > 0 {
		if onAccountAmount >= order.TotalAmount-0.01 {
			sellingScheme = "credit" // fully on account
		} else {
			sellingScheme = "mixed" // part cash / part on account
		}
	}

	data := map[string]any{
		"order_id":     order.ID.String(),
		"order_number": order.OrderNumber,
		"tenant_id":    order.TenantID.String(),
		"tenant_slug":  outletSlug,
		"outlet_id":    order.OutletID.String(),
		"warehouse_id": warehouseID,
		"total_amount": order.TotalAmount,
		"currency":     order.Currency,
		"items":        items,
		// Sum of per-line cost (cost_amount x quantity) for the whole sale; used by treasury
		// to post Cost of Goods Sold (DR 5400 / CR 1500). 0 when no item carries a known cost.
		"cost_total": costTotal,
		// Selling scheme + tender breakdown for treasury GL routing (cash receipt vs AR).
		"selling_scheme":    sellingScheme,
		"on_account_amount": onAccountAmount,
		"tenders":           tenderBreakdown,
		"customer_phone": func() string {
			if order.CustomerPhone != nil {
				return *order.CustomerPhone
			}
			return ""
		}(),
	}

	if err := s.publisher.PublishSaleFinalized(ctx, order.TenantID, data); err != nil {
		s.log.Warn("failed to publish pos.sale.finalized", zap.String("order_id", order.ID.String()), zap.Error(err))
	}

	// Stock backflush for the sale is handled EXCLUSIVELY by inventory-api's
	// pos.sale.finalized consumer (in-process, with BOM explosion) — see PublishSaleFinalized
	// above. We deliberately do NOT also call RecordConsumption here. The two paths each
	// deduct independently, so once the S2S client's request started succeeding (after the
	// order_id JSON-tag fix) every sold item was consumed twice (two consumptions per order).
	// backflushInventory is retained only for reference / non-sale direct backflush callers.
}

// resolveLineCosts returns SKU -> per-unit cost and SKU -> stock-unit (uom) maps for the
// sold lines, used to enrich the pos.sale.finalized payload with COGS figures and a real
// uom_code. Both are the inventory-synced values cached on POSCatalogOverride.metadata
// ("cost_price"/"uom", keyed by tenant + inventory_sku; see catalog/inventory_events.go).
// SKUs with no cached value are simply absent from the maps, so the caller reads 0/"" —
// a missing cost or unit must never block the sale.
func (s *Service) resolveLineCosts(ctx context.Context, tenantID uuid.UUID, lines []*ent.POSOrderLine) (map[string]float64, map[string]string) {
	costs := make(map[string]float64)
	uoms := make(map[string]string)
	if len(lines) == 0 {
		return costs, uoms
	}
	skus := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		if l.Sku == "" {
			continue
		}
		if _, ok := seen[l.Sku]; ok {
			continue
		}
		seen[l.Sku] = struct{}{}
		skus = append(skus, l.Sku)
	}
	if len(skus) == 0 {
		return costs, uoms
	}

	overrides, err := s.client.POSCatalogOverride.Query().
		Where(
			entcatalogoverride.TenantID(tenantID),
			entcatalogoverride.InventorySkuIn(skus...),
		).
		All(ctx)
	if err != nil {
		s.log.Warn("sale.finalized: failed to resolve item costs (defaulting to 0)",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return costs, uoms
	}
	for _, ov := range overrides {
		if ov.Metadata == nil {
			continue
		}
		switch v := ov.Metadata["cost_price"].(type) {
		case float64:
			costs[ov.InventorySku] = v
		case int:
			costs[ov.InventorySku] = float64(v)
		}
		if u, ok := ov.Metadata["uom"].(string); ok && u != "" {
			uoms[ov.InventorySku] = u
		}
	}
	return costs, uoms
}

// backflushInventory calls inventory-api to deduct stock for each sold item.
// Runs in a goroutine to avoid blocking the payment flow.
// Publishes pos.inventory.consumption.failed on error so a retry worker can re-attempt.
func (s *Service) backflushInventory(parent context.Context, order *ent.POSOrder, lines []*ent.POSOrderLine) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	items := make([]inventory.ConsumptionItem, 0, len(lines))
	for _, l := range lines {
		if l.Sku != "" {
			items = append(items, inventory.ConsumptionItem{
				SKU:      l.Sku,
				Quantity: float64(l.Quantity),
			})
		}
	}
	if len(items) == 0 {
		return
	}

	err := s.inventoryClient.RecordConsumption(ctx, order.TenantID.String(), inventory.ConsumptionRequest{
		OrderID: order.ID.String(),
		Items:   items,
	})
	if err != nil {
		s.log.Warn("inventory backflush failed",
			zap.String("order_id", order.ID.String()),
			zap.Error(err))
		if s.publisher != nil {
			_ = s.publisher.PublishInventoryConsumptionFailed(ctx, order.TenantID, map[string]any{
				"order_id":  order.ID.String(),
				"tenant_id": order.TenantID.String(),
				"items":     items,
				"error":     err.Error(),
			})
		}
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// calcCommissions creates CommissionRecord entries for the order's creator (staff member)
// based on active CommissionRule definitions. Called after order status → completed.
// Errors are logged but never block the caller — commission calc is best-effort.
func (s *Service) calcCommissions(ctx context.Context, order *ent.POSOrder) {
	// Resolve the staff member from the order's user_id.
	staffMembers, err := s.client.StaffMember.Query().
		Where(staffmemberent.TenantID(order.TenantID), staffmemberent.UserID(order.UserID)).
		All(ctx)
	if err != nil || len(staffMembers) == 0 {
		return
	}
	staffMember := staffMembers[0]

	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(order.ID)).
		All(ctx)
	if err != nil || len(lines) == 0 {
		return
	}

	// Fetch all active rules for this tenant once.
	rules, err := s.client.CommissionRule.Query().
		Where(entcommissionrule.TenantID(order.TenantID), entcommissionrule.IsActive(true)).
		All(ctx)
	if err != nil || len(rules) == 0 {
		return
	}

	for _, line := range lines {
		// Find the most specific rule: staff+item > staff-only > item-only > global.
		var bestRule *ent.CommissionRule
		for _, r := range rules {
			staffMatch := r.StaffMemberID == nil || *r.StaffMemberID == staffMember.ID
			itemMatch := r.CatalogItemID == nil || *r.CatalogItemID == line.CatalogItemID
			if !staffMatch || !itemMatch {
				continue
			}
			if bestRule == nil {
				bestRule = r
				continue
			}
			// More specific wins (non-nil fields beat nil).
			bestScore, rScore := 0, 0
			if bestRule.StaffMemberID != nil {
				bestScore++
			}
			if bestRule.CatalogItemID != nil {
				bestScore++
			}
			if r.StaffMemberID != nil {
				rScore++
			}
			if r.CatalogItemID != nil {
				rScore++
			}
			if rScore > bestScore {
				bestRule = r
			}
		}
		if bestRule == nil {
			continue
		}

		var rate, amount float64
		switch bestRule.RuleType {
		case "flat":
			if bestRule.FlatAmount != nil {
				amount = *bestRule.FlatAmount
			}
		default: // "percentage"
			if bestRule.Percentage != nil {
				rate = *bestRule.Percentage
				amount = line.TotalPrice * rate / 100
			}
		}
		if amount <= 0 {
			continue
		}

		creator := s.client.CommissionRecord.Create().
			SetTenantID(order.TenantID).
			SetStaffMemberID(staffMember.ID).
			SetOrderID(order.ID).
			SetOrderLineID(line.ID).
			SetServiceSku(line.Sku).
			SetSaleAmount(line.TotalPrice).
			SetCommissionRate(rate).
			SetCommissionAmount(amount).
			SetStatus("pending")
		if _, err := creator.Save(ctx); err != nil {
			s.log.Warn("commission record create failed",
				zap.String("order_id", order.ID.String()),
				zap.String("staff_id", staffMember.ID.String()),
				zap.Error(err))
		}
	}
}
