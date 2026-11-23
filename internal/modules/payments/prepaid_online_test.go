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

// The part of an online order's sale no till payment covers was collected by ordering and reaches
// treasury as an online_<method> tender, which draws Customer Deposits instead of booking cash.
func TestOnlineCollectedTender(t *testing.T) {
	online := map[string]any{"online_order_id": "x", "payment_method": "mpesa", "prepaid": true}
	paid := []saleTender{{Type: "cash", Amount: 200, Status: StatusCompleted}}

	if got, ok := onlineCollectedTender(&ent.POSOrder{Metadata: online, TotalAmount: 700}, nil); !ok || got.Type != "online_mpesa" || got.Amount != 700 {
		t.Errorf("prepaid online order: got %+v %v, want online_mpesa 700", got, ok)
	}
	if got, ok := onlineCollectedTender(&ent.POSOrder{Metadata: online, TotalAmount: 900}, paid); !ok || got.Amount != 700 {
		t.Errorf("200 added and paid at the till: got %+v %v, want 700 online", got, ok)
	}
	cod := map[string]any{"online_order_id": "x", "payment_method": "mpesa", "collected_payment_method": "cash"}
	if got, ok := onlineCollectedTender(&ent.POSOrder{Metadata: cod, TotalAmount: 500}, nil); !ok || got.Type != "online_cash" {
		t.Errorf("cash collected on delivery: got %+v %v, want online_cash", got, ok)
	}
	if _, ok := onlineCollectedTender(&ent.POSOrder{Metadata: online, TotalAmount: 200}, paid); ok {
		t.Error("fully paid at the till: no online tender")
	}
	if _, ok := onlineCollectedTender(&ent.POSOrder{TotalAmount: 700}, nil); ok {
		t.Error("till order: no online tender")
	}
}
