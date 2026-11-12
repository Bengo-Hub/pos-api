package printing

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

func copyTestOrder(reprints int) *ent.POSOrder {
	cuInv := "KRACU0300003541/340368"
	scu := "KRACU0300003541"
	return &ent.POSOrder{
		ID: uuid.New(), OrderNumber: "POS-COPY1", Status: "completed", Currency: "KES",
		Subtotal: 100, TotalAmount: 100, PaidTotal: 100, CreatedAt: time.Now(),
		ReprintCount: reprints, EtimsCuInvNo: &cuInv, EtimsScuID: &scu,
	}
}

// Only the "Print copy" action (Copy) makes a COPY customer receipt; a reprint is the original
// (owner decision 2026-11-12). Kitchen and waiter slips are never fiscal documents and never marked.
func TestBuildReceiptView_IsCopy(t *testing.T) {
	cases := []struct {
		name     string
		reprints int
		opts     ReceiptViewOpts
		want     bool
	}{
		{"original", 0, ReceiptViewOpts{}, false},
		{"plain reprint stays the original", 1, ReceiptViewOpts{}, false},
		{"print copy", 0, ReceiptViewOpts{Copy: true}, true},
		{"print copy after reprints", 3, ReceiptViewOpts{Copy: true}, true},
		{"kitchen ticket", 2, ReceiptViewOpts{Type: "kitchen_ticket", Copy: true}, false},
	}
	for _, c := range cases {
		v := BuildReceiptView(copyTestOrder(c.reprints), nil, nil, nil, c.opts)
		if v.IsCopy != c.want {
			t.Errorf("%s: IsCopy = %v, want %v", c.name, v.IsCopy, c.want)
		}
	}
}

// The ESC/POS bytes carry the COPY banner at the top and bottom of a copy and the KRA label CS;
// the original carries no banner and the label NS.
func TestBuildReceipt_EscposCopyMarking(t *testing.T) {
	original := string(BuildReceipt(OrderReceiptDataOpts(copyTestOrder(0), nil, nil, nil, ReceiptViewOpts{})))
	if strings.Contains(original, CopyBanner) {
		t.Errorf("original ESC/POS: must not print the COPY banner")
	}
	if !strings.Contains(original, formatLine("Receipt Type", "NS")) {
		t.Errorf("original ESC/POS: missing KRA receipt label NS")
	}

	// A plain reprint stays the original; only Print copy (Copy) marks it.
	if reprint := string(BuildReceipt(OrderReceiptDataOpts(copyTestOrder(1), nil, nil, nil, ReceiptViewOpts{}))); strings.Contains(reprint, CopyBanner) {
		t.Errorf("plain reprint ESC/POS: must not print the COPY banner")
	}
	copyBytes := string(BuildReceipt(OrderReceiptDataOpts(copyTestOrder(1), nil, nil, nil, ReceiptViewOpts{Copy: true})))
	if n := strings.Count(copyBytes, CopyBanner); n != 2 {
		t.Errorf("copy ESC/POS: COPY banner printed %d times, want 2 (top and bottom)", n)
	}
	if !strings.Contains(copyBytes, formatLine("Receipt Type", "CS")) {
		t.Errorf("copy ESC/POS: missing KRA receipt label CS")
	}
	if !strings.Contains(copyBytes, "KRACU0300003541/340368") {
		t.Errorf("copy ESC/POS: lost the original's CU invoice number")
	}

	refundCopy := ReceiptData{Type: "customer", IsCopy: true, IsReturn: true, EtimsCuInvNo: "KRACU0300003541/340369"}
	if got := string(BuildReceipt(refundCopy)); !strings.Contains(got, formatLine("Receipt Type", "CC")) {
		t.Errorf("refund copy ESC/POS: missing KRA receipt label CC")
	}
}
