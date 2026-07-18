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
		},
		Subtotal: 86, TaxAmount: 0, TotalAmount: 86, AmountPaid: 86,
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
		BarcodePNG:     "data:image/png;base64,iVBORw0KGgo=",
		ReceiptFooter:  "Thank you for shopping with us",
	}
}

// Every layout must render the KRA TIMS fiscal block when the sale is fiscalised.
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
		pdf, err := RenderPDF(rec, Brand{})
		if err != nil {
			t.Fatalf("layout %s PDF: %v", l.ID, err)
		}
		if len(pdf) == 0 || !strings.HasPrefix(string(pdf[:5]), "%PDF-") {
			t.Errorf("layout %s PDF: not a PDF", l.ID)
		}
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
		{"bogus", "retail", ThermalModern},
	}
	for _, c := range cases {
		if got := Resolve(c.setting, c.useCase); got != c.want {
			t.Errorf("Resolve(%q,%q) = %q, want %q", c.setting, c.useCase, got, c.want)
		}
	}
	if !Valid("") || !Valid("auto") || !Valid(ThermalModern) || Valid("bogus") {
		t.Error("Valid() acceptance set wrong")
	}
}

// Thermal variants: classic renders monospace, modern renders the sans stack; retail
// thermal receipts carry the barcode + payment-date labelled method.
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
	}
}
