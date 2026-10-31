package orders

import (
	"github.com/Bengo-Hub/httpware/money"
	vat "github.com/Bengo-Hub/httpware/tax"
)

// SaleLineVATInput is one sold line as pos-api stores it: total_price (VAT-inclusive, or net with
// tax_amount on top for an exclusive line), the VAT recorded at the till and its rate.
type SaleLineVATInput struct {
	TotalPrice float64
	TaxAmount  float64
	TaxRate    float64
	Inclusive  bool
}

// SaleLineVAT is a line after its share of the order discount: what the customer paid for it and
// the VAT in that amount.
type SaleLineVAT struct {
	PaidGross float64
	Tax       float64
}

// OrderDiscountVAT shares an order (header) discount across the lines before VAT, with the
// platform's shared calculator: each line's share is in proportion to its gross (largest
// remainder, so the shares add up to the discount exactly) and its VAT is worked out on what was
// paid for it. VAT is due on the consideration actually paid, so this is the VAT the books, the
// eTIMS receipt and the printed receipt declare. The bill itself does not move: the lines still add
// up to the gross less the discount.
//
// ok is false when there is no discount to share; the recorded line figures then stand as they are.
func OrderDiscountVAT(lines []SaleLineVATInput, discount float64) (out []SaleLineVAT, ok bool) {
	if discount <= 0 || len(lines) == 0 {
		return nil, false
	}
	doc := vat.Document{Discount: discount, DiscountBase: vat.DiscountOnGross, Lines: make([]vat.Line, len(lines))}
	for i, l := range lines {
		gross := l.TotalPrice
		if !l.Inclusive && l.TaxAmount > 0 {
			gross += l.TaxAmount
		}
		rate := l.TaxRate
		if rate <= 0 && l.TaxAmount > 0 && gross-l.TaxAmount > 0 {
			rate = l.TaxAmount / (gross - l.TaxAmount) * 100
		}
		if l.TaxAmount <= 0 {
			rate = 0 // a line that carried no VAT gets none
		}
		doc.Lines[i] = vat.Line{Quantity: 1, UnitPrice: money.Round2(gross), Rate: rate, Inclusive: true}
	}
	res := vat.Compute(doc)
	out = make([]SaleLineVAT, len(lines))
	for i, lr := range res.Lines {
		out[i] = SaleLineVAT{PaidGross: lr.Gross, Tax: lr.Tax}
	}
	return out, true
}
