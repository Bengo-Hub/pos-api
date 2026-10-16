package returns

import (
	"context"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// fiscalRefund is what treasury needs to send a refund to KRA for a returned, fiscalised sale. The
// zero value means the sale was never fiscalised and the credit note stays a record only document.
type fiscalRefund struct {
	orderID  string
	outletID string
	currency string
	lines    []treasury.RefundLine
}

// fiscalRefundFor builds the refund lines for a return when its original sale was transmitted to
// KRA. Lines are priced from the sale (price paid, VAT inside it, the order discount spread the way
// the sale spread it), never from the refund amount, so the refund reverses what KRA recorded.
func (s *Service) fiscalRefundFor(ctx context.Context, tenantSlug string, ret *ent.POSReturn, returned []*ent.POSReturnLine) fiscalRefund {
	if s.treasuryClient == nil {
		return fiscalRefund{}
	}
	switch ret.ReturnType {
	case posreturn.ReturnTypeRefund, posreturn.ReturnTypeStoreCredit, posreturn.ReturnTypeExchange:
	default:
		return fiscalRefund{}
	}
	if fiscalized, _, _ := orders.IsFiscalized(ctx, s.treasuryClient, tenantSlug, ret.OrderID); !fiscalized {
		return fiscalRefund{}
	}
	order, err := s.client.POSOrder.Get(ctx, ret.OrderID)
	if err != nil {
		s.log.Warn("fiscal refund: could not load the original order", zap.Error(err), zap.String("return_id", ret.ID.String()))
		return fiscalRefund{}
	}
	orderLines, err := s.client.POSOrderLine.Query().Where(entposorderline.OrderID(ret.OrderID)).All(ctx)
	if err != nil {
		s.log.Warn("fiscal refund: could not load the original order lines", zap.Error(err), zap.String("return_id", ret.ID.String()))
		return fiscalRefund{}
	}
	byID := make(map[string]*ent.POSOrderLine, len(orderLines))
	for _, ol := range orderLines {
		byID[ol.ID.String()] = ol
	}
	inputs := make([]orders.RefundLineInput, 0, len(returned))
	for _, l := range returned {
		in := orders.RefundLineInput{SKU: l.Sku, Name: l.Name, Quantity: l.Quantity, Gross: l.TotalPrice}
		if ol, ok := byID[l.OrderLineID.String()]; ok {
			in.TaxCode = ol.TaxCodeID
			if ol.TaxAmount != nil && *ol.TaxAmount > 0 && ol.Quantity > 0 {
				ratio := l.Quantity / ol.Quantity
				if ratio > 1 {
					ratio = 1
				}
				in.Tax = *ol.TaxAmount * ratio
			}
		}
		inputs = append(inputs, in)
	}
	lines := orders.BuildRefundLines(orders.OrderDiscountRatio(order.TotalAmount, orderLines), inputs)
	if len(lines) == 0 {
		return fiscalRefund{}
	}
	currency := order.Currency
	if currency == "" {
		currency = "KES"
	}
	return fiscalRefund{orderID: ret.OrderID.String(), outletID: ret.OutletID.String(), currency: currency, lines: lines}
}

// raiseFiscalCreditNote asks treasury for the credit note on its own, for a return that settled no
// money through the refund call (an exchange with nothing left over, or a refund call that failed).
// Treasury keys it on the return id, so the credit note the refund call raises later is the same one.
func (s *Service) raiseFiscalCreditNote(ctx context.Context, tenantSlug string, ret *ent.POSReturn, f fiscalRefund) {
	crmContactID, customerName, customerIdentifier := orders.ResolveOrderCustomer(ctx, s.client, ret.TenantID, ret.OrderID)
	cn, err := s.treasuryClient.RaiseReturnCreditNote(ctx, tenantSlug, treasury.ReturnCreditNoteRequest{
		ReferenceID: ret.ID.String(), Reference: ret.ReturnNumber, OriginalOrderID: f.orderID, OutletID: f.outletID,
		CustomerName: customerName, CustomerPhone: customerIdentifier, CrmContactID: crmContactID,
		Currency: f.currency, Reason: ret.Reason, Lines: f.lines,
	})
	if err != nil {
		s.log.Warn("eTIMS credit note for return failed (non fatal)", zap.Error(err), zap.String("return_id", ret.ID.String()))
		return
	}
	s.log.Info("eTIMS credit note raised for return", zap.String("return_id", ret.ID.String()), zap.String("credit_note", cn.Number))
}
