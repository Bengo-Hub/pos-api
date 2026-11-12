package payments

import (
	"github.com/bengobox/pos-service/internal/modules/saletax"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// shareOrderDiscountVAT puts the order's discount onto the sale event's lines before VAT
// (saletax.OrderDiscountVAT): a deal on one item (lineDiscounts, same order as items) comes off
// that line only, and the rest of the discount is shared across all lines. Each line gets
// paid_gross, what the customer paid for it, and its tax_amount becomes the VAT in that amount.
// treasury then books output VAT and sends eTIMS the VAT on the consideration actually paid, from
// the lines as given. An undiscounted sale's lines are left exactly as recorded.
func shareOrderDiscountVAT(items []map[string]any, lineDiscounts []float64, discount float64) {
	in := make([]saletax.SaleLineVATInput, len(items))
	for i, it := range items {
		in[i] = saletax.SaleLineVATInput{
			TotalPrice: num(it["total_price"]),
			TaxAmount:  num(it["tax_amount"]),
			TaxRate:    num(it["tax_rate"]),
			Inclusive:  it["price_includes_tax"] == true,
			Discount:   discountAt(lineDiscounts, i),
		}
	}
	out, ok := saletax.OrderDiscountVAT(in, discount)
	if !ok {
		return
	}
	for i := range items {
		items[i]["paid_gross"] = out[i].PaidGross
		if num(items[i]["tax_amount"]) > 0 {
			items[i]["tax_amount"] = out[i].Tax
		}
	}
}

// shareSignItemsDiscountVAT is shareOrderDiscountVAT for the synchronous checkout signing request,
// so the sync and async eTIMS payloads declare the same VAT.
func shareSignItemsDiscountVAT(items []treasury.SignPOSSaleItem, lineDiscounts []float64, discount float64) {
	in := make([]saletax.SaleLineVATInput, len(items))
	for i, it := range items {
		in[i] = saletax.SaleLineVATInput{TotalPrice: it.TotalPrice, TaxAmount: it.TaxAmount, TaxRate: it.TaxRate, Inclusive: it.PriceIncludesTax, Discount: discountAt(lineDiscounts, i)}
	}
	out, ok := saletax.OrderDiscountVAT(in, discount)
	if !ok {
		return
	}
	for i := range items {
		items[i].PaidGross = out[i].PaidGross
		if items[i].TaxAmount > 0 {
			items[i].TaxAmount = out[i].Tax
		}
	}
}

// discountAt is v[i], or 0 past its end.
func discountAt(v []float64, i int) float64 {
	if i < len(v) {
		return v[i]
	}
	return 0
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}
