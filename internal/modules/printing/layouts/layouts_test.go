package layouts

import (
	"strings"
	"testing"
	"time"
)

func fixtureReceipt(useCase string) Receipt {
	paid := time.Date(2026, 7, 18, 20, 11, 0, 0, time.UTC)
	return Receipt{
		ReceiptNumber: "RCT-POS-TEST01",
		OrderNumber:   "POS-TEST01",
		OutletName:    "Demo City Supermarket",
		OutletAddress: "Kinoo 87",
		OutletPhones:  "SAF +254740002000",
		BillTo:        "Walk-in customer",
		BillToLabel:   "Customer",
		IssuedAt:      paid,
		Timezone:      "Africa/Nairobi",
		Lines: []Line{
			{SKU: "DWL750", Name: "Dish Washing Liquid 750ml", Quantity: 1, UnitPrice: 86, TotalPrice: 86},
			{SKU: "FREE1", Name: "Carrier Bag", Quantity: 1, UnitPrice: 0, TotalPrice: 0},
			// Multi-quantity line — asserts the qty × unit-price sub-line renders on every
			// flex/line-based layout (classic/modern/a4) the same way the pos-ui client does.
			{SKU: "BW500", Name: "Body Wash 500ml", Quantity: 2, UnitPrice: 400, TotalPrice: 800},
		},
		Subtotal: 886, TaxAmount: 0, TotalAmount: 886, AmountPaid: 886,
		Currency: "KES", PaymentMethod: "cash", PaymentDate: &paid,
		VatEnabled: false, PaperWidth: "80mm", ShowLogo: true,
		UseCase: useCase,
		// eTIMS fiscal identity (the KRA TIMS Details block).
		EtimsKraPin:   "P052257611W",
		EtimsScuID:    "KRACU0300003541",
		EtimsCuInvNo:  "KRACU0300003541/340368",
		EtimsRcptSign: "ABC1DEF2GHI3JKL4",
		EtimsQRCodeURL: "https://etims-sbx.kra.go.ke/common/link/etims/receipt/indexEtimsReceiptData?Data=P052257611W00ABC1DEF2GHI3JKL4",
		EtimsQRPNG:     "data:image/png;base64,iVBORw0KGgo=",
		// Mirrors what handlers.newReceiptResponse computes via ReceiptView.FiscalBarcodeValue:
		// once fiscalised, the barcode encodes the CU Invoice Number, never the order number.
		BarcodePNG:    "data:image/png;base64,iVBORw0KGgo=",
		BarcodeValue:  "KRACU0300003541/340368",
		ReceiptFooter: "Thank you for shopping with us",
	}
}

// Every layout must render the KRA TIMS fiscal block when the sale is fiscalised, the KRA
// PIN in the header (not buried in the fiscal block), and the fiscal barcode (CU Invoice
// Number, NOT the order number) — but must NEVER print the receipt signature in the clear.
func TestAllLayoutsRenderEtimsBlock(t *testing.T) {
	rec := fixtureReceipt("retail")
	for _, l := range All() {
		rec.Layout = l.ID
		html := string(RenderHTML(rec, ""))
		for _, want := range []string{"KRA TIMS Details", "KRACU0300003541", "KRACU0300003541/340368", "P052257611W"} {
			if !strings.Contains(html, want) {
				t.Errorf("layout %s HTML missing fiscal detail %q", l.ID, want)
			}
		}
		if strings.Contains(html, "ABC1DEF2GHI3JKL4") {
			t.Errorf("layout %s HTML must NOT print the raw receipt signature", l.ID)
		}
		// The barcode caption must be the fiscal CU Inv No — never the internal order number
		// (OrderNumber legitimately appears elsewhere, e.g. the a4_invoice INVOICE.NO cell, so
		// check the barcode caption class specifically, not a bare substring of the page).
		if !strings.Contains(html, `class="num">`+rec.BarcodeValue+`<`) {
			t.Errorf("layout %s HTML barcode caption must be the fiscal CU Inv No %q", l.ID, rec.BarcodeValue)
		}
		if strings.Contains(html, `class="num">`+rec.OrderNumber+`<`) {
			t.Errorf("layout %s HTML barcode caption must not be the order number", l.ID)
		}
		pdf, err := RenderPDF(rec, Brand{})
		if err != nil {
			t.Fatalf("layout %s PDF: %v", l.ID, err)
		}
		if len(pdf) == 0 || !strings.HasPrefix(string(pdf[:5]), "%PDF-") {
			t.Errorf("layout %s PDF: not a PDF", l.ID)
		}
	}
}

