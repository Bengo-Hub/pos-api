package orders

import (
	"github.com/Bengo-Hub/httpware/money"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/saletax"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// RefundLineInput is one returned or reversed POS line in the terms the sale used: the price paid
// for the quantity coming back and the VAT inside it, both before any order level discount.
type RefundLineInput struct {
	SKU      string
	Name     string
	Quantity float64
	Gross    float64
	Tax      float64
	TaxCode  string
}

// OrderDiscountRatio is the share of the order's gross that was discounted at the header. The sale
// spread the header discount over its lines by gross when it was sent to KRA, and a refund has to
// spread it the same way to reverse what was recorded. An exclusive line's total_price is net, so
// its VAT is added back to compare it with the order total.
func OrderDiscountRatio(orderTotal float64, orderLines []*ent.POSOrderLine) float64 {
	var gross float64
	for _, l := range orderLines {
		gross += saletax.LineGross(l)
	}
	if gross > orderTotal && gross > 0 {
		return (gross - orderTotal) / gross
	}
	return 0
}

// BuildRefundLines turns returned lines into the lines treasury raises the credit note from. Each
// line is netted of the order discount, then split into a price before tax and a tax rate so that
// treasury's own line arithmetic gives back the same gross the sale was recorded with.
func BuildRefundLines(discRatio float64, in []RefundLineInput) []treasury.RefundLine {
	out := make([]treasury.RefundLine, 0, len(in))
	for _, l := range in {
		if l.Quantity <= 0 || l.Gross <= 0 {
			continue
		}
		eff := l.Gross * (1 - discRatio)
		// A line that carried no VAT refunds none; treasury's registered-band check decides the
		// rest, the same way it did for the sale.
		tax := 0.0
		if l.Tax > 0 {
			tax = l.Tax * (1 - discRatio)
		}
		net := eff - tax
		rate := 0.0
		if net > 0 {
			rate = money.Round(tax/net*100, 4)
		}
		out = append(out, treasury.RefundLine{
			ItemSKU: l.SKU, Description: l.Name, Quantity: l.Quantity,
			UnitPrice: money.Round(net/l.Quantity, 6), TaxCode: l.TaxCode, TaxRate: rate,
		})
	}
	return out
}
