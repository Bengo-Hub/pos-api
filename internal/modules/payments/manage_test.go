package payments

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

// TestPaymentIsManageable_CreditSettlement_VoidableRegardlessOfTenderType is the regression test
// for a real, confirmed gap: a credit-settlement row could be recorded with a gateway tender
// type (mpesa/bank/paystack — none in manualTenderTypes), and had NO correction path at all — a
// tenant/admin recording a settlement against the wrong order/amount could never fix it. A
// credit-settlement row must be voidable regardless of its tender type, since VoidPayment routes
// it through the dedicated treasury VoidARReceipt reversal, never the generic manual-tender
// cash-refund path this restriction exists to gate.
func TestPaymentIsManageable_CreditSettlement_VoidableRegardlessOfTenderType(t *testing.T) {
	cases := []struct {
		name       string
		tenderType string
		want       bool
	}{
		{"mpesa credit settlement", "mpesa", true},
		{"bank credit settlement", "bank", true},
		{"paystack credit settlement", "paystack", true},
		{"cash credit settlement", "cash", true}, // already manual, must stay true
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &ent.POSPayment{PaymentData: map[string]any{"method": c.tenderType, "credit_settlement": true}}
			if got := paymentIsManageable(p, c.tenderType); got != c.want {
				t.Errorf("paymentIsManageable(tender=%q, credit_settlement=true) = %v, want %v", c.tenderType, got, c.want)
			}
		})
	}
}

// TestPaymentIsManageable_NonSettlementGatewayTender_StillBlocked proves this fix didn't widen
// the gate for an ORDINARY (non-credit-settlement) gateway payment — those must still be
// unmanageable here, going through the refund flow instead, exactly as before.
func TestPaymentIsManageable_NonSettlementGatewayTender_StillBlocked(t *testing.T) {
	p := &ent.POSPayment{PaymentData: map[string]any{"method": "mpesa"}}
	if got := paymentIsManageable(p, "mpesa"); got != false {
		t.Errorf("paymentIsManageable(ordinary mpesa payment) = %v, want false (unchanged)", got)
	}
}

// TestPaymentIsManageable_TreasuryGateway_StillBlockedEvenIfMislabeledCreditSettlement guards the
// existing settled_via=treasury_gateway guard staying authoritative — it's checked first.
func TestPaymentIsManageable_TreasuryGateway_StillBlockedEvenIfMislabeledCreditSettlement(t *testing.T) {
	p := &ent.POSPayment{PaymentData: map[string]any{"settled_via": "treasury_gateway", "credit_settlement": true}}
	if got := paymentIsManageable(p, "cash"); got != false {
		t.Errorf("paymentIsManageable(settled_via=treasury_gateway) = %v, want false", got)
	}
}
