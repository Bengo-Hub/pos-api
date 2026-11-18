package layouts

import (
	"math"
	"strings"
	"testing"

	"github.com/go-pdf/fpdf"

	"github.com/bengobox/pos-service/internal/modules/printing"
)

// A reprint must be marked COPY (KRA copy marking): the thermal HTML prints the banner at the top
// and the bottom, and the KRA receipt label reads CS. The original prints neither and reads NS.
func TestThermalHTMLMarksCopy(t *testing.T) {
	for _, layout := range []string{ThermalClassic, ThermalModern, ThermalGrid} {
		rec := fixtureReceipt("hospitality")
		rec.Layout = layout

		original := string(RenderHTML(rec, ""))
		if strings.Contains(original, printing.CopyBanner(false)) || strings.Contains(original, `class="copy-roll"`) {
			t.Errorf("%s original: must not print the COPY title or mark", layout)
		}
		if !strings.Contains(original, "<span>Receipt Type:</span><span>NS</span>") {
			t.Errorf("%s original: missing KRA receipt label NS", layout)
		}

		rec.IsCopy = true
		copyHTML := string(RenderHTML(rec, ""))
		if n := strings.Count(copyHTML, printing.CopyBanner(false)); n != 2 {
			t.Errorf("%s copy: RECEIPT COPY title printed %d times, want 2 (top and bottom)", layout, n)
		}
		// The COPY mark runs down the roll behind the content.
		if !strings.Contains(copyHTML, `<body class="is-copy">`) || strings.Count(copyHTML, "<span>COPY</span>") < 10 {
			t.Errorf("%s copy: missing the COPY mark down the roll", layout)
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
	if strings.Contains(original, `class="copy-wm"`) || strings.Contains(original, printing.CopyBanner(false)) {
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
	if !strings.Contains(copyHTML, printing.CopyBanner(false)) {
		t.Errorf("A4 copy: missing the RECEIPT COPY title")
	}
	// The mark is sized for the A4 diagonal, not the old 190px word.
	if !strings.Contains(copyHTML, "font-size:95mm") || !strings.Contains(copyHTML, "rgba(51,51,51,.15)") {
		t.Errorf("A4 copy: watermark not at the agreed size and colour")
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
	html := string(RenderHTML(rec, ""))
	if !strings.Contains(html, "<span>Receipt Type:</span><span>CC</span>") {
		t.Errorf("refund copy: missing KRA receipt label CC")
	}
	if !strings.Contains(html, printing.CopyBanner(true)) {
		t.Errorf("refund copy: missing the REFUND RECEIPT COPY title")
	}
}

// The A4 PDF mark spans about three quarters of the page diagonal (KRA review 2026-11-18).
func TestA4CopyWatermarkSize(t *testing.T) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pageW, pageH := pdf.GetPageSize()
	diag := math.Hypot(pageW, pageH)
	size := copyWatermarkSize(pdf, diag*printing.CopyWatermarkDiagonalShare)
	pdf.SetFont("Helvetica", "B", size)
	share := pdf.GetStringWidth(printing.CopyWatermark) / diag
	if share < 0.5 || share > 0.8 {
		t.Errorf("COPY spans %.0f%% of the A4 diagonal, want 50 to 80", share*100)
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
