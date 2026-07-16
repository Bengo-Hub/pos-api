package handlers

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/bengobox/pos-service/internal/modules/printing"
)

// Retail receipt template (BOI/GoDigital design): boxed business header, a
// Customer | INVOICE.NO | DATE table, SERVED BY line, a bordered items table
// (Item Name/Qty/Price/Subtotal), a totals block with quantity/item counts, the
// itemised discount/tax/charges breakdown, the payment method with its date
// ("Cash (14-07-2026)"), AMOUNT PAID and the balance due, a Code 128 barcode of the
// invoice/order number, the configurable footer text (Receipt & Printing settings —
// e.g. "IN GOD WE TRUST") flowing right below the barcode, and the provider
// advertisement in a deliberately smaller print.
//
// Selected by GetReceipt when the outlet's use_case is "retail"; other use cases keep
// the classic thermal layout (generateReceiptHTML / generateReceiptPDF).

// retailMoney renders "KSh 16,200" / "KSh -1,000" style amounts: thousands separators,
// decimals only when the amount isn't whole.
func retailMoney(currency string, v float64) string {
	unit := currency
	if unit == "" || strings.EqualFold(unit, "KES") {
		unit = "KSh"
	}
	return unit + " " + retailAmount(v)
}

func retailAmount(v float64) string {
	neg := v < 0
	av := math.Abs(v)
	var s string
	if av == math.Trunc(av) {
		s = groupThousands(fmt.Sprintf("%.0f", av))
	} else {
		s = groupThousands(fmt.Sprintf("%.2f", av))
	}
	if neg {
		return "-" + s
	}
	return s
}

// groupThousands inserts comma separators into the integer part of a plain numeric string.
func groupThousands(s string) string {
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	if len(intPart) <= 3 {
		return intPart + frac
	}
	var b strings.Builder
	lead := len(intPart) % 3
	if lead > 0 {
		b.WriteString(intPart[:lead])
	}
	for i := lead; i < len(intPart); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(intPart[i : i+3])
	}
	return b.String() + frac
}

// retailDate renders dd-mm-yyyy (the sample receipt's date style) in the outlet timezone.
func retailDate(t time.Time, tz string) string {
	return retailTimeIn(t, tz).Format("02-01-2006")
}

func retailDateTime(t time.Time, tz string) string {
	return retailTimeIn(t, tz).Format("02-01-2006 15:04")
}

func retailTimeIn(t time.Time, tz string) time.Time {
	if tz == "" {
		tz = "Africa/Nairobi"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc)
}

// retailChargeRows returns the named charge breakdown as sorted display rows
// ("Shipping(+)", 200), falling back to one aggregate "Charges(+)" row.
func retailChargeRows(rec receiptResponse) [][2]interface{} {
	var rows [][2]interface{}
	if len(rec.Charges) > 0 {
		keys := make([]string, 0, len(rec.Charges))
		for k := range rec.Charges {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			label := strings.ToUpper(k[:1]) + k[1:]
			rows = append(rows, [2]interface{}{label + "(+)", rec.Charges[k]})
		}
		return rows
	}
	if rec.ChargesTotal > 0 {
		rows = append(rows, [2]interface{}{"Charges(+)", rec.ChargesTotal})
	}
	return rows
}

// paymentMethodLabel renders "Cash (14-07-2026)" — the tender name plus the settle date.
func paymentMethodLabel(rec receiptResponse) string {
	m := strings.TrimSpace(strings.ReplaceAll(rec.PaymentMethod, "_", " "))
	if m == "" {
		return ""
	}
	m = strings.ToUpper(m[:1]) + m[1:]
	if rec.PaymentDate != nil {
		return fmt.Sprintf("%s (%s)", m, retailDate(*rec.PaymentDate, rec.Timezone))
	}
	return m
}

