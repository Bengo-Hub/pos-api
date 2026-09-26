package handlers

import "testing"

// A pay-on-collection order the counter already rang through checkout must be released without
// asking for the cash again; a part-paid one must not.
func TestPaidAtTerminal(t *testing.T) {
	cases := []struct {
		total, paid float64
		want        bool
	}{
		{160, 160, true},
		{160, 200, true},       // overpaid (change given) still covers it
		{0.3, 0.1 + 0.2, true}, // float rounding
		{160, 100, false},      // part paid
		{160, 0, false},
		{0, 0, false}, // nothing to take
	}
	for _, c := range cases {
		if got := paidAtTerminal(c.total, c.paid); got != c.want {
			t.Errorf("paidAtTerminal(%v, %v) = %v, want %v", c.total, c.paid, got, c.want)
		}
	}
}
