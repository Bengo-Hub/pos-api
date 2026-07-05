package handlers

import (
	"context"
	"fmt"
	"net/http"

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
// Returned goods restock via the existing exchange.completed event.

// SetOrderService wires the orders service used to create exchange replacement orders.
func (h *ReturnHandler) SetOrderService(svc *orders.Service) { h.orderSvc = svc }

// exchangeResult reports the replacement order + money split back to the till.
type exchangeResult struct {
	OrderID          uuid.UUID `json:"order_id"`
	OrderNumber      string    `json:"order_number"`
	ReplacementTotal float64   `json:"replacement_total"`
	ExchangeCredit   float64   `json:"exchange_credit"`
	// AmountPayable is what the customer still owes on the replacement order (dearer swap);
	// the till collects it through the normal payment flow.
	AmountPayable float64 `json:"amount_payable"`
	// Leftover is the value still owed TO the customer (cheaper swap); CompleteReturn
	// settles it in treasury via the chosen refund channel.
	Leftover float64 `json:"leftover_refund"`
}

// fulfilExchange creates the replacement order for an exchange return. No-op (nil, nil)
// for non-exchange returns.
func (h *ReturnHandler) fulfilExchange(ctx context.Context, r *http.Request, tid uuid.UUID, ret *ent.POSReturn, _ []*ent.POSReturnLine, input completeReturnInput) (*exchangeResult, error) {
	if ret.ReturnType != posreturn.ReturnTypeExchange {
		return nil, nil
	}
	if h.orderSvc == nil {
		return nil, fmt.Errorf("exchange fulfilment is not available (order service unwired)")
	}
	if len(input.ExchangeLines) == 0 {
		return nil, fmt.Errorf("an exchange requires at least one replacement item (exchange_lines)")
	}

	var replacementTotal float64
	orderLines := make([]orders.OrderLineInput, 0, len(input.ExchangeLines))
	for _, l := range input.ExchangeLines {
		if l.Quantity <= 0 || l.UnitPrice < 0 {
			return nil, fmt.Errorf("invalid replacement line %q: quantity and price must be positive", l.Name)
		}
		total := l.TotalPrice
		if total <= 0 {
			total = l.UnitPrice * l.Quantity
		}
		replacementTotal += total
		orderLines = append(orderLines, orders.OrderLineInput{
			CatalogItemID: l.CatalogItemID,
			SKU:           l.SKU,
			Name:          l.Name,
			Quantity:      l.Quantity,
			UnitPrice:     l.UnitPrice,
			TotalPrice:    total,
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

	// Carry the original buyer onto the replacement order (receipts, loyalty, AR linkage).
	customerName, customerPhone := "", ""
	if orig, err := h.client.POSOrder.Query().
		Where(entposorder.ID(ret.OrderID), entposorder.TenantID(tid)).
		Only(ctx); err == nil {
		if orig.CustomerName != nil {
			customerName = *orig.CustomerName
		}
		if orig.CustomerPhone != nil {
			customerPhone = *orig.CustomerPhone
		}
	}
	completedBy := uuid.Nil
	if uid, err := uuid.Parse(r.Header.Get("X-User-ID")); err == nil {
		completedBy = uid
	}

	order, err := h.orderSvc.CreateOrder(ctx, orders.CreateOrderRequest{
		TenantID:       tid,
		OutletID:       ret.OutletID,
		UserID:         completedBy,
		Currency:       "KES",
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
		if _, uerr := h.orderSvc.UpdateStatus(ctx, tid, order.ID, "completed"); uerr != nil {
			h.log.Warn("exchange: completing zero-balance replacement order failed",
				zap.String("order_id", order.ID.String()), zap.Error(uerr))
		}
	}

	return &exchangeResult{
		OrderID:          order.ID,
		OrderNumber:      order.OrderNumber,
		ReplacementTotal: replacementTotal,
		ExchangeCredit:   credit,
		AmountPayable:    payable,
		Leftover:         leftover,
	}, nil
}
