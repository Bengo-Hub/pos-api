package returns

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/payments"
)

type completionSpy struct{ orders []uuid.UUID }

func (c *completionSpy) RecheckCompletion(_ context.Context, _, orderID uuid.UUID) error {
	c.orders = append(c.orders, orderID)
	return nil
}

// An exchange replacement is sold at full price and paid with an exchange_credit tender, never
// discounted: a discount would take the returned value off KRA's figures a second time, on top of
// the return's credit note. A replacement the credit covers completes through the payment path.
func TestFulfilExchangeSellsReplacementAtFullPrice(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                      string
		replacement               float64
		credit, leftover, payable float64
		completes                 bool
	}{
		{name: "cheaper replacement", replacement: 300, credit: 300, leftover: 200, payable: 0, completes: true},
		{name: "even swap", replacement: 500, credit: 500, leftover: 0, payable: 0, completes: true},
		{name: "dearer replacement", replacement: 800, credit: 500, leftover: 0, payable: 300, completes: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, client := newTestService(t)
			svc.SetOrderService(orders.NewService(client, orders.Config{}, zap.NewNop()))
			spy := &completionSpy{}
			svc.SetOrderCompletion(spy)
			tid, outlet := uuid.New(), uuid.New()
			orig := seedOrder(t, client, tid, outlet, "completed")
			ret := &ent.POSReturn{ID: uuid.New(), TenantID: tid, OrderID: orig.ID, OutletID: outlet,
				ReturnType: posreturn.ReturnTypeExchange, RefundAmount: 500, ReturnNumber: "RET-1"}

			res, err := svc.fulfilExchange(ctx, tid, ret, CompleteReturnRequest{ExchangeLines: []LineInput{
				{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "Replacement", Quantity: 1, UnitPrice: tc.replacement, TotalPrice: tc.replacement},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if res.ExchangeCredit != tc.credit || res.Leftover != tc.leftover || res.AmountPayable != tc.payable {
				t.Fatalf("got credit %v leftover %v payable %v, want %v %v %v",
					res.ExchangeCredit, res.Leftover, res.AmountPayable, tc.credit, tc.leftover, tc.payable)
			}
			order, err := client.POSOrder.Get(ctx, res.OrderID)
			if err != nil {
				t.Fatal(err)
			}
			if order.TotalAmount != tc.replacement || order.DiscountTotal != 0 {
				t.Fatalf("replacement total %v discount %v, want full price %v and no discount", order.TotalAmount, order.DiscountTotal, tc.replacement)
			}
			pays, err := client.POSPayment.Query().Where(pospayment.OrderID(order.ID)).All(ctx)
			if err != nil || len(pays) != 1 {
				t.Fatalf("payments %v err %v, want one exchange credit payment", pays, err)
			}
			if m, _ := pays[0].PaymentData["method"].(string); m != payments.TenderExchangeCredit || pays[0].Amount != tc.credit || pays[0].Status != payments.StatusCompleted {
				t.Fatalf("payment %+v, want a completed %s payment of %v", pays[0], payments.TenderExchangeCredit, tc.credit)
			}
			if got := len(spy.orders) == 1; got != tc.completes {
				t.Fatalf("completion called %d times, want completes=%v", len(spy.orders), tc.completes)
			}
		})
	}
}
