package saletax

import (
	"sort"

	"github.com/Bengo-Hub/httpware/money"
)

// RateRow is one VAT rate on a receipt: the taxable value and the VAT at that rate.
type RateRow struct {
	Rate    float64 `json:"rate"`
	Taxable float64 `json:"taxable"`
	Tax     float64 `json:"tax"`
}

// RateSummary groups a sale's VAT by rate, on what was actually paid: the order discount is shared
// across the lines before VAT (OrderDiscountVAT), the same figures the sale event, treasury and KRA
// eTIMS carry. VAT inside VAT-inclusive prices is included. Lines with no VAT are left out. Rows are
// ordered by rate, highest first.
func RateSummary(lines []SaleLineVATInput, discount float64) []RateRow {
	paid, shared := OrderDiscountVAT(lines, discount)
	byRate := map[float64]*RateRow{}
	for i, l := range lines {
		gross := l.TotalPrice
		if !l.Inclusive && l.TaxAmount > 0 {
			gross += l.TaxAmount
		}
		tax := l.TaxAmount
		if shared {
			gross, tax = paid[i].PaidGross, paid[i].Tax
		}
		if tax <= 0 {
			continue
		}
		rate := l.TaxRate
		if rate <= 0 && gross-tax > 0 {
			rate = money.Round(tax/(gross-tax)*100, 2)
		}
		row := byRate[rate]
		if row == nil {
			row = &RateRow{Rate: rate}
			byRate[rate] = row
		}
		row.Taxable = money.Round2(row.Taxable + gross - tax)
		row.Tax = money.Round2(row.Tax + tax)
	}
	out := make([]RateRow, 0, len(byRate))
	for _, r := range byRate {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rate > out[j].Rate })
	return out
}
