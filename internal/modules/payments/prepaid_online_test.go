package payments

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

// The customer's online payment counts against the bill, so a prepaid online order shows nothing
// due at the till. Before this, CC-000017 (paid online, KES 700) showed 700 due and the cashier took
// the same M-Pesa code again, booking the sale twice.
func TestOnlinePrepaidCredit(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		tot  float64
		want float64
	}{
		{"till order", nil, 700, 0},
		{"online unpaid", map[string]any{"online_order_id": "x", "prepaid": false, "online_grand_total": 700.0}, 700, 0},
		{"online prepaid", map[string]any{"online_order_id": "x", "prepaid": true, "online_grand_total": 700.0}, 700, 700},
		{"prepaid, items added at the counter", map[string]any{"online_order_id": "x", "prepaid": true, "online_grand_total": 700.0}, 900, 700},
		{"prepaid, total missing", map[string]any{"online_order_id": "x", "prepaid": true}, 650, 650},
		{"prepaid flag without an online order", map[string]any{"prepaid": true, "online_grand_total": 700.0}, 700, 0},
	}
	for _, c := range cases {
		got := onlinePrepaidCredit(&ent.POSOrder{Metadata: c.meta, TotalAmount: c.tot})
		if got != c.want {
			t.Errorf("%s: onlinePrepaidCredit = %v, want %v", c.name, got, c.want)
		}
	}
}
