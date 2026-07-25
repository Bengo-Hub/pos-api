package orders

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

// TestDerivePaymentStatus_CreditSaleSettledByReturnPlusPayment reproduces the reported bug: a
// credit (on-account) sale part-settled by a completed return-offset (never touches paid_total)
// and part-settled by a real payment must read "paid" once the two together cover the total —
// not stay stuck on "partial" forever.
func TestDerivePaymentStatus_CreditSaleSettledByReturnPlusPayment(t *testing.T) {
	got := DerivePaymentStatus("completed", 1000, 600, 400, true)
	if got != "paid" {
		t.Errorf("DerivePaymentStatus(completed, total=1000, collected=600, completedReturns=400, onAccount=true) = %q, want %q", got, "paid")
	}
}

func TestDerivePaymentStatus_CreditSalePartialWithoutReturn(t *testing.T) {
	got := DerivePaymentStatus("completed", 1000, 400, 0, true)
	if got != "partial" {
		t.Errorf("DerivePaymentStatus(collected=400, completedReturns=0, onAccount=true) = %q, want %q", got, "partial")
	}
}

func TestDerivePaymentStatus_CreditSaleDueWithoutAnySettlement(t *testing.T) {
	got := DerivePaymentStatus("completed", 1000, 0, 0, true)
	if got != "due" {
		t.Errorf("DerivePaymentStatus(collected=0, completedReturns=0, onAccount=true) = %q, want %q", got, "due")
	}
}

func TestDerivePaymentStatus_CashSaleUnaffectedByReturns(t *testing.T) {
	// Non-on-account completed orders always read "paid" regardless of collected/returns — the
	// existing masking behavior (status=="completed") must be unchanged.
	got := DerivePaymentStatus("completed", 1000, 0, 0, false)
	if got != "paid" {
		t.Errorf("DerivePaymentStatus(cash, completed) = %q, want %q", got, "paid")
	}
}

func TestComputeSettlement_CreditSaleFullySettledByReturnPlusPayment(t *testing.T) {
	// Mirrors the reported scenario end-to-end via ComputeSettlement (AmountDue and
	// PaymentStatus must agree): total 1000, a completed return offsets 400, a payment
	// collects the remaining 600.
	o := &ent.POSOrder{
		Status:      "completed",
		TotalAmount: 1000,
		PaidTotal:   600,
		Metadata:    map[string]any{"on_account": true},
	}
	st := ComputeSettlement(o, 400)
	if st.PaymentStatus != "paid" {
		t.Errorf("PaymentStatus = %q, want %q", st.PaymentStatus, "paid")
	}
	if st.AmountDue != 0 {
		t.Errorf("AmountDue = %v, want 0", st.AmountDue)
	}
}
