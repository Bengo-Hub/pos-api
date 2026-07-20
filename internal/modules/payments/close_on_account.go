package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// CloseOnAccountRequest closes a partially-paid (or wholly unpaid) sale by booking its
// OUTSTANDING balance to the customer's treasury AR as a credit sale, then finalizing the order.
//
// This is the "put the remaining balance on account" path: whatever cash was already collected
// stays banked, the unpaid remainder becomes an AR debt the customer settles later (Receive
// Payment / settle-credit). It exists because a plain partial payment used to leave the order
// open with the receivable living ONLY in pos-api — treasury never learned of it, so the debtor
// was invisible (and uncollectable) on the treasury Customers page.
type CloseOnAccountRequest struct {
	TenantID   uuid.UUID
	TenantSlug string
	OrderID    uuid.UUID
	// Optional credit-sale details mirrored from the credit-sale modal: an explicit due date
	// (wins over the customer's treasury credit period, which wins over +30 days) and notes.
	PaymentDueDate *time.Time
	CreditNotes    string
	// TenderID is the tender the on-account POSPayment row is stamped with; uuid.Nil is accepted
	// (the same fallback the back-office Record-Payment modal uses).
	TenderID uuid.UUID
}

// CloseOnAccount books the order's unpaid remainder on account and finalizes the sale.
//
// It is deliberately a thin wrapper over recordCreditSale with the UNPAID REMAINDER as the
// on-account amount: that path already resolves the customer's CRM/AR key, enforces the credit
// limit in treasury, records the on-account POSPayment, stamps on_account + the due date, and
// runs completeOrderIfFullyPaid — so the order lands in the EXACT end-state of a normal mixed
// cash+credit sale (GL revenue/COGS, stock backflush, eTIMS and the treasury AR charge all fire
// through the one pos.sale.finalized path). Reusing it means there is a single credit-sale code
// path to keep correct.
//
// Idempotent: an order already booked on account (metadata.on_account) is a no-op that only
// re-runs completion — so a crash between the treasury AR post and completion can't strand the
// order open, and a double-click can't double-charge the customer's balance.
func (s *Service) CloseOnAccount(ctx context.Context, req CloseOnAccountRequest) (*CreateIntentResult, error) {
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(req.OrderID), posorder.TenantID(req.TenantID)).
		WithPayments().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: order not found: %w", err)
	}
	switch order.Status {
	case orders.StatusCancelled, orders.StatusVoided, orders.StatusRefunded:
		return nil, fmt.Errorf("payments: cannot put a %s order on account", order.Status)
	}
	// Already on account → only ensure it is finalized (idempotent recovery), never re-charge.
	if on, _ := order.Metadata["on_account"].(bool); on {
		s.completeOrderIfFullyPaid(ctx, order)
		return &CreateIntentResult{IsCash: true}, nil
	}

	outstanding := s.outstandingBalance(ctx, order)
	if outstanding <= 0.01 {
		return nil, fmt.Errorf("payments: order %s has no outstanding balance to put on account", order.OrderNumber)
	}
	currency := order.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}

	// recordCreditSale enforces "a real customer (phone), never Walk-in" and "treasury client
	// configured" itself, and returns a descriptive error the handler surfaces verbatim.
	return s.recordCreditSale(ctx, order, RecordPaymentRequest{
		TenantID:       req.TenantID,
		TenantSlug:     req.TenantSlug,
		OrderID:        req.OrderID,
		TenderID:       req.TenderID,
		TenderMethod:   TenderOnAccount,
		Amount:         outstanding,
		Currency:       currency,
		PaymentDueDate: req.PaymentDueDate,
		CreditNotes:    req.CreditNotes,
	}, currency)
}
