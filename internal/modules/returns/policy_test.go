package returns

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// The refund-method matrix: store credit is refused for a seller-fault reason, an unpaid credit
// sale must reduce what the customer owes, and a sale paid in full has no balance to reduce.
func TestValidateRefundChannel(t *testing.T) {
	damaged := posreturn.ReasonCodeDamaged
	refund := posreturn.ReturnTypeRefund
	for _, tc := range []struct {
		name                string
		reason              *posreturn.ReasonCode
		channel             string
		onAccount, restrict bool
		ok                  bool
	}{
		{name: "cash on a paid sale", channel: "cash", ok: true},
		{name: "reduce customer credit on a paid sale", channel: "offset_invoice", ok: false},
		{name: "reduce customer credit on a credit sale", channel: "offset_invoice", onAccount: true, restrict: true, ok: true},
		{name: "cash on a credit sale", channel: "cash", onAccount: true, restrict: true, ok: false},
		{name: "cash on a credit sale, guard switched off", channel: "cash", onAccount: true, restrict: false, ok: true},
		{name: "store credit for damaged goods", reason: &damaged, channel: "store_credit", ok: false},
		{name: "no channel yet", channel: "", ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRefundChannel(tc.reason, refund, tc.channel, tc.onAccount, tc.restrict)
			if (err == nil) != tc.ok {
				t.Fatalf("err %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