// The QR must always come from the server-generated PNG data: URI — never the raw KRA
// verification URL used directly as an <img src> (a URL is not image bytes; this was the
// broken-QR-icon bug). And the fiscal barcode must encode the CU Invoice Number once
// fiscalised, appearing immediately after the fiscal block (ETR-style adjacency).
func TestFiscalQRAndBarcodeAdjacency(t *testing.T) {
	rec := fixtureReceipt("retail")
	for _, id := range []string{ThermalClassic, ThermalModern, ThermalGrid, A4Invoice} {
		rec.Layout = id
		html := string(RenderHTML(rec, ""))
		if strings.Contains(html, `src="https://etims-sbx.kra.go.ke`) {
			t.Errorf("layout %s HTML must not use the verification URL as an <img src>", id)
		}
		if !strings.Contains(html, `src="data:image/png;base64,iVBORw0KGgo="`) {
			t.Errorf("layout %s HTML must render the QR from the data: URI", id)
		}
		fiscalIdx := strings.Index(html, "KRA TIMS Details")
		barcodeIdx := strings.Index(html, `class="barcode"`)
		if fiscalIdx == -1 || barcodeIdx == -1 || barcodeIdx < fiscalIdx {
			t.Errorf("layout %s: fiscal block must precede the barcode block", id)
		}
	}
}

// A non-fiscalised retail sale falls back to the order number as the barcode value (no
// eTIMS block at all); a fiscalised sale in ANY use case gets the fiscal barcode.
func TestFiscalBarcodeValueFallback(t *testing.T) {
	rec := fixtureReceipt("hospitality")
	rec.EtimsKraPin, rec.EtimsScuID, rec.EtimsCuInvNo = "", "", ""
	rec.EtimsInvoiceNumber, rec.EtimsRcptSign, rec.EtimsQRCodeURL, rec.EtimsQRPNG = "", "", "", ""
	// A real receipt.go response only ever sets these when FiscalBarcodeValue() is non-empty
	// (a non-fiscalised, non-retail sale never gets a barcode) — mirror that here.
	rec.BarcodePNG, rec.BarcodeValue = "", ""
	rec.Layout = ThermalClassic
	html := string(RenderHTML(rec, ""))
	if strings.Contains(html, "KRA TIMS Details") {
		t.Error("non-fiscalised sale must not render the fiscal block")
	}
	if strings.Contains(html, `class="barcode"`) {
		t.Error("non-fiscalised, non-retail sale must not render a barcode")
	}
}

// The print pipeline fix: every HTML layout must use a zero @page margin (this is what
// suppresses the browser's own about:blank/date/URL header chrome) with inner padding.
func TestHTMLLayoutsUseZeroPageMargin(t *testing.T) {
	rec := fixtureReceipt("retail")
	for _, l := range All() {
		rec.Layout = l.ID
		html := string(RenderHTML(rec, ""))
		if !strings.Contains(html, "margin:0}") || !strings.Contains(html, "@page{") {
			t.Errorf("layout %s HTML must declare @page{...margin:0}", l.ID)
		}
	}
}

// Layout resolution: auto → thermal for everyone (modern for retail, classic otherwise);
// explicit settings win; unknown values fall back to auto behaviour.
func TestResolve(t *testing.T) {
	cases := []struct{ setting, useCase, want string }{
		{"", "retail", ThermalModern},
		{"auto", "retail", ThermalModern},
		{"", "hospitality", ThermalClassic},
		{"auto", "", ThermalClassic},
		{A4Invoice, "retail", A4Invoice},
		{ThermalClassic, "retail", ThermalClassic},
		{ThermalModern, "hospitality", ThermalModern},
		{ThermalGrid, "retail", ThermalGrid},
		{ThermalGrid, "hospitality", ThermalGrid},
		{"bogus", "retail", ThermalModern},
	}
	for _, c := range cases {
		if got := Resolve(c.setting, c.useCase); got != c.want {
			t.Errorf("Resolve(%q,%q) = %q, want %q", c.setting, c.useCase, got, c.want)
		}
	}
	if !Valid("") || !Valid("auto") || !Valid(ThermalModern) || !Valid(ThermalGrid) || Valid("bogus") {
		t.Error("Valid() acceptance set wrong")
	}
	// thermal_grid is opt-in ONLY — auto must never resolve to it regardless of use case.
	for _, uc := range []string{"retail", "hospitality", "pharmacy", ""} {
		if got := Resolve("auto", uc); got == ThermalGrid {
			t.Errorf("Resolve(auto,%q) must never pick thermal_grid (opt-in only), got %q", uc, got)
		}
	}
}

