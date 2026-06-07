// Package payments provides the payment service layer for POS operations.
package payments

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcommissionrule "github.com/bengobox/pos-service/internal/ent/commissionrule"
	outletsettingpredicate "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	staffmemberent "github.com/bengobox/pos-service/internal/ent/staffmember"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tableassignment"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/treasury"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// PaymentStatus defines valid payment states.
const (
	StatusPending   = "pending"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusRefunded  = "refunded"
)

// isCashMethod returns true for tender types that settle immediately without treasury round-trip.
func isCashMethod(method string) bool {
	m := strings.ToLower(method)
	return m == "cash" || m == "manual" || m == "room_charge"
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
		paymentMethod = "cash"
	}

	intentReq := treasury.CreateIntentRequest{
		SourceService: "pos",
		ReferenceID:   req.OrderID.String(),
		ReferenceType: "pos_order",
		Amount:        req.Amount,
		Currency:      currency,
		PaymentMethod: paymentMethod,
		Description:   fmt.Sprintf("POS order %s", order.OrderNumber),
		OutletID:      order.OutletID.String(),
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
			SetNillableExternalReference(nilIfEmpty(intent.ResolvedID())).
			Save(ctx)
		if err != nil {
			s.log.Warn("failed to record pending payment", zap.Error(err))
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
		SetNillableExternalReference(nilIfEmpty(cashRef)).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: record cash payment: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order, req.Amount)
	return result, nil
}

// recordCreditSale settles an order "on account" (credit sale). It posts the amount to the
// customer's AR balance in treasury FIRST — treasury enforces the customer credit limit and
// rejects over-limit sales — and only on success records a completed on-account payment and
// completes the order (so goods never leave without the debt being recorded). Requires a customer.
func (s *Service) recordCreditSale(ctx context.Context, order *ent.POSOrder, req RecordPaymentRequest, currency string) (*CreateIntentResult, error) {
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
	if strings.TrimSpace(phone) == "" && strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("payments: credit sale requires a customer (phone or name)")
	}

	if _, err := s.treasuryClient.RecordCreditSale(ctx, req.TenantSlug, treasury.CreditSaleRequest{
		CustomerIdentifier: phone,
		CustomerName:       name,
		POSOrderID:         order.ID.String(),
		Amount:             req.Amount,
		Currency:           currency,
	}); err != nil {
		return nil, fmt.Errorf("payments: credit sale rejected: %w", err)
	}

	if _, err := s.client.POSPayment.Create().
		SetOrderID(req.OrderID).
		SetTenderID(req.TenderID).
		SetAmount(req.Amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		Save(ctx); err != nil {
		return nil, fmt.Errorf("payments: record on-account payment: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order, req.Amount)
	return &CreateIntentResult{IsCash: true}, nil
}

// ConfirmPaymentByIntentID is called by the treasury.payment.success NATS subscriber.
// It finds the pending local payment for the given treasury intent ID and marks it completed,
// then completes the order if fully paid.
func (s *Service) ConfirmPaymentByIntentID(ctx context.Context, tenantID uuid.UUID, intentID string) error {
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
		_, err := s.client.POSPayment.UpdateOne(p).
			SetStatus(StatusCompleted).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("payments: confirm payment %s: %w", p.ID, err)
		}
		if p.Edges.Order != nil {
			s.completeOrderIfFullyPaid(ctx, p.Edges.Order, 0)
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

	s.completeOrderIfFullyPaid(ctx, order, req.Amount)
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

// completeOrderIfFullyPaid checks total payments and marks order completed when fully covered.
func (s *Service) completeOrderIfFullyPaid(ctx context.Context, order *ent.POSOrder, additionalAmount float64) {
	payments, err := s.client.POSPayment.Query().
		Where(pospayment.OrderID(order.ID), pospayment.Status(StatusCompleted)).
		All(ctx)
	if err != nil {
		return
	}

	var totalPaid float64
	for _, p := range payments {
		totalPaid += p.Amount
	}
	totalPaid += additionalAmount

	if totalPaid >= order.TotalAmount {
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

	items := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		item := map[string]any{
			"sku":                l.Sku,
			"name":               l.Name,
			"quantity":           l.Quantity,
			"unit_price":         l.UnitPrice,
			"total_price":        l.TotalPrice,
			"uom_code":           "",
			"price_includes_tax": l.PriceIncludesTax,
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

	// Backflush inventory consumption asynchronously — non-blocking, publish retry event on failure.
	if s.inventoryClient != nil {
		go s.backflushInventory(order, lines)
	}
}

// backflushInventory calls inventory-api to deduct stock for each sold item.
// Runs in a goroutine to avoid blocking the payment flow.
// Publishes pos.inventory.consumption.failed on error so a retry worker can re-attempt.
func (s *Service) backflushInventory(order *ent.POSOrder, lines []*ent.POSOrderLine) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
