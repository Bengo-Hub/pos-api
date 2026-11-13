package orders

import (
	"testing"

	"github.com/shopspring/decimal"
)

// The till total follows the tenant's central tax setting (treasury tax_settings round_off and
// round_off_step). Order 000173 on codevertex-demo (2026-11-13): 4,384.00 less a 243.20 deal is
// 4,140.80, which the default rounds up to 4,141.
func TestFinalizeTotalsRoundOffPolicy(t *testing.T) {
	d := decimal.RequireFromString
	cases := []struct {
		policy    RoundOffPolicy
		total     string
		roundOff  string
	}{
		{DefaultRoundOff, "4141", "0.2"},
		{RoundOffPolicy{Mode: "none", Step: 1}, "4140.8", "0"},
		{RoundOffPolicy{Mode: "nearest", Step: 1}, "4141", "0.2"},
		{RoundOffPolicy{Mode: "nearest", Step: 5}, "4140", "-0.8"},
		{RoundOffPolicy{Mode: "up", Step: 5}, "4145", "4.2"},
		{RoundOffPolicy{Mode: "up", Step: 10}, "4150", "9.2"},
		{RoundOffPolicy{Mode: "bogus", Step: 0}, "4141", "0.2"}, // unknown falls back to the default
	}
	for _, c := range cases {
		got := finalizeTotals(c.policy, d("4384"), d("0"), d("243.2"), d("0"), d("0"))
		if !got.TotalAmount.Equal(d(c.total)) || !got.RoundOff.Equal(d(c.roundOff)) {
			t.Errorf("%+v: total %s round_off %s, want %s and %s", c.policy, got.TotalAmount, got.RoundOff, c.total, c.roundOff)
		}
		// The identity the receipts and books rely on still holds.
		if sum := got.Subtotal.Add(got.TaxTotal).Sub(got.DiscountTotal).Add(got.ChargesTotal).Add(got.RoundOff); !sum.Equal(got.TotalAmount) {
			t.Errorf("%+v: identity broken: %s != %s", c.policy, sum, got.TotalAmount)
		}
	}
}
