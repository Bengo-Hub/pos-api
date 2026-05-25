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
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	outletsettingpredicate "github.com/bengobox/pos-service/internal/ent/outletsetting"
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
	client           *ent.Client
	orderSvc         *orders.Service
	treasuryClient   *treasury.Client
	inventoryClient  *inventory.Client
	publisher        *events.Publisher
	log              *zap.Logger
	defaultCurrency  string
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

	currency := req.Currency
	if currency == "" {
		currency = s.defaultCurrency
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
	}

	intent, err := s.treasuryClient.CreateIntent(ctx, req.TenantSlug, intentReq)
	if err != nil {
		return nil, fmt.Errorf("payments: create treasury intent: %w", err)
	}

	result := &CreateIntentResult{
		PaymentIntentID: intent.ID,
		IsCash:          cash,
	}

	if !cash {
		// Digital: build the proxy initiateUrl that TreasuryPaymentModal will call
		result.InitiateURL = fmt.Sprintf("%s/api/v1/%s/pos/payments/initiate", req.PublicBaseURL, req.TenantSlug)

		// Record local payment as pending — will be completed by treasury NATS subscriber
		_, err = s.client.POSPayment.Create().
			SetOrderID(req.OrderID).
			SetTenderID(req.TenderID).
			SetAmount(req.Amount).
			SetCurrency(currency).
			SetStatus(StatusPending).
			SetNillableExternalReference(nilIfEmpty(intent.ID)).
			Save(ctx)
		if err != nil {
			s.log.Warn("failed to record pending payment", zap.Error(err))
		}
		return result, nil
	}

	// Cash / manual: record as completed immediately.
	// For manual payments use the cashier-entered ref; otherwise store the treasury intent ID.
	cashRef := intent.ID
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
		All(ctx)
	if err != nil {
		s.log.Warn("sale.finalized: failed to load lines", zap.String("order_id", order.ID.String()), zap.Error(err))
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
