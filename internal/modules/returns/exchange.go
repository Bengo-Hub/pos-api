package returns

import (
	"context"
	"fmt"
	"math"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/payments"
)

// Exchange fulfilment: the customer swaps returned goods for replacement items. KRA treats an
// exchange as a refund of the returned goods plus a new sale, so that is what happens here:
//   - the return is refunded in full (CompleteReturn): its credit note goes to eTIMS and treasury
//     parks the "exchange credit" (the returned value, capped at the replacement total) in
//     exchange clearing, paying out only any leftover through the chosen channel;
//   - the replacement is a REAL order at full price (stock, receipts, eTIMS, GL and reporting use
//     the normal sale pipeline), paid with an exchange_credit tender for that credit, which
//     treasury books against exchange clearing. Never a discount: a discount would take the
//     returned value off KRA's figures a second time.
//
// Replacement dearer: the order's balance is the top-up the customer pays at the till. Cheaper:
// the leftover is refunded. Equal: nothing to pay, the replacement order completes directly.
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

	orderLines := make([]orders.OrderLineInput, 0, len(req.ExchangeLines))
	for _, l := range req.ExchangeLines {
		if l.Quantity <= 0 || l.UnitPrice < 0 {
			return nil, fmt.Errorf("invalid replacement line %q: quantity and price must be positive", l.Name)
		}
		total := l.TotalPrice
		if total <= 0 {
			total = l.UnitPrice * l.Quantity
		}
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
		TenantID:      tenantID,
		OutletID:      ret.OutletID,
		UserID:        req.CompletedBy,
		Currency:      currency,
		Lines:         orderLines,
		OrderSubtype:  "retail",
		Source:        "back_office",
		CustomerName:  customerName,
		CustomerPhone: customerPhone,
		Metadata: map[string]any{
			"exchange_return_id":  ret.ID.String(),
			"exchange_for_order":  ret.OrderID.String(),
			"exchange_return_num": ret.ReturnNumber,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create replacement order: %w", err)
	}

	// The credit is measured against what the replacement actually costs, VAT included.
	returnedValue := ret.RefundAmount
	credit := math.Min(returnedValue, order.TotalAmount)
	leftover := math.Max(returnedValue-credit, 0)
	if credit > 0.009 {
		if _, err := s.client.POSPayment.Create().
			SetOrderID(order.ID).
			SetTenderID(uuid.Nil).
			SetAmount(credit).
			SetCurrency(currency).
			SetStatus(payments.StatusCompleted).
			SetPaymentData(map[string]any{"method": payments.TenderExchangeCredit, "return_id": ret.ID.String()}).
			SetNillableExternalReference(&ret.ReturnNumber).
			Save(ctx); err != nil {
			return nil, fmt.Errorf("record exchange credit on the replacement order: %w", err)
		}
		// Shift cash-up's legacy fallback reads the credit off the replacement order.
		md := cloneReturnMetadata(order.Metadata)
		md["exchange_credit"] = credit
		if _, err := s.client.POSOrder.UpdateOneID(order.ID).SetMetadata(md).Save(ctx); err != nil {
			s.log.Warn("exchange: stamping the credit on the replacement order failed", zap.String("order_id", order.ID.String()), zap.Error(err))
		}
	}

	payable := math.Max(order.TotalAmount-credit, 0)
	if payable <= 0.009 {
		// Even swap or cheaper replacement: the exchange credit pays it in full. Complete it the
		// way a till payment does, so pos.sale.finalized carries it to treasury, KRA and stock.
		payable = 0
		if s.completion == nil {
			s.log.Warn("exchange: payment completion not wired; replacement order left open", zap.String("order_id", order.ID.String()))
		} else if cerr := s.completion.RecheckCompletion(ctx, tenantID, order.ID); cerr != nil {
			s.log.Warn("exchange: completing the paid replacement order failed",
				zap.String("order_id", order.ID.String()), zap.Error(cerr))
		}
	}

	return &ExchangeResult{
		OrderID:          order.ID,
		OrderNumber:      order.OrderNumber,
		ReplacementTotal: order.TotalAmount,
		ExchangeCredit:   credit,
		AmountPayable:    payable,
		Leftover:         leftover,
	}, nil
}
