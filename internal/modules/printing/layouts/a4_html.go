package layouts

import (
	"bytes"
	"fmt"
	"math"
	"strings"
)

// renderA4HTML renders the boxed invoice-style receipt as printable HTML (A4 portrait):
// boxed business header, a Customer | INVOICE.NO | DATE table, SERVED BY line, a bordered
// items table (Item Name/Qty/Price/Subtotal), a totals block with quantity/item counts,
// the itemised discount/tax/charges breakdown, the payment method with its date
// ("Cash (14-07-2026)"), AMOUNT PAID and the balance due, a Code 128 barcode of the
// invoice/order number, the configurable footer text and the provider advertisement.
//
// Print pipeline note: @page margin is 0 (suppresses the browser's own header/footer
// chrome — the about:blank/date/URL lines); the sheet margin is inner body padding.
func renderA4HTML(rec Receipt, logoURL string) []byte {
	var totalQty float64
	for _, l := range rec.Lines {
		totalQty += l.Quantity
	}

	var buf bytes.Buffer
	buf.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8">`)
	buf.WriteString(`<title>Receipt ` + escape(rec.ReceiptNumber) + `</title>`)
	// Typography: Helvetica Neue sans stack at >=13px with a medium (500) baseline and bold
	// (700) emphasis — thin serif strokes printed almost invisibly on low-toner printers.
	buf.WriteString(`<style>
@page{size:A4 portrait;margin:0}
*{box-sizing:border-box}
body{font-family:'Helvetica Neue',Helvetica,Arial,'Segoe UI',sans-serif;font-size:14px;font-weight:500;color:#000;background:#fff;max-width:210mm;margin:0 auto;padding:12mm;-webkit-print-color-adjust:exact;print-color-adjust:exact}
.box{border:2px solid #000;padding:6px 10px;text-align:center}
.biz{font-size:24px;font-weight:700;letter-spacing:.5px;margin:2px 0}
.sub{font-size:13px;margin:1px 0;white-space:pre-wrap}
.logo{display:block;margin:2px auto;max-height:22mm;max-width:60mm;object-fit:contain}
table{width:100%;border-collapse:collapse;margin-top:6px}
th,td{border:2px solid #000;padding:4px 8px;font-size:14px}
th{font-weight:700;text-align:left}
td.c,th.c{text-align:center}
td.r,th.r{text-align:right}
.served{display:flex;justify-content:space-between;border-bottom:1.5px solid #000;padding:5px 2px;font-size:13px;margin-top:4px}
.totals{margin-top:8px}
.trow{display:flex;justify-content:space-between;padding:1.5px 2px;font-size:14px}
.trow.b{font-weight:700}
.barcode{text-align:center;margin:14px 0 4px}
.barcode img{height:14mm}
.barcode .num{font-size:13px;letter-spacing:2px;margin-top:1px}
.ftr{font-size:14px;margin:12px 0 4px;white-space:pre-wrap}
.prov{font-size:10px;text-align:center;color:#000;margin-top:10px;line-height:1.4}
@media print{body{max-width:100%}}
</style></head><body>`)

	// ── Boxed business header ──
	buf.WriteString(`<div class="box">`)
	if logoURL != "" && rec.ShowLogo {
		buf.WriteString(fmt.Sprintf(`<img class="logo" src="%s" alt="logo">`, escape(logoURL)))
	}
	name := rec.OutletName
	if name == "" {
		name = "RECEIPT"
	}
	buf.WriteString(fmt.Sprintf(`<div class="biz">%s</div>`, escape(strings.ToUpper(name))))
	if rec.OutletAddress != "" {
		buf.WriteString(fmt.Sprintf(`<div class="sub">%s</div>`, escape(strings.ToUpper(rec.OutletAddress))))
	}
	if rec.OutletPhones != "" {
		buf.WriteString(fmt.Sprintf(`<div class="sub"><b>Mobile:</b> %s</div>`, escape(rec.OutletPhones)))
	}
	if rec.EtimsKraPin != "" {
		buf.WriteString(fmt.Sprintf(`<div class="sub"><b>KRA PIN:</b> %s</div>`, escape(rec.EtimsKraPin)))
	}
	if rec.ReceiptHeader != "" {
		buf.WriteString(fmt.Sprintf(`<div class="sub"><b>%s</b></div>`, escape(rec.ReceiptHeader)))
	}
	buf.WriteString(`</div>`)

	// ── Customer | INVOICE.NO | DATE ──
	billLabel := rec.BillToLabel
	if billLabel == "" {
		billLabel = "Customer"
	}
	buf.WriteString(`<table><tr>`)
	buf.WriteString(fmt.Sprintf(`<th style="width:45%%">%s</th><th class="c" style="width:22%%">INVOICE.NO</th><th class="c">DATE</th>`, escape(billLabel)))
	buf.WriteString(`</tr><tr>`)
	buf.WriteString(fmt.Sprintf(`<td><b>%s</b></td>`, escape(strings.ToUpper(rec.BillTo))))
	buf.WriteString(fmt.Sprintf(`<td class="c">%s</td>`, escape(rec.OrderNumber)))
	buf.WriteString(fmt.Sprintf(`<td class="c">%s</td>`, shortDateTime(rec.IssuedAt, rec.Timezone)))
	buf.WriteString(`</tr></table>`)

	// ── SERVED BY ──
	if rec.ServedBy != "" {
		buf.WriteString(fmt.Sprintf(`<div class="served"><b>SERVED BY</b><span>%s</span></div>`, escape(rec.ServedBy)))
	}

	// ── Items ──
	buf.WriteString(`<table><tr><th>Item Name</th><th class="c" style="width:14%">Qty</th><th class="r" style="width:18%">Price</th><th class="r" style="width:20%">Subtotal</th></tr>`)
	for _, l := range rec.Lines {
		lineName := l.Name
		if lineName == "" {
			lineName = l.SKU
		}
		price := amount(l.UnitPrice)
		total := amount(l.TotalPrice)
		if l.TotalPrice == 0 {
			price, total = "FREE", "FREE"
		}
		buf.WriteString(fmt.Sprintf(`<tr><td>%s</td><td class="c">%g</td><td class="r">%s</td><td class="r">%s</td></tr>`,
			escape(strings.ToUpper(lineName)), l.Quantity, price, total))
	}
	buf.WriteString(`</table>`)

	// ── Totals block ──
	trow := func(label, value string, bold bool) {
		cls := "trow"
		if bold {
			cls = "trow b"
		}
		buf.WriteString(fmt.Sprintf(`<div class="%s"><span>%s</span><span>%s</span></div>`, cls, escape(label), escape(value)))
	}
	buf.WriteString(`<div class="totals">`)
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
	if rec.CustomerAccountBalance != nil {
		trow(rec.CustomerAccountBalanceLabel, money(rec.Currency, *rec.CustomerAccountBalance), false)
	}
	buf.WriteString(`</div>`)

	// ── KRA TIMS Details, adapted from the KRA-issued paper ETR receipt (see the Jazaribu
	// Retail reference): SCU ID + CU Inv No, then the verification QR, then — right after,
	// no other content between — the fiscal barcode below. The receipt SIGNATURE is
	// deliberately never printed as plain text (it's already encoded in the QR). ──
	if rec.EtimsCuInvNo != "" || rec.EtimsInvoiceNumber != "" || rec.EtimsQRPNG != "" {
		buf.WriteString(`<div style="text-align:center;margin-top:10px">`)
		buf.WriteString(`<div style="font-weight:bold;font-size:14px">KRA TIMS Details</div>`)
		if rec.EtimsScuID != "" {
			buf.WriteString(fmt.Sprintf(`<div class="sub"><b>SCU ID:</b> %s</div>`, escape(rec.EtimsScuID)))
		}
		if rec.EtimsCuInvNo != "" {
			buf.WriteString(fmt.Sprintf(`<div class="sub"><b>CU_Inv No.:</b> %s</div>`, escape(rec.EtimsCuInvNo)))
		} else if rec.EtimsInvoiceNumber != "" {
			buf.WriteString(fmt.Sprintf(`<div class="sub"><b>CU No.:</b> %s</div>`, escape(rec.EtimsInvoiceNumber)))
		}
		// Server-generated QR only (EtimsQRPNG, a data: URI) — the verification LINK
		// (EtimsQRCodeURL) is never rendered as an <img src>; a URL is not image bytes.
		if rec.EtimsQRPNG != "" {
			buf.WriteString(fmt.Sprintf(`<img src="%s" alt="eTIMS QR" style="height:32mm;margin-top:4px">`, rec.EtimsQRPNG))
		}
		buf.WriteString(`</div>`)
	}

	// ── Fiscal barcode: encodes rec.BarcodeValue (the eTIMS CU Invoice Number once
	// fiscalised, else the order number for non-fiscalised retail sales). ──
	if rec.BarcodePNG != "" {
		buf.WriteString(fmt.Sprintf(`<div class="barcode"><img src="%s" alt="barcode"><div class="num">%s</div></div>`,
			rec.BarcodePNG, escape(rec.BarcodeValue)))
	}

	// ── Configurable footer text ("IN GOD WE TRUST" position — flows below the barcode) ──
	footer := rec.ReceiptFooter
	if footer == "" {
		footer = "Thank you for your business!"
	}
	buf.WriteString(fmt.Sprintf(`<div class="ftr">%s</div>`, escape(strings.ToUpper(footer))))

	// ── Provider advertisement — deliberately smaller than everything above ──
	lead, contact := providerFooter(rec)
	buf.WriteString(fmt.Sprintf(`<div class="prov"><b>%s</b><br>%s</div>`, escape(lead), escape(contact)))
	buf.WriteString(`</body></html>`)
	return buf.Bytes()
}
