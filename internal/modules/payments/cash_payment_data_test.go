package payments

import "testing"

// TestCashPaymentData_OverTenderRecordsChange reproduces the alpha-china-market order #000530
// live bug: a 1,820 bill paid with a 2,000 note printed neither the amount tendered nor the
// change due anywhere on the receipt. Root cause: POSPayment.Amount is always capped to what the
// sale actually needed (never the raw cash handed over), and nothing else captured the surplus —
// so by the time a receipt is built, the 180 change is already gone. cashPaymentData is the fix's
// write side: it must stash the tendered/change figures in PaymentData whenever tendered > amount.
func TestCashPaymentData_OverTenderRecordsChange(t *testing.T) {
	data := cashPaymentData("cash", 1820, 2000)
	if data["method"] != "cash" {
		t.Fatalf("method = %v, want cash", data["method"])
	}
	tendered, ok := data["amount_tendered"].(float64)
	if !ok || tendered != 2000 {
		t.Fatalf("amount_tendered = %v, want 2000", data["amount_tendered"])
	}
	change, ok := data["change_due"].(float64)
	if !ok || change != 180 {
		t.Fatalf("change_due = %v, want 180", data["change_due"])
	}
}

// TestCashPaymentData_ExactTenderOmitsChangeFields is the non-regression half: an exact-cash sale
// (or any tender that never sends AmountTendered — every non-cash method, and every pre-existing
// caller) must keep writing exactly {"method": "..."}, byte-for-byte as before this fix, so no
// receipt anywhere starts printing a spurious "Tendered"/"Change" line for a sale that was never
// over-collected.
func TestCashPaymentData_ExactTenderOmitsChangeFields(t *testing.T) {
	for _, tc := range []struct {
		name           string
		amount         float64
		amountTendered float64
	}{
		{"exact cash", 1820, 1820},
		{"no tendered sent (legacy/non-cash caller)", 1820, 0},
		{"tendered within rounding epsilon", 1820, 1820.001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := cashPaymentData("cash", tc.amount, tc.amountTendered)
			if len(data) != 1 || data["method"] != "cash" {
				t.Fatalf("data = %#v, want only {method: cash}", data)
			}
		})
	}
}