// generateRetailReceiptHTML renders the boxed retail receipt as printable HTML (A4 portrait).
func generateRetailReceiptHTML(rec receiptResponse, logoURL string) []byte {
	var totalQty float64
	for _, l := range rec.Lines {
		totalQty += l.Quantity
	}

	var buf bytes.Buffer
	buf.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8">`)
	buf.WriteString(`<title>Receipt ` + htmlEscape(rec.ReceiptNumber) + `</title>`)
	buf.WriteString(`<style>
@page{size:A4 portrait;margin:12mm}
*{box-sizing:border-box}
body{font-family:'Times New Roman',Georgia,serif;font-size:13px;color:#000;background:#fff;max-width:186mm;margin:0 auto;-webkit-print-color-adjust:exact;print-color-adjust:exact}
.box{border:1.5px solid #000;padding:6px 10px;text-align:center}
.biz{font-size:24px;font-weight:bold;letter-spacing:.5px;margin:2px 0}
.sub{font-size:12px;margin:1px 0;white-space:pre-wrap}
.logo{display:block;margin:2px auto;max-height:22mm;max-width:60mm;object-fit:contain}
table{width:100%;border-collapse:collapse;margin-top:6px}
th,td{border:1.5px solid #000;padding:4px 8px;font-size:13px}
th{font-weight:bold;text-align:left}
td.c,th.c{text-align:center}
td.r,th.r{text-align:right}
.served{display:flex;justify-content:space-between;border-bottom:1px solid #000;padding:5px 2px;font-size:12px;margin-top:4px}
.totals{margin-top:8px}
.trow{display:flex;justify-content:space-between;padding:1.5px 2px;font-size:13px}
.trow.b{font-weight:bold}
.barcode{text-align:center;margin:14px 0 4px}
.barcode img{height:14mm}
.barcode .num{font-size:12px;letter-spacing:2px;margin-top:1px}
.ftr{font-size:13px;margin:12px 0 4px;white-space:pre-wrap}
.prov{font-size:8.5px;text-align:center;color:#000;margin-top:10px;line-height:1.4}
@media print{body{max-width:100%}}
</style></head><body>`)

	// ── Boxed business header ──
	buf.WriteString(`<div class="box">`)
	if logoURL != "" && rec.ShowLogo {
		buf.WriteString(fmt.Sprintf(`<img class="logo" src="%s" alt="logo">`, htmlEscape(logoURL)))
	}
	name := rec.OutletName
	if name == "" {
		name = "RECEIPT"
	}
	buf.WriteString(fmt.Sprintf(`<div class="biz">%s</div>`, htmlEscape(strings.ToUpper(name))))
	if rec.OutletAddress != "" {
		buf.WriteString(fmt.Sprintf(`<div class="sub">%s</div>`, htmlEscape(strings.ToUpper(rec.OutletAddress))))
	}
	if rec.OutletPhones != "" {
		buf.WriteString(fmt.Sprintf(`<div class="sub"><b>Mobile:</b> %s</div>`, htmlEscape(rec.OutletPhones)))
	}
	if rec.EtimsKraPin != "" {
		buf.WriteString(fmt.Sprintf(`<div class="sub"><b>KRA PIN:</b> %s</div>`, htmlEscape(rec.EtimsKraPin)))
	}
	if rec.ReceiptHeader != "" {
		buf.WriteString(fmt.Sprintf(`<div class="sub"><b>%s</b></div>`, htmlEscape(rec.ReceiptHeader)))
	}
	buf.WriteString(`</div>`)

	// ── Customer | INVOICE.NO | DATE ──
	billLabel := rec.BillToLabel
	if billLabel == "" {
		billLabel = "Customer"
	}
	buf.WriteString(`<table><tr>`)
	buf.WriteString(fmt.Sprintf(`<th style="width:45%%">%s</th><th class="c" style="width:22%%">INVOICE.NO</th><th class="c">DATE</th>`, htmlEscape(billLabel)))
	buf.WriteString(`</tr><tr>`)
	buf.WriteString(fmt.Sprintf(`<td><b>%s</b></td>`, htmlEscape(strings.ToUpper(rec.BillTo))))
	buf.WriteString(fmt.Sprintf(`<td class="c">%s</td>`, htmlEscape(rec.OrderNumber)))
	buf.WriteString(fmt.Sprintf(`<td class="c">%s</td>`, retailDateTime(rec.IssuedAt, rec.Timezone)))
	buf.WriteString(`</tr></table>`)

	// ── SERVED BY ──
	if rec.ServedBy != "" {
		buf.WriteString(fmt.Sprintf(`<div class="served"><b>SERVED BY</b><span>%s</span></div>`, htmlEscape(rec.ServedBy)))
	}

	// ── Items ──
	buf.WriteString(`<table><tr><th>Item Name</th><th class="c" style="width:14%">Qty</th><th class="r" style="width:18%">Price</th><th class="r" style="width:20%">Subtotal</th></tr>`)
	for _, l := range rec.Lines {
		lineName := l.Name
		if lineName == "" {
			lineName = l.SKU
		}
		price := retailAmount(l.UnitPrice)
		total := retailAmount(l.TotalPrice)
		if l.TotalPrice == 0 {
			price, total = "FREE", "FREE"
		}
		buf.WriteString(fmt.Sprintf(`<tr><td>%s</td><td class="c">%g</td><td class="r">%s</td><td class="r">%s</td></tr>`,
			htmlEscape(strings.ToUpper(lineName)), l.Quantity, price, total))
	}
	buf.WriteString(`</table>`)

	// ── Totals block ──
	trow := func(label, value string, bold bool) {
		cls := "trow"
		if bold {
			cls = "trow b"
		}
		buf.WriteString(fmt.Sprintf(`<div class="%s"><span>%s</span><span>%s</span></div>`, cls, htmlEscape(label), htmlEscape(value)))
	}
	buf.WriteString(`<div class="totals">`)
	trow("Total Quantity", fmt.Sprintf("%g", totalQty), false)
	trow("TOTAL ITEMS", fmt.Sprintf("%d", len(rec.Lines)), false)
	trow("Subtotal:", retailMoney(rec.Currency, rec.Subtotal), true)
	if rec.DiscountAmount > 0 {
		trow("Discount(-):", "-"+retailMoney(rec.Currency, rec.DiscountAmount), false)
	}
	if rec.VatEnabled || rec.TaxAmount > 0 {
		taxLabel := "Tax(+):"
		if rec.VatRate > 0 {
			taxLabel = fmt.Sprintf("VAT %g%%(+):", rec.VatRate)
		}
		trow(taxLabel, retailMoney(rec.Currency, rec.TaxAmount), false)
	}
	for _, cr := range retailChargeRows(rec) {
		trow(cr[0].(string)+":", retailMoney(rec.Currency, cr[1].(float64)), false)
	}
	if rec.RoundOff > 0 {
		trow("Round Off:", retailMoney(rec.Currency, rec.RoundOff), false)
	}
	trow("TOTAL:", retailMoney(rec.Currency, rec.TotalAmount), true)
	if pl := paymentMethodLabel(rec); pl != "" && rec.AmountPaid > 0 {
		trow(pl, retailMoney(rec.Currency, rec.AmountPaid), false)
	}
	trow("AMOUNT PAID", retailMoney(rec.Currency, rec.AmountPaid), false)
	if rec.ChangeDue > 0 {
		trow("Change", retailMoney(rec.Currency, rec.ChangeDue), false)
	}
	if math.Abs(rec.BalanceDue) >= 0.005 {
		trow("Total Due with Current", retailMoney(rec.Currency, rec.BalanceDue), false)
	}
	buf.WriteString(`</div>`)

	// ── KRA TIMS Details (fiscalised sales) — mirrors the paper ETR layout ──
	if rec.EtimsCuInvNo != "" || rec.EtimsInvoiceNumber != "" || rec.EtimsQRPNG != "" {
		buf.WriteString(`<div style="text-align:center;margin-top:10px">`)
		buf.WriteString(`<div style="font-weight:bold;font-size:14px">KRA TIMS Details</div>`)
		if rec.EtimsScuID != "" {
			buf.WriteString(fmt.Sprintf(`<div class="sub"><b>SCU ID:</b> %s</div>`, htmlEscape(rec.EtimsScuID)))
		}
		if rec.EtimsCuInvNo != "" {
			buf.WriteString(fmt.Sprintf(`<div class="sub"><b>CU_Inv No.:</b> %s</div>`, htmlEscape(rec.EtimsCuInvNo)))
		} else if rec.EtimsInvoiceNumber != "" {
			buf.WriteString(fmt.Sprintf(`<div class="sub"><b>CU No.:</b> %s</div>`, htmlEscape(rec.EtimsInvoiceNumber)))
		}
		if rec.EtimsRcptSign != "" {
			buf.WriteString(fmt.Sprintf(`<div class="sub" style="word-break:break-all">Sign: %s</div>`, htmlEscape(rec.EtimsRcptSign)))
		}
		if rec.EtimsQRPNG != "" {
			buf.WriteString(fmt.Sprintf(`<img src="%s" alt="eTIMS QR" style="height:30mm;margin-top:4px">`, rec.EtimsQRPNG))
		}
		buf.WriteString(`</div>`)
	}

	// ── Barcode of the invoice/order number ──
	if rec.BarcodePNG != "" {
		buf.WriteString(fmt.Sprintf(`<div class="barcode"><img src="%s" alt="barcode"><div class="num">%s</div></div>`,
			rec.BarcodePNG, htmlEscape(rec.OrderNumber)))
	}

	// ── Configurable footer text ("IN GOD WE TRUST" position — flows below the barcode) ──
	footer := rec.ReceiptFooter
	if footer == "" {
		footer = "Thank you for your business!"
	}
	buf.WriteString(fmt.Sprintf(`<div class="ftr">%s</div>`, htmlEscape(strings.ToUpper(footer))))

	// ── Provider advertisement — deliberately smaller than everything above ──
	lead, contact := rec.ProviderFooterLead, rec.ProviderFooterContact
	if lead == "" || contact == "" {
		d := printing.DefaultProviderFooter()
		if lead == "" {
			lead = d.Lead
		}
		if contact == "" {
			contact = d.Contact
		}
	}
	buf.WriteString(fmt.Sprintf(`<div class="prov"><b>%s</b><br>%s</div>`, htmlEscape(lead), htmlEscape(contact)))
	buf.WriteString(`</body></html>`)
	return buf.Bytes()
}

