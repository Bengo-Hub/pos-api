package saletax

import (
	"testing"

	"github.com/Bengo-Hub/httpware/money"

	"github.com/bengobox/pos-service/internal/ent"
)

// bandLine is a test line with its OSCU band: A exempt, B 16%, C zero, D non-VAT, E 8%.
type bandLine struct {
	band string
	in   SaleLineVATInput
}

type bandSum struct{ taxable, tax float64 }

// Every expected figure below is worked out by hand: an inclusive line's VAT is
// gross x rate / (100 + rate), rounded to the cent, and its taxable value is gross less VAT.
func TestOrderDiscountVATLineVersusOrderDiscount(t *testing.T) {
	cases := []struct {
		name     string
		lines    []bandLine
		discount float64
		wantPaid []float64
		wantTax  []float64
		wantBand map[string]bandSum
	}{
		{
			// 50 off one B line only (happy hour on that item). The other lines keep their figures.
			name: "discount on one B line in a mixed sale",
			lines: []bandLine{
				{"A", SaleLineVATInput{TotalPrice: 200, Inclusive: true}},
				{"B", SaleLineVATInput{TotalPrice: 580, TaxAmount: 80, TaxRate: 16, Inclusive: true, Discount: 50}},
				{"B", SaleLineVATInput{TotalPrice: 116, TaxAmount: 16, TaxRate: 16, Inclusive: true}},
				{"C", SaleLineVATInput{TotalPrice: 150, Inclusive: true}},
				{"D", SaleLineVATInput{TotalPrice: 100, Inclusive: true}},
				{"E", SaleLineVATInput{TotalPrice: 108, TaxAmount: 8, TaxRate: 8, Inclusive: true}},
			},
			discount: 50,
			// 530 x 16 / 116 = 73.10
			wantPaid: []float64{200, 530, 116, 150, 100, 108},
			wantTax:  []float64{0, 73.10, 16, 0, 0, 8},
			wantBand: map[string]bandSum{
				"A": {200, 0}, "B": {556.90, 89.10}, "C": {150, 0}, "D": {100, 0}, "E": {100, 8},
			},
		},
		{
			// 54 off the E line: 216 x 8 / 108 = 16.00.
			name: "discount on an E line",
			lines: []bandLine{
				{"A", SaleLineVATInput{TotalPrice: 100, Inclusive: true}},
				{"B", SaleLineVATInput{TotalPrice: 116, TaxAmount: 16, TaxRate: 16, Inclusive: true}},
				{"E", SaleLineVATInput{TotalPrice: 270, TaxAmount: 20, TaxRate: 8, Inclusive: true, Discount: 54}},
			},
			discount: 54,
			wantPaid: []float64{100, 116, 216},
			wantTax:  []float64{0, 16, 16},
			wantBand: map[string]bandSum{"A": {100, 0}, "B": {100, 16}, "E": {200, 16}},
		},
		{
			// 100 off the whole bill of 1000: shares 20, 58 and 22 by gross.
			// B: 522 x 16 / 116 = 72.00. E: 198 x 8 / 108 = 14.67.
			name: "order discount over A, B and E lines",
			lines: []bandLine{
				{"A", SaleLineVATInput{TotalPrice: 200, Inclusive: true}},
				{"B", SaleLineVATInput{TotalPrice: 580, TaxAmount: 80, TaxRate: 16, Inclusive: true}},
				{"E", SaleLineVATInput{TotalPrice: 220, TaxAmount: 16.30, TaxRate: 8, Inclusive: true}},
			},
			discount: 100,
			wantPaid: []float64{180, 522, 198},
			wantTax:  []float64{0, 72, 14.67},
			wantBand: map[string]bandSum{"A": {180, 0}, "B": {450, 72}, "E": {183.33, 14.67}},
		},
		{
			// 50 deal on the B line plus 100 off the bill (discount_total 150). The 100 is shared on
			// what is left after the deal, 200 + 530 + 220 = 950: 21.05, 55.79, 23.16.
			// B: 474.21 x 16 / 116 = 65.41. E: 196.84 x 8 / 108 = 14.58.
			name: "line deal and order discount together",
			lines: []bandLine{
				{"A", SaleLineVATInput{TotalPrice: 200, Inclusive: true}},
				{"B", SaleLineVATInput{TotalPrice: 580, TaxAmount: 80, TaxRate: 16, Inclusive: true, Discount: 50}},
				{"E", SaleLineVATInput{TotalPrice: 220, TaxAmount: 16.30, TaxRate: 8, Inclusive: true}},
			},
			discount: 150,
			wantPaid: []float64{178.95, 474.21, 196.84},
			wantTax:  []float64{0, 65.41, 14.58},
			wantBand: map[string]bandSum{"A": {178.95, 0}, "B": {408.80, 65.41}, "E": {182.26, 14.58}},
		},
		{
			// An exclusive B line: net 500 plus 80 VAT is 580 gross; the 50 deal leaves 530.
			name: "discount on an exclusive B line",
			lines: []bandLine{
				{"A", SaleLineVATInput{TotalPrice: 200, Inclusive: true}},
				{"B", SaleLineVATInput{TotalPrice: 500, TaxAmount: 80, TaxRate: 16, Discount: 50}},
			},
			discount: 50,
			wantPaid: []float64{200, 530},
			wantTax:  []float64{0, 73.10},
			wantBand: map[string]bandSum{"A": {200, 0}, "B": {456.90, 73.10}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := make([]SaleLineVATInput, len(tc.lines))
			var gross float64
			for i, l := range tc.lines {
				in[i] = l.in
				gross += l.in.gross()
			}
			out, ok := OrderDiscountVAT(in, tc.discount)
			if !ok {
				t.Fatal("expected the discount to be applied")
			}
			var paid float64
			bands := map[string]bandSum{}
			for i, o := range out {
				if o.PaidGross != tc.wantPaid[i] || o.Tax != tc.wantTax[i] {
					t.Errorf("line %d (%s): got paid %.2f tax %.2f, want %.2f and %.2f",
						i, tc.lines[i].band, o.PaidGross, o.Tax, tc.wantPaid[i], tc.wantTax[i])
				}
				paid += o.PaidGross
				b := bands[tc.lines[i].band]
				b.taxable = money.Round2(b.taxable + o.PaidGross - o.Tax)
				b.tax = money.Round2(b.tax + o.Tax)
				bands[tc.lines[i].band] = b
			}
			for band, want := range tc.wantBand {
				if got := bands[band]; got != want {
					t.Errorf("band %s: got %+v, want %+v", band, got, want)
				}
			}
			if money.Round2(paid) != money.Round2(gross-tc.discount) {
				t.Errorf("lines add up to %.2f, want the bill less the discount %.2f", paid, gross-tc.discount)
			}
		})
	}
}

