// Package saletax works out the VAT of a POS sale as declared: per line, with each line's own
// discount kept on that line and only the order discount shared across the lines before VAT,
// through the platform's shared calculator. It depends on no other module, so orders, payments
// (sale events) and printing (receipts) all use the same figures.
package saletax

import (
	"math"

	"github.com/Bengo-Hub/httpware/money"
	vat "github.com/Bengo-Hub/httpware/tax"

	"github.com/bengobox/pos-service/internal/ent"
)

// LineGross is what an order line costs with its VAT: total_price for an inclusive line, total_price
// plus tax_amount for an exclusive one.
func LineGross(l *ent.POSOrderLine) float64 {
	if !l.PriceIncludesTax && l.TaxAmount != nil && *l.TaxAmount > 0 {
		return l.TotalPrice + *l.TaxAmount
	}
	return l.TotalPrice
}

// LineDiscount is the discount stamped on one order line by a deal on that item (happy hour, buy
// one get one): metadata discount_amount. It is part of the order's discount_total but belongs to
// this line only. 0 when the line has none.
func LineDiscount(l *ent.POSOrderLine) float64 {
	if l == nil || l.Metadata == nil {
		return 0
	}
	switch v := l.Metadata["discount_amount"].(type) {
	case float64:
		return math.Max(0, v)
	case int:
		return math.Max(0, float64(v))
	}
	return 0
}

// SaleLineVATInput is one sold line as pos-api stores it: total_price (VAT-inclusive, or net with
// tax_amount on top for an exclusive line), the VAT recorded at the till and its rate. Discount is
// the part of the order discount that belongs to this line alone (LineDiscount), on the gross.
type SaleLineVATInput struct {
	TotalPrice float64
	TaxAmount  float64
	TaxRate    float64
	Inclusive  bool
	Discount   float64
}

// SaleLineVAT is a line after its discounts: what the customer paid for it and the VAT in that
// amount.
type SaleLineVAT struct {
	PaidGross float64
	Tax       float64
}

// gross is the line's price with its VAT.
func (l SaleLineVATInput) gross() float64 {
	if !l.Inclusive && l.TaxAmount > 0 {
		return l.TotalPrice + l.TaxAmount
	}
	return l.TotalPrice
}

// OrderDiscountVAT works out each line's VAT on what was paid for it, with the platform's shared
// calculator. discount is the order's whole discount_total. The part of it that belongs to single
// lines (SaleLineVATInput.Discount) comes off those lines only, at their own rate. Only the rest, a
// discount on the whole bill, is shared across the lines in proportion to their gross after their
// own discounts (largest remainder, so the shares add up exactly). VAT is due on the consideration
// actually paid, so this is the VAT the books, the eTIMS receipt and the printed receipt declare.
// The bill itself does not move: the lines still add up to the gross less the discount.
//
// ok is false when there is no discount; the recorded line figures then stand as they are.
func OrderDiscountVAT(lines []SaleLineVATInput, discount float64) (out []SaleLineVAT, ok bool) {
	if discount <= 0 || len(lines) == 0 {
		return nil, false
	}
	discount = money.Round2(discount)
	own := make([]float64, len(lines))
	var ownSum float64
	for i, l := range lines {
		own[i] = money.Round2(math.Min(math.Max(0, l.Discount), money.Round2(l.gross())))
		ownSum += own[i]
	}
	ownSum = money.Round2(ownSum)
	if ownSum > discount {
		// Line discounts never exceed the order's recorded discount: scale them down to it.
		if s := money.Split(discount, 2, len(own), own...); s != nil {
			own = s
		}
		ownSum = discount
	}
	doc := vat.Document{
		Discount:     money.Round2(discount - ownSum),
		DiscountBase: vat.DiscountOnGross,
		Lines:        make([]vat.Line, len(lines)),
	}
	for i, l := range lines {
		gross := l.gross()
		rate := l.TaxRate
		if rate <= 0 && l.TaxAmount > 0 && gross-l.TaxAmount > 0 {
			rate = l.TaxAmount / (gross - l.TaxAmount) * 100
		}
		if l.TaxAmount <= 0 {
			rate = 0 // a line that carried no VAT gets none
		}
		doc.Lines[i] = vat.Line{Quantity: 1, UnitPrice: money.Round2(gross), Discount: own[i], Rate: rate, Inclusive: true}
	}
	res := vat.Compute(doc)
	out = make([]SaleLineVAT, len(lines))
	for i, lr := range res.Lines {
		out[i] = SaleLineVAT{PaidGross: lr.Gross, Tax: lr.Tax}
	}
	return out, true
}
