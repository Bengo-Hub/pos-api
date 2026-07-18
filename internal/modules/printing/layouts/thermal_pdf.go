package layouts

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/bengobox/pos-service/internal/modules/printing"
)

// thermalPDFFont maps the thermal layout variant to an fpdf core font family.
// Classic = Courier (monospace, the classic POS look); Modern = Helvetica.
func thermalPDFFont(layout string) string {
	if layout == ThermalClassic {
		return "Courier"
	}
	return "Helvetica"
}

// renderThermalPDF renders the receipt-roll layout as a real PDF on an 80mm-wide strip,
// applying tenant branding (logo). Mirrors renderThermalHTML.
func renderThermalPDF(rec Receipt, brand Brand, layout string) ([]byte, error) {
	const pageW = 80.0 // 80mm thermal paper
	const margin = 4.0
	contentW := pageW - 2*margin
	font := thermalPDFFont(layout)

	pdf := fpdf.NewCustom(&fpdf.InitType{
		UnitStr: "mm",
		Size:    fpdf.SizeType{Wd: pageW, Ht: 297},
	})
	pdf.SetCompression(true) // zlib/FlateDecode the content streams
	pdf.SetMargins(margin, 6, margin)
	pdf.SetAutoPageBreak(true, 6)
	pdf.AddPage()

	currency := rec.Currency
	if currency == "" {
		currency = "KES"
	}
	moneyLine := func(v float64) string { return fmt.Sprintf("%s %0.2f", currency, v) }

	center := func(s string, style string, size float64) {
		pdf.SetFont(font, style, size)
		pdf.MultiCell(contentW, size*0.5+1.0, s, "", "C", false)
	}
	line := func(label, value string, style string, size float64) {
		pdf.SetFont(font, style, size)
		half := contentW / 2
		pdf.CellFormat(half, size*0.5+1.0, label, "", 0, "L", false, 0, "")
		pdf.CellFormat(half, size*0.5+1.0, value, "", 1, "R", false, 0, "")
	}
	hr := func() {
		y := pdf.GetY() + 0.5
		pdf.SetLineWidth(0.2)
		pdf.Line(margin, y, pageW-margin, y)
		pdf.SetY(y + 1.2)
	}

	// Header — tenant logo (centered, honours the "show logo" receipt setting) + company name
	if logo, lt := FetchLogo(brand.LogoURL); logo != nil && rec.ShowLogo {
		const logoW = 22.0
		// Guard against a mislabeled/unsupported logo poisoning the whole receipt: only draw
		// it if fpdf registered it cleanly, otherwise clear the error and skip the logo.
		if info := pdf.RegisterImageOptionsReader("brandlogo", fpdf.ImageOptions{ImageType: lt}, bytes.NewReader(logo)); info != nil && info.Width() > 0 {
			pdf.ImageOptions("brandlogo", (pageW-logoW)/2, pdf.GetY(), logoW, 0, true, fpdf.ImageOptions{ImageType: lt}, 0, "")
			pdf.Ln(1)
		} else {
			pdf.ClearError()
		}
	}
	if brand.CompanyName != "" {
		// Keep the company name black, not the brand colour — POS receipts print on thermal/non-colour
		// printers where coloured text comes out faint.
		center(brand.CompanyName, "B", 12)
	}
	if rec.ReceiptHeader != "" {
		center(rec.ReceiptHeader, "B", 9)
	}
	if rec.OutletName != "" && rec.OutletName != brand.CompanyName {
		center(rec.OutletName, "B", 11)
	}
	if rec.OutletAddress != "" {
		center(rec.OutletAddress, "", 9)
	}
	if rec.OutletPhones != "" {
		center("Mobile: "+rec.OutletPhones, "", 9)
	}
	if rec.EtimsKraPin != "" {
		center("KRA PIN: "+rec.EtimsKraPin, "B", 9)
	}
	pdf.Ln(1)
	hr()

	// Meta
	line("Receipt:", rec.ReceiptNumber, "", 9)
	line("Order:", rec.OrderNumber, "", 9)
	line("Date:", shortDateTime(rec.IssuedAt, rec.Timezone), "", 9)
	if rec.BillTo != "" {
		custLabel := rec.BillToLabel
		if custLabel == "" {
			custLabel = "Customer"
		}
		line(custLabel+":", rec.BillTo, "", 9)
	}
	if rec.ServedBy != "" {
		line("Served by:", rec.ServedBy, "", 9)
	}
	hr()

	// Line items
	pdf.SetFont(font, "B", 9)
	pdf.CellFormat(contentW*0.5, 4, "Item", "", 0, "L", false, 0, "")
	pdf.CellFormat(contentW*0.2, 4, "Qty", "", 0, "R", false, 0, "")
	pdf.CellFormat(contentW*0.3, 4, "Total", "", 1, "R", false, 0, "")
	for _, l := range rec.Lines {
		pdf.SetFont(font, "", 9)
		name := l.Name
		if name == "" {
			name = l.SKU
		}
		total := fmt.Sprintf("%0.2f", l.TotalPrice)
		if l.TotalPrice == 0 {
			total = "FREE"
		}
		pdf.CellFormat(contentW*0.5, 4, truncate(name, 24), "", 0, "L", false, 0, "")
		pdf.CellFormat(contentW*0.2, 4, fmt.Sprintf("%.0f", l.Quantity), "", 0, "R", false, 0, "")
		pdf.CellFormat(contentW*0.3, 4, total, "", 1, "R", false, 0, "")
	}
	hr()

	// Totals
	line("Subtotal", moneyLine(rec.Subtotal), "", 9)
	if rec.DiscountAmount > 0 {
		line("Discount", "-"+moneyLine(rec.DiscountAmount), "", 9)
	}
	if rec.VatEnabled || rec.TaxAmount > 0 {
		line(taxLabel(rec), moneyLine(rec.TaxAmount), "", 9)
	}
	if rec.ChargesTotal > 0 {
		line("Charges", moneyLine(rec.ChargesTotal), "", 9)
	}
	if rec.RoundOff > 0 {
		line("Round Off", moneyLine(rec.RoundOff), "", 9)
	}
	line("TOTAL", moneyLine(rec.TotalAmount), "B", 12)
	hr()

	// Payment
	if rec.PaymentMethod != "" {
		label := strings.ReplaceAll(rec.PaymentMethod, "_", " ")
		if rec.UseCase == "retail" {
			if pl := paymentMethodLabel(rec); pl != "" {
				label = pl
			}
		}
		line("Paid via", label, "", 9)
	}
	if rec.AmountPaid > 0 {
		line("Amount paid", moneyLine(rec.AmountPaid), "", 9)
	}
	if rec.AmountTendered > 0 && !(rec.UseCase == "retail" && rec.AmountTendered == rec.AmountPaid) {
		line("Tendered", moneyLine(rec.AmountTendered), "", 9)
	}
	if rec.ChangeDue > 0 {
		line("Change", moneyLine(rec.ChangeDue), "", 9)
	}
	if rec.BalanceDue > 0.004 || rec.BalanceDue < -0.004 {
		line("Balance Due", moneyLine(rec.BalanceDue), "", 9)
	}

	// KRA TIMS Details (fiscalised sales) — mirrors the paper ETR layout.
	if rec.EtimsInvoiceNumber != "" || rec.EtimsCuInvNo != "" {
		hr()
		center("KRA TIMS Details", "B", 9)
		if rec.EtimsScuID != "" {
			line("SCU ID", rec.EtimsScuID, "", 8)
		}
		if rec.EtimsCuInvNo != "" {
			line("CU Inv No.", rec.EtimsCuInvNo, "", 8)
		} else if rec.EtimsInvoiceNumber != "" {
			line("eTIMS Inv", rec.EtimsInvoiceNumber, "", 8)
		}
		if rec.EtimsRcptSign != "" {
			line("Sign", truncate(rec.EtimsRcptSign, 34), "", 8)
		}
		if rec.EtimsQRCodeURL != "" {
			if qrPNG, qerr := qrcode.Encode(rec.EtimsQRCodeURL, qrcode.Medium, 256); qerr == nil {
				const qrW = 18.0
				if info := pdf.RegisterImageOptionsReader("etimsqr", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(qrPNG)); info != nil && info.Width() > 0 {
					pdf.ImageOptions("etimsqr", (pageW-qrW)/2, pdf.GetY()+1, qrW, qrW, true, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
				} else {
					pdf.ClearError()
				}
			}
		}
	}

	// Payment display ("HOW TO PAY") — M-Pesa/bank details, same block as the HTML/ESC-POS receipts.
	if pm := rec.PaymentMethods; pm != nil {
		hr()
		center("HOW TO PAY", "B", 9)
		if pm.MpesaPaybill != "" {
			line("M-PESA Paybill", pm.MpesaPaybill, "", 9)
		}
		if pm.MpesaAccountRef != "" {
			line("Account No.", pm.MpesaAccountRef, "", 9)
		}
		if pm.MpesaTill != "" {
			line("M-PESA Till", pm.MpesaTill, "", 9)
		}
		if pm.MpesaPochi != "" {
			line("M-PESA Pochi", pm.MpesaPochi, "", 9)
		}
		if pm.BankName != "" || pm.BankAccountNumber != "" {
			label := pm.BankName
			if label == "" {
				label = "Bank"
			}
			line(label, pm.BankAccountNumber, "", 9)
		}
		if pm.BankAccountName != "" {
			center(pm.BankAccountName, "", 8)
		}
	}

	// Code 128 barcode of the order number (retail) — scannable for returns/lookups.
	if rec.UseCase == "retail" && rec.OrderNumber != "" {
		if bcPNG, err := printing.Code128PNG(rec.OrderNumber, 400, 70); err == nil {
			const bcW, bcH = 48.0, 10.0
			pdf.Ln(2)
			if info := pdf.RegisterImageOptionsReader("orderbarcode", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(bcPNG)); info != nil && info.Width() > 0 {
				pdf.ImageOptions("orderbarcode", (pageW-bcW)/2, pdf.GetY(), bcW, bcH, true, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
				pdf.SetFont(font, "", 9)
				pdf.CellFormat(contentW, 4.5, rec.OrderNumber, "", 1, "C", false, 0, "")
			} else {
				pdf.ClearError()
			}
		}
	}

	// Footer
	if rec.ReceiptFooter != "" {
		pdf.Ln(2)
		center(rec.ReceiptFooter, "I", 8)
	}

	// Platform-owner (Codevertex) advertisement — always shown.
	// Never below ~8pt — sub-7pt text disappears on low-quality/low-toner printers.
	lead, contact := providerFooter(rec)
	pdf.Ln(1)
	hr()
	center(lead, "B", 8.2)
	center(contact, "", 7.6)

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("render thermal receipt pdf: %w", err)
	}
	return buf.Bytes(), nil
}