// Deals stamped on lines never take off more than the order's recorded discount.
func TestOrderDiscountVATLineDiscountCappedAtTotal(t *testing.T) {
	out, _ := OrderDiscountVAT([]SaleLineVATInput{
		{TotalPrice: 100, Inclusive: true},
		{TotalPrice: 580, TaxAmount: 80, TaxRate: 16, Inclusive: true, Discount: 60},
	}, 50)
	if out[0].PaidGross != 100 || out[1].PaidGross != 530 || out[1].Tax != 73.10 {
		t.Fatalf("got %+v, want the deal capped at 50 on the B line", out)
	}
}

// The receipt's per-rate block keeps a deal on the line it belongs to.
func TestRateSummaryLineDiscount(t *testing.T) {
	rows := RateSummary([]SaleLineVATInput{
		{TotalPrice: 200, Inclusive: true},
		{TotalPrice: 580, TaxAmount: 80, TaxRate: 16, Inclusive: true, Discount: 50},
		{TotalPrice: 116, TaxAmount: 16, TaxRate: 16, Inclusive: true},
		{TotalPrice: 108, TaxAmount: 8, TaxRate: 8, Inclusive: true},
	}, 50)
	want := []RateRow{{Rate: 16, Taxable: 556.90, Tax: 89.10}, {Rate: 8, Taxable: 100, Tax: 8}}
	if len(rows) != len(want) || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("got %+v, want %+v", rows, want)
	}
}

func TestLineDiscount(t *testing.T) {
	if got := LineDiscount(&ent.POSOrderLine{Metadata: map[string]any{"discount_amount": 25.5}}); got != 25.5 {
		t.Fatalf("got %v, want 25.5", got)
	}
	if got := LineDiscount(&ent.POSOrderLine{}); got != 0 {
		t.Fatalf("a line with no deal has no discount, got %v", got)
	}
}