// Thermal variants: classic renders monospace, modern renders the sans stack; retail
// thermal receipts carry the barcode + payment-date labelled method + the qty × unit-price
// sub-line for any line with quantity != 1 (parity with the pos-ui client renderer).
func TestThermalVariants(t *testing.T) {
	rec := fixtureReceipt("retail")
	rec.Layout = ThermalClassic
	classic := string(RenderHTML(rec, ""))
	if !strings.Contains(classic, "Courier New") {
		t.Error("thermal_classic must use the Courier stack")
	}
	rec.Layout = ThermalModern
	modern := string(RenderHTML(rec, ""))
	if !strings.Contains(modern, "Helvetica Neue") {
		t.Error("thermal_modern must use the Helvetica stack")
	}
	for _, html := range []string{classic, modern} {
		if !strings.Contains(html, `class="barcode"`) {
			t.Error("retail thermal receipt must render the order barcode")
		}
		if !strings.Contains(html, "Cash (18-07-2026)") {
			t.Error("retail thermal receipt must render the payment method with settle date")
		}
		if !strings.Contains(html, "FREE") {
			t.Error("zero-charge line must print FREE")
		}
		// qty × unit-price sub-line for the 2-quantity Body Wash line.
		if !strings.Contains(html, `class="line-sub">2 &times; 400`) {
			t.Error("a line with quantity != 1 must render the qty x unit-price sub-line")
		}
	}
}

// The opt-in bordered-grid layout: uses the sans font (like modern), renders the
// customer/receipt-no and item list as real bordered <table>s, and — since Qty/Price are
// already separate table columns — does NOT render the flex-layout qty-subline.
func TestThermalGridLayout(t *testing.T) {
	rec := fixtureReceipt("retail")
	rec.Layout = ThermalGrid
	html := string(RenderHTML(rec, ""))
	if !strings.Contains(html, "Helvetica Neue") {
		t.Error("thermal_grid must use the Helvetica stack")
	}
	if !strings.Contains(html, `class="gtable"`) {
		t.Error("thermal_grid must render bordered <table class=\"gtable\"> elements")
	}
	if strings.Contains(html, `class="line-sub"`) {
		t.Error("thermal_grid has Qty/Price as table columns — it must not also render the flex qty-subline")
	}
	if !strings.Contains(html, "RECEIPT NO") {
		t.Error("thermal_grid must render the bordered Customer|Receipt No meta table")
	}
	pdf, err := RenderPDF(rec, Brand{})
	if err != nil {
		t.Fatalf("thermal_grid PDF: %v", err)
	}
	if len(pdf) == 0 || !strings.HasPrefix(string(pdf[:5]), "%PDF-") {
		t.Error("thermal_grid PDF: not a PDF")
	}
}

// Content must not be silently clipped: long item/payment names wrap instead of being
// ellipsis-truncated on the HTML renderers (a full fix, unlike the fixed-width PDF cells).
func TestLongContentWrapsNotClips(t *testing.T) {
	rec := fixtureReceipt("retail")
	rec.Lines = append(rec.Lines, Line{
		SKU: "LONG1", Name: "A Genuinely Very Long Product Name That Would Have Been Ellipsis-Truncated Before",
		Quantity: 1, UnitPrice: 10, TotalPrice: 10,
	})
	for _, id := range []string{ThermalClassic, ThermalModern, ThermalGrid} {
		rec.Layout = id
		html := string(RenderHTML(rec, ""))
		if !strings.Contains(html, "Ellipsis-Truncated Before") {
			t.Errorf("layout %s must not clip a long item name in its own HTML output", id)
		}
	}
}
