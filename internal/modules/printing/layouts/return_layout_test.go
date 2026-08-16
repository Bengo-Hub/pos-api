package layouts

import (
	"strings"
	"testing"
)

// returnFixture is the sale fixture re-framed as a refund document.
func returnFixture() Receipt {
	rec := fixtureReceipt("retail")
	rec.ReceiptNumber = "RET-000123"
	rec.OrderNumber = ""
	rec.IsReturn = true
	rec.OriginalOrderNumber = "POS-000450"
	rec.PaymentMethod = "mpesa"
	// A refund carries no fiscal identity of its own on this document.
	rec.EtimsKraPin, rec.EtimsScuID, rec.EtimsCuInvNo, rec.EtimsRcptSign = "", "", "", ""
	rec.EtimsQRCodeURL, rec.EtimsQRPNG, rec.BarcodePNG, rec.BarcodeValue = "", "", "", ""
	return rec
}

// Every layout (HTML and PDF) must swap the grand-total label to "REFUND TOTAL" and name the sale
// the refund is against — the two things that stop a refund slip reading like a fresh sale.
func TestAllLayoutsRenderReturnFraming(t *testing.T) {
	rec := returnFixture()
	for _, l := range All() {
		rec.Layout = l.ID
		html := string(RenderHTML(rec, ""))
		if !strings.Contains(html, "REFUND TOTAL") {
			t.Errorf("layout %s HTML: missing REFUND TOTAL label", l.ID)
		}
		if strings.Contains(html, ">TOTAL<") {
			t.Errorf("layout %s HTML: still renders a bare TOTAL label on a return", l.ID)
		}
		if !strings.Contains(html, "POS-000450") {
			t.Errorf("layout %s HTML: missing the original order number", l.ID)
		}

		pdf, err := RenderPDF(rec, Brand{CompanyName: "BOI Enterprises"})
		if err != nil {
			t.Fatalf("layout %s PDF render failed: %v", l.ID, err)
		}
		if len(pdf) == 0 {
			t.Errorf("layout %s produced an empty PDF", l.ID)
		}
	}
}

// The same fixture as an ordinary SALE must keep printing "TOTAL" — the return framing is
// strictly opt-in, so no existing receipt changes.
func TestSaleLayoutsKeepPlainTotal(t *testing.T) {
	rec := fixtureReceipt("retail")
	for _, l := range All() {
		rec.Layout = l.ID
		html := string(RenderHTML(rec, ""))
		if strings.Contains(html, "REFUND TOTAL") {
			t.Errorf("layout %s: a sale receipt must never say REFUND TOTAL", l.ID)
		}
		if strings.Contains(html, "Return against Order") {
			t.Errorf("layout %s: a sale receipt must never carry the return-against line", l.ID)
		}
	}
}

// totalLabel/returnAgainstLine are the two shared helpers the four renderers call — one place
// to change the wording, so the layouts can never drift apart.
func TestReturnLabelHelpers(t *testing.T) {
	sale := Receipt{}
	if got := totalLabel(sale, ":"); got != "TOTAL:" {
		t.Errorf("totalLabel(sale) = %q, want TOTAL:", got)
	}
	if got := returnAgainstLine(sale); got != "" {
		t.Errorf("returnAgainstLine(sale) = %q, want empty", got)
	}
	ret := Receipt{IsReturn: true, OriginalOrderNumber: "POS-000450"}
	if got := totalLabel(ret, ""); got != "REFUND TOTAL" {
		t.Errorf("totalLabel(return) = %q, want REFUND TOTAL", got)
	}
	if got := returnAgainstLine(ret); got != "Return against Order #POS-000450" {
		t.Errorf("returnAgainstLine(return) = %q", got)
	}
	// A return whose original order couldn't be resolved prints no dangling "#" line.
	if got := returnAgainstLine(Receipt{IsReturn: true}); got != "" {
		t.Errorf("returnAgainstLine(unknown original) = %q, want empty", got)
	}
}