// generateRetailReceiptPDF renders the same boxed retail design as a real A4 PDF (fpdf).
func generateRetailReceiptPDF(rec receiptResponse, brand receiptBrand) ([]byte, error) {
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
	if logo, lt := fetchReceiptLogo(brand.LogoURL); logo != nil && rec.ShowLogo {
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
	pdf.SetFont("Times", "B", 18)
	pdf.MultiCell(contentW, 8, strings.ToUpper(name), "", "C", false)
	if rec.OutletAddress != "" {
		pdf.SetFont("Times", "", 10)
		pdf.MultiCell(contentW, 5, strings.ToUpper(rec.OutletAddress), "", "C", false)
	}
	if rec.OutletPhones != "" {
		pdf.SetFont("Times", "B", 10)
		pdf.MultiCell(contentW, 5, "Mobile: "+rec.OutletPhones, "", "C", false)
	}
	if rec.EtimsKraPin != "" {
		pdf.SetFont("Times", "B", 10)
		pdf.MultiCell(contentW, 5, "KRA PIN: "+rec.EtimsKraPin, "", "C", false)
	}
	if rec.ReceiptHeader != "" {
		pdf.SetFont("Times", "B", 10)
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
	pdf.SetFont("Times", "B", 11)
	pdf.CellFormat(custW, 7, billLabel, "1", 0, "L", false, 0, "")
	pdf.CellFormat(invW, 7, "INVOICE.NO", "1", 0, "C", false, 0, "")
	pdf.CellFormat(dateW, 7, "DATE", "1", 1, "C", false, 0, "")
	pdf.SetFont("Times", "B", 10)
	pdf.CellFormat(custW, 8, strings.ToUpper(truncate(rec.BillTo, 38)), "1", 0, "L", false, 0, "")
	pdf.SetFont("Times", "", 10)
	pdf.CellFormat(invW, 8, rec.OrderNumber, "1", 0, "C", false, 0, "")
	pdf.CellFormat(dateW, 8, retailDateTime(rec.IssuedAt, rec.Timezone), "1", 1, "C", false, 0, "")
	pdf.Ln(2)

	// ── SERVED BY ──
	if rec.ServedBy != "" {
		pdf.SetFont("Times", "B", 10)
		pdf.CellFormat(contentW/2, 6, "SERVED BY", "B", 0, "L", false, 0, "")
		pdf.SetFont("Times", "", 10)
		pdf.CellFormat(contentW/2, 6, rec.ServedBy, "B", 1, "R", false, 0, "")
		pdf.Ln(2)
	}

	// ── Items table ──
	itemW, qtyW, priceW, subW := contentW*0.48, contentW*0.14, contentW*0.18, contentW*0.20
	pdf.SetFont("Times", "B", 11)
	pdf.CellFormat(itemW, 7, "Item Name", "1", 0, "L", false, 0, "")
	pdf.CellFormat(qtyW, 7, "Qty", "1", 0, "C", false, 0, "")
	pdf.CellFormat(priceW, 7, "Price", "1", 0, "R", false, 0, "")
	pdf.CellFormat(subW, 7, "Subtotal", "1", 1, "R", false, 0, "")
	pdf.SetFont("Times", "", 10)
	var totalQty float64
	for _, l := range rec.Lines {
		totalQty += l.Quantity
		lineName := l.Name
		if lineName == "" {
			lineName = l.SKU
		}
		price := retailAmount(l.UnitPrice)
		total := retailAmount(l.TotalPrice)
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
		pdf.SetFont("Times", style, 10.5)
		pdf.CellFormat(contentW*0.6, 5.5, label, "", 0, "L", false, 0, "")
		pdf.CellFormat(contentW*0.4, 5.5, value, "", 1, "R", false, 0, "")
	}
	trow("Total Quantity", fmt.Sprintf("%g", totalQty), false)
	trow("TOTAL ITEMS", fmt.Sprintf("%d", len(rec.Lines)), false)
	trow("Subtotal:", retailMoney(rec.Currency, rec.Subtotal), true)
	if rec.DiscountAmount > 0 {
		trow("Discount(-):", "-"+retailMoney(rec.Currency, rec.DiscountAmount), false)
	}
	if rec.VatEnabled || rec.TaxAmount > 0 {
		taxLabel := "Tax(+):"
		if rec.VatRate > 0 {
			taxLabel = fmt.Sprintf("VAT %g%%(+):", rec.VatRate)
		}
		trow(taxLabel, retailMoney(rec.Currency, rec.TaxAmount), false)
	}
	for _, cr := range retailChargeRows(rec) {
		trow(cr[0].(string)+":", retailMoney(rec.Currency, cr[1].(float64)), false)
	}
	if rec.RoundOff > 0 {
		trow("Round Off:", retailMoney(rec.Currency, rec.RoundOff), false)
	}
	trow("TOTAL:", retailMoney(rec.Currency, rec.TotalAmount), true)
	if pl := paymentMethodLabel(rec); pl != "" && rec.AmountPaid > 0 {
		trow(pl, retailMoney(rec.Currency, rec.AmountPaid), false)
	}
	trow("AMOUNT PAID", retailMoney(rec.Currency, rec.AmountPaid), false)
	if rec.ChangeDue > 0 {
		trow("Change", retailMoney(rec.Currency, rec.ChangeDue), false)
	}
	if math.Abs(rec.BalanceDue) >= 0.005 {
		trow("Total Due with Current", retailMoney(rec.Currency, rec.BalanceDue), false)
	}

	// ── KRA TIMS Details (fiscalised sales) — mirrors the paper ETR layout ──
	if rec.EtimsCuInvNo != "" || rec.EtimsInvoiceNumber != "" || rec.EtimsQRCodeURL != "" {
		pdf.Ln(4)
		pdf.SetFont("Times", "B", 12)
		pdf.CellFormat(contentW, 6, "KRA TIMS Details", "", 1, "C", false, 0, "")
		pdf.SetFont("Times", "", 10)
		if rec.EtimsScuID != "" {
			pdf.CellFormat(contentW, 5, "SCU ID: "+rec.EtimsScuID, "", 1, "C", false, 0, "")
		}
		if rec.EtimsCuInvNo != "" {
			pdf.CellFormat(contentW, 5, "CU_Inv No.: "+rec.EtimsCuInvNo, "", 1, "C", false, 0, "")
		} else if rec.EtimsInvoiceNumber != "" {
			pdf.CellFormat(contentW, 5, "CU No.: "+rec.EtimsInvoiceNumber, "", 1, "C", false, 0, "")
		}
		if rec.EtimsRcptSign != "" {
			pdf.SetFont("Times", "", 8)
			pdf.MultiCell(contentW, 4, "Sign: "+rec.EtimsRcptSign, "", "C", false)
		}
		if rec.EtimsQRCodeURL != "" {
			if qrPNG, qerr := qrcode.Encode(rec.EtimsQRCodeURL, qrcode.Medium, 256); qerr == nil {
				const qrW = 26.0
				if info := pdf.RegisterImageOptionsReader("etimsqr", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(qrPNG)); info != nil && info.Width() > 0 {
					pdf.ImageOptions("etimsqr", (pageW-qrW)/2, pdf.GetY()+1, qrW, qrW, true, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
				} else {
					pdf.ClearError()
				}
			}
		}
	}

	// ── Barcode of the invoice/order number ──
	if rec.OrderNumber != "" {
		if bcPNG, err := printing.Code128PNG(rec.OrderNumber, 400, 70); err == nil {
			const bcW, bcH = 55.0, 12.0
			pdf.Ln(4)
			if info := pdf.RegisterImageOptionsReader("orderbarcode", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(bcPNG)); info != nil && info.Width() > 0 {
				pdf.ImageOptions("orderbarcode", (pageW-bcW)/2, pdf.GetY(), bcW, bcH, true, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
				pdf.SetFont("Times", "", 10)
				pdf.CellFormat(contentW, 5, rec.OrderNumber, "", 1, "C", false, 0, "")
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
	pdf.SetFont("Times", "", 11)
	pdf.MultiCell(contentW, 5.5, strings.ToUpper(footer), "", "L", false)

	// ── Provider advertisement — smaller print ──
	lead, contact := rec.ProviderFooterLead, rec.ProviderFooterContact
	if lead == "" || contact == "" {
		d := printing.DefaultProviderFooter()
		if lead == "" {
			lead = d.Lead
		}
		if contact == "" {
			contact = d.Contact
		}
	}
	pdf.Ln(3)
	pdf.SetFont("Helvetica", "B", 6.8)
	pdf.MultiCell(contentW, 3.4, lead, "", "C", false)
	pdf.SetFont("Helvetica", "", 6.2)
	pdf.MultiCell(contentW, 3.2, contact, "", "C", false)

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("render retail receipt pdf: %w", err)
	}
	return buf.Bytes(), nil
}
