package layouts

import (
	"bytes"
	"fmt"

	"github.com/bengobox/pos-service/internal/modules/printing"
)

// thermalFontCSS returns the body font stack for the thermal layout variant.
// Classic = bold monospace (the proven hospitality receipt look); Modern = bold
// sans-serif (crisper glyphs on browser/PDF prints — the ETR-style retail look).
func thermalFontCSS(layout string) (family string, weight string) {
	if layout == ThermalModern {
		return `'Helvetica Neue',Helvetica,Arial,'Segoe UI',sans-serif`, "700"
	}
	return `'Courier New',Courier,'DejaVu Sans Mono',monospace`, "bold"
}

// renderThermalHTML renders the receipt-roll HTML layout (58/80mm). Designed for
// window.print() and browser print-to-PDF.
//
// Print pipeline note: @page margin is 0 — a zero page margin is what suppresses the
// browser's own header/footer chrome (the "about:blank + date/URL" lines that ruined
// printed receipts); the visual margin is inner body padding instead.
func renderThermalHTML(rec Receipt, logoURL string, layout string) []byte {
	// Paper width drives the @page size and body width. Default 80mm.
	pageWidth, bodyWidth := "80mm", "72mm"
	if rec.PaperWidth == "58mm" {
		pageWidth, bodyWidth = "58mm", "50mm"
	}
	family, weight := thermalFontCSS(layout)

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
.line{display:flex;justify-content:space-between;margin:2px 0}
.divider{border-top:1px dashed #000;margin:4px 0}
.bold{font-weight:bold}
.tot{font-size:16px}
.prov{font-size:9px;text-align:center;margin:1px 0 2px;line-height:1.3;white-space:pre-wrap}
.prov-lead{font-size:10.5px;font-weight:bold;letter-spacing:.3px;text-align:center;margin:4px 0 1px;white-space:pre-wrap}
.etims-qr{display:block;margin:4px auto;width:80px;height:80px}
.etims-num{font-size:9px;text-align:center;word-break:break-all}
.barcode{text-align:center;margin:6px 0 2px}
.barcode img{height:11mm;max-width:100%%}
.barcode .num{font-size:11px;letter-spacing:2px;margin-top:1px}
@media print{body{width:100%%}}
</style></head><body>`, pageWidth, family, weight, bodyWidth))
	// Logo prints only when the Receipt & Printing "show logo" setting allows it.
	if logoURL != "" && rec.ShowLogo {
		buf.WriteString(fmt.Sprintf(`<img class="logo" src="%s" alt="logo">`, escape(logoURL)))
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
	buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, receiptTime(rec.IssuedAt, rec.Timezone)))
	buf.WriteString(fmt.Sprintf(`<p class="sub">Receipt: %s</p>`, escape(rec.ReceiptNumber)))
	if rec.BillTo != "" {
		label := rec.BillToLabel
		if label == "" {
			label = "Customer"
		}
		buf.WriteString(fmt.Sprintf(`<p class="sub">%s: %s</p>`, escape(label), escape(rec.BillTo)))
	}
	if rec.ServedBy != "" {
		buf.WriteString(fmt.Sprintf(`<p class="sub">Served by: %s</p>`, escape(rec.ServedBy)))
	}
	buf.WriteString(`<div class="divider"></div>`)
	for _, l := range rec.Lines {
		// A zero-charge line (complimentary accompaniment / bundled side) prints "FREE" rather than
		// "0.00" so the bill clearly shows it was included at no charge.
		amt := fmt.Sprintf("%.2f", l.TotalPrice)
		if l.TotalPrice == 0 {
			amt = "FREE"
		}
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s x%.0f</span><span>%s</span></div>`, escape(l.Name), l.Quantity, amt))
	}
	buf.WriteString(`<div class="divider"></div>`)
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>Subtotal</span><span>%.2f</span></div>`, rec.Subtotal))
	tl := "Tax"
	if rec.VatEnabled && rec.VatRate > 0 {
		tl = taxLabel(rec)
	}
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s</span><span>%.2f</span></div>`, tl, rec.TaxAmount))
	if rec.DiscountAmount > 0 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Discount</span><span>-%.2f</span></div>`, rec.DiscountAmount))
	}
	if rec.ChargesTotal > 0 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Charges</span><span>%.2f</span></div>`, rec.ChargesTotal))
	}
	if rec.RoundOff > 0 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Round Off</span><span>%.2f</span></div>`, rec.RoundOff))
	}
	buf.WriteString(fmt.Sprintf(`<div class="line bold tot"><span>TOTAL</span><span>%.2f %s</span></div>`, rec.TotalAmount, escape(rec.Currency)))
	// Payment method with settle date on retail ("Cash (14-07-2026)") — matches ESC/POS.
	if rec.UseCase == "retail" {
		if pl := paymentMethodLabel(rec); pl != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>Payment</span><span>%s</span></div>`, escape(pl)))
		}
	}
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>Paid</span><span>%.2f</span></div>`, rec.AmountPaid))
	// Tendered duplicates Paid on exact-settle retail receipts — print only when it differs.
	if rec.AmountTendered > 0 && !(rec.UseCase == "retail" && rec.AmountTendered == rec.AmountPaid) {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Tendered</span><span>%.2f</span></div>`, rec.AmountTendered))
	}
	if rec.ChangeDue > 0 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Change</span><span>%.2f</span></div>`, rec.ChangeDue))
	}
	if rec.BalanceDue > 0.004 || rec.BalanceDue < -0.004 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Balance Due</span><span>%.2f</span></div>`, rec.BalanceDue))
	}
	if rec.EtimsInvoiceNumber != "" || rec.EtimsQRCodeURL != "" || rec.EtimsCuInvNo != "" {
		// "KRA TIMS Details" block, mirroring the paper ETR layout: SCU ID + CU Inv No
		// ({SCU}/{receipt no}) + signature above the verification QR.
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
		if rec.EtimsRcptSign != "" {
			buf.WriteString(fmt.Sprintf(`<p class="sub" style="word-break:break-all">Sign: %s</p>`, escape(rec.EtimsRcptSign)))
		}
		if rec.EtimsQRPNG != "" {
			buf.WriteString(fmt.Sprintf(`<img class="etims-qr" src="%s" alt="eTIMS QR">`, rec.EtimsQRPNG))
		} else if rec.EtimsQRCodeURL != "" {
			buf.WriteString(fmt.Sprintf(`<img class="etims-qr" src="%s" alt="eTIMS QR">`, escape(rec.EtimsQRCodeURL)))
		}
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
	// Code 128 barcode of the order number (retail receipts populate BarcodePNG) —
	// scannable for returns/lookups, matching the ESC/POS + A4 surfaces.
	if rec.BarcodePNG != "" {
		buf.WriteString(fmt.Sprintf(`<div class="barcode"><img src="%s" alt="barcode"><div class="num">%s</div></div>`,
			rec.BarcodePNG, escape(rec.OrderNumber)))
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
