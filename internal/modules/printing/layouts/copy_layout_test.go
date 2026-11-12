package layouts

import (
	"strings"
	"testing"

	"github.com/bengobox/pos-service/internal/modules/printing"
)

// A reprint must be marked COPY (KRA copy marking): the thermal HTML prints the banner at the top
// and the bottom, and the KRA receipt label reads CS. The original prints neither and reads NS.
func TestThermalHTMLMarksCopy(t *testing.T) {
	for _, layout := range []string{ThermalClassic, ThermalModern, ThermalGrid} {
		rec := fixtureReceipt("hospitality")
		rec.Layout = layout

		original := string(RenderHTML(rec, ""))
		if strings.Contains(original, printing.CopyBanner) {
			t.Errorf("%s original: must not print the COPY banner", layout)
		}
		if !strings.Contains(original, "<span>Receipt Type:</span><span>NS</span>") {
			t.Errorf("%s original: missing KRA receipt label NS", layout)
		}

		rec.IsCopy = true
		copyHTML := string(RenderHTML(rec, ""))
		if n := strings.Count(copyHTML, printing.CopyBanner); n != 2 {
			t.Errorf("%s copy: COPY banner printed %d times, want 2 (top and bottom)", layout, n)
		}
		if !strings.Contains(copyHTML, "<span>Receipt Type:</span><span>CS</span>") {
			t.Errorf("%s copy: missing KRA receipt label CS", layout)
		}
		// The copy keeps the original's fiscal data.
		for _, want := range []string{rec.EtimsScuID, rec.EtimsCuInvNo, rec.EtimsQRPNG} {
			if !strings.Contains(copyHTML, want) {
				t.Errorf("%s copy: lost fiscal field %q", layout, want)
			}
		}
	}
}

// The A4 sheet draws a large diagonal COPY watermark plus the banner on a reprint, and neither on
// the original.
func TestA4HTMLMarksCopy(t *testing.T) {
	rec := fixtureReceipt("retail")
	rec.Layout = A4Invoice

	original := string(RenderHTML(rec, ""))
	if strings.Contains(original, `class="copy-wm"`) || strings.Contains(original, printing.CopyBanner) {
		t.Errorf("A4 original: must not carry the COPY watermark or banner")
	}
	if !strings.Contains(original, "<b>Receipt Type:</b> NS") {
		t.Errorf("A4 original: missing KRA receipt label NS")
	}

	rec.IsCopy = true
	copyHTML := string(RenderHTML(rec, ""))
	if !strings.Contains(copyHTML, `<div class="copy-wm">COPY</div>`) {
		t.Errorf("A4 copy: missing the diagonal COPY watermark")
	}
	if !strings.Contains(copyHTML, printing.CopyBanner) {
		t.Errorf("A4 copy: missing the COPY banner")
	}
	if !strings.Contains(copyHTML, "<b>Receipt Type:</b> CS") {
		t.Errorf("A4 copy: missing KRA receipt label CS")
	}
}

// A copy of a refund reads CC; the refund itself reads NC.
func TestRefundCopyLabel(t *testing.T) {
	rec := fixtureReceipt("retail")
	rec.Layout = ThermalModern
	rec.IsReturn = true
	rec.OriginalOrderNumber = "POS-000450"

	if html := string(RenderHTML(rec, "")); !strings.Contains(html, "<span>Receipt Type:</span><span>NC</span>") {
		t.Errorf("refund original: missing KRA receipt label NC")
	}
	rec.IsCopy = true
	if html := string(RenderHTML(rec, "")); !strings.Contains(html, "<span>Receipt Type:</span><span>CC</span>") {
		t.Errorf("refund copy: missing KRA receipt label CC")
	}
}

func TestKRAReceiptLabel(t *testing.T) {
	cases := []struct {
		isCopy, isReturn bool
		want             string
	}{
		{false, false, "NS"},
		{false, true, "NC"},
		{true, false, "CS"},
		{true, true, "CC"},
	}
	for _, c := range cases {
		if got := kraReceiptLabel(Receipt{IsCopy: c.isCopy, IsReturn: c.isReturn}); got != c.want {
			t.Errorf("kraReceiptLabel(copy=%v, return=%v) = %q, want %q", c.isCopy, c.isReturn, got, c.want)
		}
	}
}

// Every PDF layout still renders with the COPY marking (watermark header func, banners).
func TestPDFLayoutsRenderCopy(t *testing.T) {
	rec := fixtureReceipt("retail")
	rec.IsCopy = true
	for _, l := range All() {
		rec.Layout = l.ID
		pdf, err := RenderPDF(rec, Brand{CompanyName: "BOI Enterprises"})
		if err != nil {
			t.Fatalf("layout %s copy PDF render failed: %v", l.ID, err)
		}
		if len(pdf) == 0 {
			t.Errorf("layout %s produced an empty copy PDF", l.ID)
		}
	}
}
