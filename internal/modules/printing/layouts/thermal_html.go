package layouts

import (
	"bytes"
	"fmt"

	"github.com/bengobox/pos-service/internal/modules/printing"
)

// thermalFontCSS returns the body font stack for the thermal layout variant.
// Classic = bold monospace (the proven hospitality receipt look); Modern and Grid = bold
// sans-serif (crisper glyphs on browser/PDF prints — the ETR-style retail look; bordered
// tables also read best in a sans face).
func thermalFontCSS(layout string) (family string, weight string) {
	if layout == ThermalClassic {
		return `'Courier New',Courier,'DejaVu Sans Mono',monospace`, "bold"
	}
	return `'Helvetica Neue',Helvetica,Arial,'Segoe UI',sans-serif`, "700"
}

// renderThermalHTML renders the receipt-roll HTML layout (58/80mm). Designed for
// window.print() and browser print-to-PDF.
//
// Print pipeline note: @page margin is 0 — a zero page margin is what suppresses the
// browser's own header/footer chrome (the "about:blank + date/URL" lines that ruined
// printed receipts); the visual margin is inner body padding instead.
//
// Grid mode (layout == ThermalGrid, opt-in per tenant): the customer/order/date meta and
// the item list render as bordered tables (same 2px-solid-black border styling as
// a4_html.go's grid, narrowed to thermal width) instead of flex "label ... value" lines —
// the clearest layout for less-tech-savvy customers. Totals/fiscal/HOW-TO-PAY/footer stay
// flex rows in every variant (matches the reference receipt this was modelled on, which
// only boxes the header/meta/items, not the totals or fine print).
func renderThermalHTML(rec Receipt, logoURL string, layout string) []byte {
	// Paper width drives the @page size and body width. Default 80mm.
	pageWidth, bodyWidth := "80mm", "72mm"
	if rec.PaperWidth == "58mm" {
		pageWidth, bodyWidth = "58mm", "50mm"
	}
	family, weight := thermalFontCSS(layout)
	isGrid := layout == ThermalGrid

	var buf bytes.Buffer
	buf.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8">`)
	buf.WriteString(`<title>Receipt ` + escape(rec.ReceiptNumber) + `</title>`)
	// White sheet, pure-black bold ink, colours kept exact (no brand colour) — thermal/non-colour
	// printers render gray/coloured text faint, so everything is black on white. The logo is the only
	// image and is grayscaled so a colour logo still prints crisply.
	buf.WriteString(fmt.Sprintf(`<style>
@page{size:%s auto;margin:0}
*{box-sizing:border-box}
body{font-family:%s;font-size:13px;font-weight:%s;color:#000;background:#fff;width:%s;margin:0 auto;padding:4mm 4px;line-height:1.35;-webkit-print-color-adjust:exact;print-color-adjust:exact;text-rendering:optimizeLegibility}
body,body *{color:#000}
.logo{display:block;margin:0 auto 4px;max-width:48mm;max-height:22mm;object-fit:contain;filter:grayscale(1) contrast(1.4)}
h1{font-size:17px;letter-spacing:.5px;text-align:center;margin:3px 0}
.sub{font-size:11px;text-align:center;margin:1px 0;color:#000}
.hdr{font-size:11px;text-align:center;margin:2px 0;white-space:pre-wrap}
.ftr{font-size:11px;text-align:center;margin:2px 0;white-space:pre-wrap}
.center{text-align:center}
.line{display:flex;justify-content:space-between;margin:2px 0;gap:6px}
.line span:first-child{flex:1;min-width:0;word-break:break-word;white-space:normal}
.line span:last-child{flex-shrink:0;white-space:nowrap}
.line-sub{font-size:10px;color:#000;padding-left:8px;margin:0 0 2px}
.divider{border-top:1px dashed #000;margin:4px 0}
.bold{font-weight:bold}
.tot{font-size:16px}
.prov{font-size:9px;text-align:center;margin:1px 0 2px;line-height:1.3;white-space:pre-wrap}
.prov-lead{font-size:10.5px;font-weight:bold;letter-spacing:.3px;text-align:center;margin:4px 0 1px;white-space:pre-wrap}
.etims-qr{display:block;margin:5px auto;width:100px;height:100px}
.etims-num{font-size:9px;text-align:center;word-break:break-all}
.barcode{text-align:center;margin:4px 0 2px}
.barcode img{height:11mm;max-width:100%%}
.barcode .num{font-size:11px;letter-spacing:2px;margin-top:1px}
.gbox{border:2px solid #000;padding:4px 6px;text-align:center;margin-bottom:3px}
.gtable{width:100%%;border-collapse:collapse;margin:4px 0}
.gtable th,.gtable td{border:2px solid #000;padding:2px 4px;font-size:10px;word-break:break-word}
.gtable th{font-weight:700;text-align:left}
.gtable td.c,.gtable th.c{text-align:center}
.gtable td.r,.gtable th.r{text-align:right}
.gserved{display:flex;justify-content:space-between;border-bottom:1.5px solid #000;padding:2px 1px;font-size:10px;margin-bottom:3px}
@media print{body{width:100%%}}
</style></head><body>`, pageWidth, family, weight, bodyWidth))
	// Logo prints only when the Receipt & Printing "show logo" setting allows it.
	if logoURL != "" && rec.ShowLogo {
		buf.WriteString(fmt.Sprintf(`<img class="logo" src="%s" alt="logo">`, escape(logoURL)))
	}
	if isGrid {
		buf.WriteString(`<div class="gbox">`)
	}
	if rec.OutletName != "" {
		buf.WriteString(fmt.Sprintf(`<h1>%s</h1>`, escape(rec.OutletName)))
	} else {
		buf.WriteString(`<h1>RECEIPT</h1>`)
	}
	if rec.OutletAddress != "" {
		buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, escape(rec.OutletAddress)))
	}
	if rec.OutletPhones != "" {
		buf.WriteString(fmt.Sprintf(`<p class="sub"><b>Mobile:</b> %s</p>`, escape(rec.OutletPhones)))
	}
	if rec.EtimsKraPin != "" {
		buf.WriteString(fmt.Sprintf(`<p class="sub"><b>KRA PIN:</b> %s</p>`, escape(rec.EtimsKraPin)))
	}
	// Custom header text configured in POS settings (business name, address, slogan…).
	if rec.ReceiptHeader != "" {
		buf.WriteString(fmt.Sprintf(`<p class="hdr">%s</p>`, escape(rec.ReceiptHeader)))
	}
	if isGrid {
		buf.WriteString(`</div>`)
	}

	billLabel := rec.BillToLabel
	if billLabel == "" {
		billLabel = "Customer"
	}
	if isGrid {
		// Bordered Customer | Receipt No | Date mini-table — same grid pattern as the A4
		// invoice's Customer|INVOICE.NO|DATE header, narrowed to thermal width.
		buf.WriteString(`<table class="gtable"><tr>`)
		buf.WriteString(fmt.Sprintf(`<th>%s</th><th class="c">RECEIPT NO</th></tr><tr>`, escape(billLabel)))
		if rec.BillTo != "" {
			buf.WriteString(fmt.Sprintf(`<td>%s</td>`, escape(rec.BillTo)))
		} else {
			buf.WriteString(`<td>Walk-in customer</td>`)
		}
		buf.WriteString(fmt.Sprintf(`<td class="c">%s</td></tr></table>`, escape(rec.ReceiptNumber)))
		buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, receiptTime(rec.IssuedAt, rec.Timezone)))
		if rec.ServedBy != "" {
			buf.WriteString(fmt.Sprintf(`<div class="gserved"><b>SERVED BY</b><span>%s</span></div>`, escape(rec.ServedBy)))
		}
	} else {
		buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, receiptTime(rec.IssuedAt, rec.Timezone)))
		buf.WriteString(fmt.Sprintf(`<p class="sub">Receipt: %s</p>`, escape(rec.ReceiptNumber)))
		if rec.BillTo != "" {
			buf.WriteString(fmt.Sprintf(`<p class="sub">%s: %s</p>`, escape(billLabel), escape(rec.BillTo)))
		}
		if rec.ServedBy != "" {
			buf.WriteString(fmt.Sprintf(`<p class="sub">Served by: %s</p>`, escape(rec.ServedBy)))
		}
		buf.WriteString(`<div class="divider"></div>`)
	}

	if isGrid {
		// Bordered Item Name | Qty | Price | Subtotal table — Qty and Price are their own
		// columns, so (unlike the flex layouts) no separate qty-subline is needed here.
		buf.WriteString(`<table class="gtable"><tr><th>Item</th><th class="c">Qty</th><th class="r">Price</th><th class="r">Total</th></tr>`)
		for _, l := range rec.Lines {
			price, total := amount(l.UnitPrice), amount(l.TotalPrice)
			if l.TotalPrice == 0 {
				price, total = "FREE", "FREE"
			}
			buf.WriteString(fmt.Sprintf(`<tr><td>%s</td><td class="c">%g</td><td class="r">%s</td><td class="r">%s</td></tr>`,
				escape(l.Name), l.Quantity, price, total))
		}
		buf.WriteString(`</table>`)
	} else {
		for _, l := range rec.Lines {
			// A zero-charge line (complimentary accompaniment / bundled side) prints "FREE" rather than
			// "0.00" so the bill clearly shows it was included at no charge.
			amt := amount(l.TotalPrice)
			if l.TotalPrice == 0 {
				amt = "FREE"
			}
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s</span><span>%s</span></div>`, escape(l.Name), amt))
			// Qty × unit-price sub-line whenever qty ≠ 1 — the clearest way to show quantity
			// (matches the pos-ui client thermal renderer's existing pattern).
			if l.Quantity != 1 {
				buf.WriteString(fmt.Sprintf(`<div class="line-sub">%g &times; %s</div>`, l.Quantity, amount(l.UnitPrice)))
			}
		}
		buf.WriteString(`<div class="divider"></div>`)
	}
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>Subtotal</span><span>%s</span></div>`, amount(rec.Subtotal)))
	tl := "Tax"
	if rec.VatEnabled && rec.VatRate > 0 {
		tl = taxLabel(rec)
	}
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s</span><span>%s</span></div>`, tl, amount(rec.TaxAmount)))
	if rec.DiscountAmount > 0 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Discount</span><span>-%s</span></div>`, amount(rec.DiscountAmount)))
	}
	for _, cr := range chargeRows(rec) {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s</span><span>%s</span></div>`, escape(cr[0].(string)), amount(cr[1].(float64))))
	}
	if rec.RoundOff > 0 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Round Off</span><span>%s</span></div>`, amount(rec.RoundOff)))
	}
	buf.WriteString(fmt.Sprintf(`<div class="line bold tot"><span>TOTAL</span><span>%s %s</span></div>`, amount(rec.TotalAmount), escape(rec.Currency)))
	// Payment method with settle date on retail ("Cash (14-07-2026)") — matches ESC/POS.
	if rec.UseCase == "retail" {
		if pl := paymentMethodLabel(rec); pl != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>Payment</span><span>%s</span></div>`, escape(pl)))
		}
	}
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>Paid</span><span>%s</span></div>`, amount(rec.AmountPaid)))
	// Tendered duplicates Paid on exact-settle retail receipts — print only when it differs.
	if rec.AmountTendered > 0 && !(rec.UseCase == "retail" && rec.AmountTendered == rec.AmountPaid) {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Tendered</span><span>%s</span></div>`, amount(rec.AmountTendered)))
	}
	if rec.ChangeDue > 0 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Change</span><span>%s</span></div>`, amount(rec.ChangeDue)))
	}
	if rec.BalanceDue > 0.004 || rec.BalanceDue < -0.004 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Balance Due</span><span>%s</span></div>`, amount(rec.BalanceDue)))
	}
	if pm := rec.PaymentMethods; pm != nil {
		buf.WriteString(`<div class="divider"></div>`)
		buf.WriteString(`<p class="bold" style="font-size:10px;text-align:center">HOW TO PAY</p>`)
		if pm.MpesaPaybill != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>M-PESA Paybill</span><span>%s</span></div>`, escape(pm.MpesaPaybill)))
		}
		if pm.MpesaAccountRef != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>Account No.</span><span>%s</span></div>`, escape(pm.MpesaAccountRef)))
		}
		if pm.MpesaTill != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>M-PESA Till</span><span>%s</span></div>`, escape(pm.MpesaTill)))
		}
		if pm.MpesaPochi != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>M-PESA Pochi</span><span>%s</span></div>`, escape(pm.MpesaPochi)))
		}
		if pm.BankName != "" || pm.BankAccountNumber != "" {
			label := pm.BankName
			if label == "" {
				label = "Bank"
			}
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s</span><span>%s</span></div>`, escape(label), escape(pm.BankAccountNumber)))
		}
		if pm.BankAccountName != "" {
			buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, escape(pm.BankAccountName)))
		}
	}
	// "KRA TIMS Details" fiscal block, adapted from the KRA-issued paper ETR receipt (see the
	// Jazaribu Retail reference): SCU ID + CU Inv No, then the verification QR immediately
	// below, then — right after, no other content between — the fiscal barcode (the same
	// adjacency a genuine ETR receipt uses). The receipt SIGNATURE is deliberately never
	// printed as plain text: it's already encoded in the QR, and printing it in the clear is
	// an avoidable exposure of KRA-issued fiscal proof with no benefit to the operator.
	if rec.EtimsInvoiceNumber != "" || rec.EtimsQRPNG != "" || rec.EtimsCuInvNo != "" {
		buf.WriteString(`<div class="divider"></div>`)
		buf.WriteString(`<p class="bold" style="font-size:10px;text-align:center">KRA TIMS Details</p>`)
		if rec.EtimsScuID != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>SCU ID:</span><span>%s</span></div>`, escape(rec.EtimsScuID)))
		}
		if rec.EtimsCuInvNo != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>CU Inv No.:</span><span>%s</span></div>`, escape(rec.EtimsCuInvNo)))
		} else if rec.EtimsInvoiceNumber != "" {
			buf.WriteString(fmt.Sprintf(`<p class="etims-num">CU No: %s</p>`, escape(rec.EtimsInvoiceNumber)))
		}
		// The QR is server-generated (EtimsQRPNG, a data: URI) — never render the raw
		// verification LINK (EtimsQRCodeURL) as an <img src>; a URL is not image bytes.
		if rec.EtimsQRPNG != "" {
			buf.WriteString(fmt.Sprintf(`<img class="etims-qr" src="%s" alt="eTIMS QR">`, rec.EtimsQRPNG))
		}
	}
	// Fiscal barcode — encodes rec.BarcodeValue (the eTIMS CU Invoice Number once fiscalised,
	// else the order number for non-fiscalised retail sales; ReceiptView.FiscalBarcodeValue).
	// Printed immediately after the fiscal block, mirroring the ETR reference layout.
	if rec.BarcodePNG != "" {
		buf.WriteString(fmt.Sprintf(`<div class="barcode"><img src="%s" alt="barcode"><div class="num">%s</div></div>`,
			rec.BarcodePNG, escape(rec.BarcodeValue)))
	}
	buf.WriteString(`<div class="divider"></div>`)
	// Custom footer text configured in POS settings; fall back to a friendly default.
	footer := rec.ReceiptFooter
	if footer == "" {
		footer = "Thank you for your business!"
	}
	buf.WriteString(fmt.Sprintf(`<p class="ftr">%s</p>`, escape(footer)))
	// Platform-owner (Codevertex) advertisement — always shown on the customer receipt.
	lead, contact := providerFooter(rec)
	buf.WriteString(`<div class="divider"></div>`)
	buf.WriteString(fmt.Sprintf(`<p class="prov-lead">&#9733; %s &#9733;</p>`, escape(lead)))
	buf.WriteString(fmt.Sprintf(`<p class="prov">%s</p>`, escape(contact)))
	buf.WriteString(`<div class="divider"></div>`)
	buf.WriteString(`</body></html>`)
	return buf.Bytes()
}

// providerFooter returns the resolved provider-advertisement lines, falling back to the
// static defaults so the advertisement always prints.
func providerFooter(rec Receipt) (lead, contact string) {
	lead, contact = rec.ProviderFooterLead, rec.ProviderFooterContact
	d := printing.DefaultProviderFooter()
	if lead == "" {
		lead = d.Lead
	}
	if contact == "" {
		contact = d.Contact
	}
	return lead, contact
}
