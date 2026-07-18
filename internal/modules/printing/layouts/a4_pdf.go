package layouts

import (
	"bytes"
	"fmt"
	"math"
	"strings"

	"github.com/go-pdf/fpdf"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/bengobox/pos-service/internal/modules/printing"
)

// renderA4PDF renders the boxed invoice-style receipt as a real A4 PDF (fpdf) —
// same design as renderA4HTML.
func renderA4PDF(rec Receipt, brand Brand) ([]byte, error) {
	const pageW = 210.0
	const margin = 12.0
	contentW := pageW - 2*margin

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetCompression(true)
	pdf.SetMargins(margin, margin, margin)
	pdf.SetAutoPageBreak(true, 12)
	pdf.AddPage()

	// ── Boxed business header ──
	headTop := pdf.GetY()
	pdf.SetX(margin)
	if logo, lt := FetchLogo(brand.LogoURL); logo != nil && rec.ShowLogo {
		const logoW = 26.0
		if info := pdf.RegisterImageOptionsReader("brandlogo", fpdf.ImageOptions{ImageType: lt}, bytes.NewReader(logo)); info != nil && info.Width() > 0 {
			pdf.ImageOptions("brandlogo", (pageW-logoW)/2, pdf.GetY()+2, logoW, 0, true, fpdf.ImageOptions{ImageType: lt}, 0, "")
			pdf.Ln(2)
		} else {
			pdf.ClearError()
		}
	}
	name := rec.OutletName
	if name == "" {
		name = "RECEIPT"
	}
	pdf.SetFont("Helvetica", "B", 18)
	pdf.MultiCell(contentW, 8, strings.ToUpper(name), "", "C", false)
	if rec.OutletAddress != "" {
		pdf.SetFont("Helvetica", "", 10)
		pdf.MultiCell(contentW, 5, strings.ToUpper(rec.OutletAddress), "", "C", false)
	}
	if rec.OutletPhones != "" {
		pdf.SetFont("Helvetica", "B", 10)
		pdf.MultiCell(contentW, 5, "Mobile: "+rec.OutletPhones, "", "C", false)
	}
	if rec.EtimsKraPin != "" {
		pdf.SetFont("Helvetica", "B", 10)
		pdf.MultiCell(contentW, 5, "KRA PIN: "+rec.EtimsKraPin, "", "C", false)
	}
	if rec.ReceiptHeader != "" {
		pdf.SetFont("Helvetica", "B", 10)
		pdf.MultiCell(contentW, 5, rec.ReceiptHeader, "", "C", false)
	}
	pdf.Ln(1)
	pdf.SetLineWidth(0.4)
	pdf.Rect(margin, headTop, contentW, pdf.GetY()-headTop, "D")
	pdf.Ln(3)

	// ── Customer | INVOICE.NO | DATE table ──
	billLabel := rec.BillToLabel
	if billLabel == "" {
		billLabel = "Customer"
	}
	custW, invW, dateW := contentW*0.45, contentW*0.22, contentW*0.33
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(custW, 7, billLabel, "1", 0, "L", false, 0, "")
	pdf.CellFormat(invW, 7, "INVOICE.NO", "1", 0, "C", false, 0, "")
	pdf.CellFormat(dateW, 7, "DATE", "1", 1, "C", false, 0, "")
	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(custW, 8, strings.ToUpper(truncate(rec.BillTo, 38)), "1", 0, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 10)
	pdf.CellFormat(invW, 8, rec.OrderNumber, "1", 0, "C", false, 0, "")
	pdf.CellFormat(dateW, 8, shortDateTime(rec.IssuedAt, rec.Timezone), "1", 1, "C", false, 0, "")
	pdf.Ln(2)

	// ── SERVED BY ──
	if rec.ServedBy != "" {
		pdf.SetFont("Helvetica", "B", 10)
		pdf.CellFormat(contentW/2, 6, "SERVED BY", "B", 0, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 10)
		pdf.CellFormat(contentW/2, 6, rec.ServedBy, "B", 1, "R", false, 0, "")
		pdf.Ln(2)
	}

	// ── Items table ──
	itemW, qtyW, priceW, subW := contentW*0.48, contentW*0.14, contentW*0.18, contentW*0.20
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(itemW, 7, "Item Name", "1", 0, "L", false, 0, "")
	pdf.CellFormat(qtyW, 7, "Qty", "1", 0, "C", false, 0, "")
	pdf.CellFormat(priceW, 7, "Price", "1", 0, "R", false, 0, "")
	pdf.CellFormat(subW, 7, "Subtotal", "1", 1, "R", false, 0, "")
	pdf.SetFont("Helvetica", "", 10)
	var totalQty float64
	for _, l := range rec.Lines {
		totalQty += l.Quantity
		lineName := l.Name
		if lineName == "" {
			lineName = l.SKU
		}
		price := amount(l.UnitPrice)
		total := amount(l.TotalPrice)
		if l.TotalPrice == 0 {
			price, total = "FREE", "FREE"
		}
		pdf.CellFormat(itemW, 7, strings.ToUpper(truncate(lineName, 42)), "1", 0, "L", false, 0, "")
		pdf.CellFormat(qtyW, 7, fmt.Sprintf("%g", l.Quantity), "1", 0, "C", false, 0, "")
		pdf.CellFormat(priceW, 7, price, "1", 0, "R", false, 0, "")
		pdf.CellFormat(subW, 7, total, "1", 1, "R", false, 0, "")
	}
	pdf.Ln(2)

	// ── Totals block ──
	trow := func(label, value string, bold bool) {
		style := ""
		if bold {
			style = "B"
		}
		pdf.SetFont("Helvetica", style, 10.5)
		pdf.CellFormat(contentW*0.6, 5.5, label, "", 0, "L", false, 0, "")
		pdf.CellFormat(contentW*0.4, 5.5, value, "", 1, "R", false, 0, "")
	}
	trow("Total Quantity", fmt.Sprintf("%g", totalQty), false)
	trow("TOTAL ITEMS", fmt.Sprintf("%d", len(rec.Lines)), false)
	trow("Subtotal:", money(rec.Currency, rec.Subtotal), true)
	if rec.DiscountAmount > 0 {
		trow("Discount(-):", "-"+money(rec.Currency, rec.DiscountAmount), false)
	}
	if rec.VatEnabled || rec.TaxAmount > 0 {
		tl := "Tax(+):"
		if rec.VatRate > 0 {
			tl = fmt.Sprintf("VAT %g%%(+):", rec.VatRate)
		}
		trow(tl, money(rec.Currency, rec.TaxAmount), false)
	}
	for _, cr := range chargeRows(rec) {
		trow(cr[0].(string)+":", money(rec.Currency, cr[1].(float64)), false)
	}
	if rec.RoundOff > 0 {
		trow("Round Off:", money(rec.Currency, rec.RoundOff), false)
	}
	trow("TOTAL:", money(rec.Currency, rec.TotalAmount), true)
	if pl := paymentMethodLabel(rec); pl != "" && rec.AmountPaid > 0 {
		trow(pl, money(rec.Currency, rec.AmountPaid), false)
	}
	trow("AMOUNT PAID", money(rec.Currency, rec.AmountPaid), false)
	if rec.ChangeDue > 0 {
		trow("Change", money(rec.Currency, rec.ChangeDue), false)
	}
	if math.Abs(rec.BalanceDue) >= 0.005 {
		trow("Total Due with Current", money(rec.Currency, rec.BalanceDue), false)
	}

	// ── KRA TIMS Details, adapted from the KRA-issued paper ETR receipt (see the Jazaribu
	// Retail reference): SCU ID + CU Inv No, then the verification QR, then — right after,
	// no other content between — the fiscal barcode below. The receipt SIGNATURE is
	// deliberately never printed as plain text (it's already encoded in the QR). ──
	if rec.EtimsCuInvNo != "" || rec.EtimsInvoiceNumber != "" || rec.EtimsQRCodeURL != "" {
		pdf.Ln(4)
		pdf.SetFont("Helvetica", "B", 12)
		pdf.CellFormat(contentW, 6, "KRA TIMS Details", "", 1, "C", false, 0, "")
		pdf.SetFont("Helvetica", "", 10)
		if rec.EtimsScuID != "" {
			pdf.CellFormat(contentW, 5, "SCU ID: "+rec.EtimsScuID, "", 1, "C", false, 0, "")
		}
		if rec.EtimsCuInvNo != "" {
			pdf.CellFormat(contentW, 5, "CU_Inv No.: "+rec.EtimsCuInvNo, "", 1, "C", false, 0, "")
		} else if rec.EtimsInvoiceNumber != "" {
			pdf.CellFormat(contentW, 5, "CU No.: "+rec.EtimsInvoiceNumber, "", 1, "C", false, 0, "")
		}
		if rec.EtimsQRCodeURL != "" {
			if qrPNG, qerr := qrcode.Encode(rec.EtimsQRCodeURL, qrcode.Medium, 256); qerr == nil {
				const qrW = 28.0
				if info := pdf.RegisterImageOptionsReader("etimsqr", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(qrPNG)); info != nil && info.Width() > 0 {
					pdf.ImageOptions("etimsqr", (pageW-qrW)/2, pdf.GetY()+1, qrW, qrW, true, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
					pdf.SetY(pdf.GetY() + qrW + 1)
				} else {
					pdf.ClearError()
				}
			}
		}
	}

	// ── Fiscal barcode: encodes rec.BarcodeValue (the eTIMS CU Invoice Number once
	// fiscalised, else the order number for non-fiscalised retail — ReceiptView.
	// FiscalBarcodeValue, the ONE place this decision is made). ──
	if rec.BarcodeValue != "" {
		if bcPNG, err := printing.Code128PNG(rec.BarcodeValue, 400, 70); err == nil {
			const bcW, bcH = 55.0, 12.0
			pdf.Ln(4)
			if info := pdf.RegisterImageOptionsReader("orderbarcode", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(bcPNG)); info != nil && info.Width() > 0 {
				pdf.ImageOptions("orderbarcode", (pageW-bcW)/2, pdf.GetY(), bcW, bcH, true, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
				pdf.SetFont("Helvetica", "", 10)
				pdf.CellFormat(contentW, 5, rec.BarcodeValue, "", 1, "C", false, 0, "")
			} else {
				pdf.ClearError()
			}
		}
	}

	// ── Configurable footer text (flows below the barcode) ──
	footer := rec.ReceiptFooter
	if footer == "" {
		footer = "Thank you for your business!"
	}
	pdf.Ln(4)
	pdf.SetFont("Helvetica", "", 11)
	pdf.MultiCell(contentW, 5.5, strings.ToUpper(footer), "", "L", false)

	// ── Provider advertisement — smaller print, but never below ~8pt (sub-7pt text
	// disappears entirely on low-quality office printers) ──
	lead, contact := providerFooter(rec)
	pdf.Ln(3)
	pdf.SetFont("Helvetica", "B", 8.2)
	pdf.MultiCell(contentW, 4, lead, "", "C", false)
	pdf.SetFont("Helvetica", "", 7.6)
	pdf.MultiCell(contentW, 3.8, contact, "", "C", false)

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("render a4 receipt pdf: %w", err)
	}
	return buf.Bytes(), nil
}
