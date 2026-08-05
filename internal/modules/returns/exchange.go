package returns

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// Exchange fulfilment — the customer swaps returned goods for replacement items. The
// replacement is a REAL order (so stock, receipts, KDS-free retail flow, GL and reporting
// all use the normal sale pipeline) carrying an order-level "exchange credit" discount
// equal to the returned goods' value (capped at the replacement total), which nets the
// revenue without a second treasury posting:
//   - replacement dearer  → the order's payable balance is the top-up the customer pays
//     at the till through the ordinary payment flow;
//   - replacement cheaper → the leftover is refunded via the policy-validated channel
//     (CompleteReturn posts it to treasury);
//   - equal               → zero-cash exchange, the replacement order completes directly.
// Returned goods restock via the pos.exchange.completed event.

// fulfilExchange creates the replacement order for an exchange return. No-op (nil, nil)
// for non-exchange returns.
func (s *Service) fulfilExchange(ctx context.Context, tenantID uuid.UUID, ret *ent.POSReturn, req CompleteReturnRequest) (*ExchangeResult, error) {
	if ret.ReturnType != posreturn.ReturnTypeExchange {
		return nil, nil
	}
	if s.orderSvc == nil {
		return nil, fmt.Errorf("exchange fulfilment is not available (order service unwired)")
	}
	if len(req.ExchangeLines) == 0 {
		return nil, fmt.Errorf("an exchange requires at least one replacement item (exchange_lines)")
	}

	var replacementTotal float64
	orderLines := make([]orders.OrderLineInput, 0, len(req.ExchangeLines))
	for _, l := range req.ExchangeLines {
		if l.Quantity <= 0 || l.UnitPrice < 0 {
			return nil, fmt.Errorf("invalid replacement line %q: quantity and price must be positive", l.Name)
		}
		total := l.TotalPrice
		if total <= 0 {
			total = l.UnitPrice * l.Quantity
		}
		replacementTotal += total
		orderLines = append(orderLines, orders.OrderLineInput{
			CatalogItemID:    l.CatalogItemID,
			SKU:              l.SKU,
			Name:             l.Name,
			Quantity:         l.Quantity,
			UnitPrice:        l.UnitPrice,
			TotalPrice:       total,
			TaxCodeID:        l.TaxCodeID,
			PriceIncludesTax: l.PriceIncludesTax,
			TaxRate:          l.TaxRate,
		})
	}

	returnedValue := ret.RefundAmount
	credit := returnedValue
	if credit > replacementTotal {
		credit = replacementTotal
	}
	leftover := returnedValue - replacementTotal
	if leftover < 0 {
		leftover = 0
	}

	// Carry the original buyer onto the replacement order (receipts, loyalty, AR linkage) —
	// and its currency: a replacement order must match what the returned sale was actually
	// transacted in, not a hardcoded assumption.
	customerName, customerPhone, currency := "", "", "KES"
	if orig, err := s.client.POSOrder.Query().
		Where(entposorder.ID(ret.OrderID), entposorder.TenantID(tenantID)).
		Only(ctx); err == nil {
		if orig.CustomerName != nil {
			customerName = *orig.CustomerName
		}
		if orig.CustomerPhone != nil {
			customerPhone = *orig.CustomerPhone
		}
		if orig.Currency != "" {
			currency = orig.Currency
		}
	}

	order, err := s.orderSvc.CreateOrder(ctx, orders.CreateOrderRequest{
		TenantID:       tenantID,
		OutletID:       ret.OutletID,
		UserID:         req.CompletedBy,
		Currency:       currency,
		Lines:          orderLines,
		OrderSubtype:   "retail",
		Source:         "back_office",
		DiscountAmount: credit,
		CustomerName:   customerName,
		CustomerPhone:  customerPhone,
		Metadata: map[string]any{
			"exchange_return_id":  ret.ID.String(),
			"exchange_for_order":  ret.OrderID.String(),
			"exchange_credit":     credit,
			"exchange_return_num": ret.ReturnNumber,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create replacement order: %w", err)
	}

	payable := order.TotalAmount
	if payable <= 0.009 {
		// Even swap / cheaper replacement — nothing to collect, complete the replacement
		// order now so stock deduction + sale.finalized fire through the normal pipeline.
		payable = 0
		if _, uerr := s.orderSvc.UpdateStatus(ctx, tenantID, order.ID, "completed"); uerr != nil {
			s.log.Warn("exchange: completing zero-balance replacement order failed",
				zap.String("order_id", order.ID.String()), zap.Error(uerr))
		}
	}

	return &ExchangeResult{
		OrderID:          order.ID,
		OrderNumber:      order.OrderNumber,
		ReplacementTotal: replacementTotal,
		ExchangeCredit:   credit,
		AmountPayable:    payable,
		Leftover:         leftover,
	}, nil
}
