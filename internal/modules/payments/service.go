// Package payments provides the payment service layer for POS operations.
// It encapsulates payment recording, status management, and order completion
// logic that was previously hardcoded in handlers.
package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// PaymentStatus defines valid payment states.
const (
	StatusPending   = "pending"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusRefunded  = "refunded"
)

// RecordPaymentRequest holds input for recording a POS payment.
type RecordPaymentRequest struct {
	TenantID  uuid.UUID
	OrderID   uuid.UUID
	TenderID  uuid.UUID
	Amount    float64
	Currency  string
	Reference string // external reference (optional)
}

// Service provides payment business logic.
type Service struct {
	client          *ent.Client
	orderSvc        *orders.Service
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

// RecordPayment records a payment against an order and checks if the order is fully paid.
// Unlike the previous implementation which auto-completed every payment, this:
// 1. Records the payment with "completed" status (POS payments are immediate)
// 2. Checks if total payments >= order total
// 3. Only marks the order as "completed" when fully paid
func (s *Service) RecordPayment(ctx context.Context, req RecordPaymentRequest) (*ent.POSPayment, error) {
	// Validate order exists and belongs to tenant
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(req.OrderID), posorder.TenantID(req.TenantID)).
		WithPayments().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: order not found: %w", err)
	}

	// Don't allow payments on cancelled/voided orders
	if order.Status == orders.StatusCancelled || order.Status == orders.StatusVoided {
		return nil, fmt.Errorf("payments: cannot pay %s order", order.Status)
	}

	currency := req.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}

	// Record the payment (POS payments are immediate — status is "completed")
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

	// Check if order is now fully paid
	totalPaid := req.Amount
	for _, p := range order.Edges.Payments {
		if p.Status == StatusCompleted {
			totalPaid += p.Amount
		}
	}

	if totalPaid >= order.TotalAmount {
		// Mark order as completed only when fully paid
		if err := s.orderSvc.ValidateStatusTransition(order.Status, orders.StatusCompleted); err == nil {
			_, updateErr := s.client.POSOrder.Update().
				Where(posorder.ID(req.OrderID)).
				SetStatus(orders.StatusCompleted).
				Save(ctx)
			if updateErr != nil {
				s.log.Warn("failed to complete order after full payment",
					zap.String("order_id", req.OrderID.String()),
					zap.Error(updateErr))
			}
		}
	}

	return payment, nil
}

// ListOrderPayments returns all payments for an order.
func (s *Service) ListOrderPayments(ctx context.Context, tenantID, orderID uuid.UUID) ([]*ent.POSPayment, error) {
	// Verify order belongs to tenant
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

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
