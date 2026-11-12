package printing

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// The refund channels added for refunds to the original method print a readable name; the older
// channels print as stored.
func TestReturnReceiptRefundChannelLabels(t *testing.T) {
	cases := map[posreturn.RefundChannel]string{
		posreturn.RefundChannel("mtn_momo_manual"):     "MTN MoMo",
		posreturn.RefundChannel("airtel_money_manual"): "Airtel Money",
		posreturn.RefundChannel("mpesa_manual"):        "M-Pesa",
		posreturn.RefundChannel("other_manual"):        "Other",
		posreturn.RefundChannel("card_manual"):         "Card",
		posreturn.RefundChannel("room_folio"):          "Room folio",
		posreturn.RefundChannel("cash"):                "cash",
	}
	for channel, want := range cases {
		c := channel
		v := BuildReturnReceiptView(&ent.POSReturn{ReturnNumber: "RET-1", RefundAmount: 10, RefundChannel: &c}, nil, ReturnReceiptOpts{})
		if v.PaymentMethod != want {
			t.Errorf("%s prints %q, want %q", channel, v.PaymentMethod, want)
		}
	}
}
