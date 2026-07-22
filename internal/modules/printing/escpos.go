// Package printing provides ESC/POS receipt generation and network printer dispatch.
package printing

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

// ESC/POS control bytes
var (
	escInit   = []byte{0x1B, 0x40}          // Initialize printer
	escCut    = []byte{0x1D, 0x56, 0x42, 0} // Full cut
	escBold   = []byte{0x1B, 0x45, 0x01}    // Bold on
	escFontA  = []byte{0x1B, 0x4D, 0x00}    // ESC M 0 — select Font A (12x24); some printers default to the condensed Font B
	escCenter = []byte{0x1B, 0x61, 0x01}    // Center align
	escLeft   = []byte{0x1B, 0x61, 0x00}    // Left align
	escLF     = []byte{0x0A}                // Line feed
	// Character-size control (GS ! n): the low nibble is the width multiplier and the high
	// nibble the height multiplier. Larger glyphs print far crisper on low-DPI thermal heads
	// than the default 1x font, so the shop name + TOTAL (the two things a customer actually
	// reads) are emphasised. Always pair *On with escSizeReset.
	escDoubleHW  = []byte{0x1D, 0x21, 0x11} // double width + double height
	escDoubleH   = []byte{0x1D, 0x21, 0x01} // double height only
	escSizeReset = []byte{0x1D, 0x21, 0x00} // back to 1x
)

// ReceiptData holds all data needed to render a receipt.
type ReceiptData struct {
	Type          string // "customer", "kitchen_ticket", "waiter_copy", "void"
	OutletName    string
	OutletAddress string
	OutletPhones  string // formatted labeled phones, printed as "Mobile: …" under the address
	OutletEmail   string // branch contact email (falls back to the tenant's general email)
	OrderNumber   string
	BillTo        string
	BillToLabel   string // "Customer" or "Paid by"
	ServedBy      string
	TableRef      string
	DateTime      time.Time
	Header        string // custom header text from OutletSetting
	Footer        string // custom footer text from OutletSetting
	Items         []ReceiptItem
	Subtotal      float64
	TaxTotal      float64
	VatRate       float64 // percentage, e.g. 16 — 0 means "unknown", falls back to the plain "Tax" label
	DiscountTotal float64
	ChargesTotal  float64
	RoundOff      float64
	TotalAmount   float64
	PaymentMethod string
	PaymentDate   *time.Time // when the payment settled — retail prints it beside the method
	AmountPaid    float64
	BalanceDue    float64 // total − paid; printed when non-zero (on-account / customer credit)
	// CustomerAccountBalance is the customer's OVERALL treasury AR position (distinct from
	// BalanceDue, which is scoped to this order) — nil when not applicable/resolved.
	CustomerAccountBalance      *float64
	CustomerAccountBalanceLabel string
	AmountTendered              float64
	ChangeDue                   float64
	Currency                    string
	EtimsInvoiceNumber          string
	// Fiscal identity ("KRA TIMS Details" block, mirroring paper ETR receipts).
	EtimsKraPin   string // printed in the business header as "KRA PIN: …"
	EtimsScuID    string // "SCU ID" line
	EtimsCuInvNo  string // "CU Inv No." line — {SCU ID}/{receipt no}
	EtimsRcptSign string
	// EtimsQRCodeURL is the KRA receipt-verification link — printed as a scannable QR at
	// the bottom of the fiscal block (native GS ( k, or GS v 0 raster when QRRaster).
	EtimsQRCodeURL string
	// QRRaster selects the raster bit-image QR encoding for printers whose firmware lacks
	// GS ( k (printer_profiles JSON `"qr_native": false`).
	QRRaster       bool
	PaymentMethods *ReceiptPaymentMethods // "HOW TO PAY" block (M-Pesa/bank), customer receipts only
	VoidReason     string
	// Banner is an attention line under the ticket-type heading — e.g.
	// "*** ADDITIONAL ITEMS ***" on a delta kitchen chit fired when a waiter adds
	// to an open bill, so the kitchen never mistakes it for a brand-new order.
	Banner         string
	ProviderFooter ProviderFooter // platform-owner (Codevertex) advertisement, customer receipts only
	// UseCase — "retail" additionally prints the payment date beside the method
	// (the BOI/GoDigital receipt design).
	UseCase string
	// BarcodeValue — when non-empty, a native Code 128 barcode of this value is printed:
	// the eTIMS CU Invoice Number once fiscalised, else the order number for retail sales.
	// Computed once by ReceiptView.FiscalBarcodeValue so ESC/POS never diverges from the
	// server HTML/PDF/client barcode. Empty = no barcode.
	BarcodeValue string
}

// ReceiptItem is a single line on the receipt.
type ReceiptItem struct {
	Name     string
	Quantity float64
	Price    float64
	Total    float64
	Notes    string
}

// BuildReceipt renders an ESC/POS byte buffer for the given receipt type. Section order mirrors
// generateReceiptHTML / the JSON receipt view: outlet name+address → custom header → order meta
// (order#, bill-to, served-by, table, time) → items → subtotal/VAT(rate)/discount/charges/
// round-off → TOTAL → tendered/change → eTIMS CU# → HOW TO PAY → footer. Keep these three renderers
// (HTML, JSON, ESC/POS) in sync — they all read from the same printing.ReceiptView.
func BuildReceipt(d ReceiptData) []byte {
	var buf bytes.Buffer

	write := func(b []byte) { buf.Write(b) }
	writeln := func(s string) { buf.WriteString(s); buf.Write(escLF) }
	separator := func() { writeln(strings.Repeat("-", 32)) }

	write(escInit)
	// Whole-ticket legibility baseline: Font A (never the condensed Font B some printers
	// default to) + emphasis ON for EVERY line. Faded/low-density thermal heads print 1x
	// unemphasised text almost invisibly — the "barely visible receipt" complaint — so the
	// body never drops back to unemphasised text; bigger sections stack on top of this.
	write(escFontA)
	write(escBold)
	write(escCenter)
	// Shop name in double-height+bold — the biggest, crispest thing on the receipt so it stays
	// legible on low-DPI thermal heads (addresses the "blurry receipt" complaint).
	write(escDoubleHW)
	if d.OutletName != "" {
		writeln(d.OutletName)
	} else {
		writeln("RECEIPT")
	}
	write(escSizeReset)
	if d.OutletAddress != "" {
		writeln(d.OutletAddress)
	}
	if d.OutletPhones != "" {
		writeln("Mobile: " + d.OutletPhones)
	}
	if d.OutletEmail != "" {
		writeln("Email: " + d.OutletEmail)
	}
	if d.Type == "customer" && d.EtimsKraPin != "" {
		writeln("KRA PIN: " + d.EtimsKraPin)
	}
	if d.Header != "" {
		writeln(d.Header)
	}
	write(escLeft)

	switch d.Type {
	case "kitchen_ticket":
		write(escCenter)
		write(escBold)
		writeln("** KITCHEN **")
		if d.Banner != "" {
			// Double-size so a delta chit ("ADDITIONAL ITEMS") is unmissable at the pass.
			write(escDoubleHW)
			writeln(d.Banner)
			write(escSizeReset)
		}
		write(escLeft)
	case "waiter_copy":
		write(escCenter)
		write(escBold)
		writeln("** WAITER COPY **")
		write(escLeft)
	case "void":
		write(escCenter)
		write(escBold)
		writeln("** VOID RECEIPT **")
		write(escLeft)
	}

	separator()
	writeln(fmt.Sprintf("Order:   #%s", d.OrderNumber))
	if (d.Type == "customer" || d.Type == "void") && d.BillTo != "" {
		label := d.BillToLabel
		if label == "" {
			label = "Customer"
		}
		writeln(fmt.Sprintf("%s: %s", label, d.BillTo))
	}
	if d.TableRef != "" {
		writeln(fmt.Sprintf("Table:   %s", d.TableRef))
	}
	writeln(fmt.Sprintf("Time:    %s", d.DateTime.Format("02 Jan 2006 15:04")))
	if d.ServedBy != "" && d.Type != "kitchen_ticket" {
		writeln(fmt.Sprintf("Server:  %s", d.ServedBy))
	}
	separator()

	for _, item := range d.Items {
		qty := fmt.Sprintf("%.0fx", item.Quantity)
		if item.Quantity != float64(int(item.Quantity)) {
			qty = fmt.Sprintf("%.2fx", item.Quantity)
		}
		prefix := fmt.Sprintf("%-3s ", qty)
		if d.Type == "kitchen_ticket" || d.Type == "waiter_copy" {
			// Kitchen/waiter tickets show no prices — the name still gets the full 32-col
			// budget minus the qty prefix, clamped so a long name wraps cleanly rather than
			// overflowing past the printer's own line width.
			writeln(prefix + trimField(item.Name, 32-len(prefix)))
		} else {
			total := fmt.Sprintf("%s %.2f", d.Currency, item.Total)
			// Right-align price in 32-char width. Clamp the NAME (not just the gap) so
			// label+value never silently overflows column 32 — a long item name is
			// truncated rather than misaligning the printer's own wrap.
			maxName := 32 - len(prefix) - len(total) - 1
			if maxName < 1 {
				maxName = 1
			}
			nameQty := prefix + trimField(item.Name, maxName)
			gap := 32 - len(nameQty) - len(total)
			if gap < 1 {
				gap = 1
			}
			writeln(nameQty + strings.Repeat(" ", gap) + total)
			// Qty × unit-price sub-line whenever qty ≠ 1 — the clearest way to show quantity
			// (matches the pos-ui client thermal renderer's existing pattern).
			if item.Quantity != 1 {
				writeln(fmt.Sprintf("   @ %s %.2f", d.Currency, item.Price))
			}
		}
		if item.Notes != "" {
			writeln(fmt.Sprintf("  * %s", item.Notes))
		}
	}

	// Totals section (customer receipts only)
	if d.Type == "customer" || d.Type == "void" {
		separator()
		taxLabel := "Tax"
		if d.VatRate > 0 {
			taxLabel = fmt.Sprintf("VAT (%g%%)", d.VatRate)
		}
		if d.TaxTotal > 0 {
			writeln(formatLine("Subtotal", fmt.Sprintf("%s %.2f", d.Currency, d.Subtotal)))
			writeln(formatLine(taxLabel, fmt.Sprintf("%s %.2f", d.Currency, d.TaxTotal)))
		}
		if d.DiscountTotal > 0 {
			writeln(formatLine("Discount", fmt.Sprintf("-%s %.2f", d.Currency, d.DiscountTotal)))
		}
		if d.ChargesTotal > 0 {
			writeln(formatLine("Charges", fmt.Sprintf("%s %.2f", d.Currency, d.ChargesTotal)))
		}
		if d.RoundOff > 0 {
			writeln(formatLine("Round Off", fmt.Sprintf("%s %.2f", d.Currency, d.RoundOff)))
		}
		// TOTAL in double-height+bold — the one figure a customer verifies at a glance.
		write(escDoubleH)
		write(escBold)
		writeln(formatLine("TOTAL", fmt.Sprintf("%s %.2f", d.Currency, d.TotalAmount)))
		write(escSizeReset)
		if d.PaymentMethod != "" {
			// Retail prints the settle date beside the method — "Payment  Cash (14-07-2026)".
			method := d.PaymentMethod
			if d.UseCase == "retail" && d.PaymentDate != nil {
				method = fmt.Sprintf("%s (%s)", method, d.PaymentDate.Format("02-01-2006"))
			}
			writeln(formatLine("Payment", method))
		}
		if d.UseCase == "retail" && d.AmountPaid > 0 {
			writeln(formatLine("Amount Paid", fmt.Sprintf("%s %.2f", d.Currency, d.AmountPaid)))
		}
		// Tendered duplicates Amount Paid on exact-settle retail receipts — print it only when it
		// actually differs (cash over-tender) or on the classic layout.
		if d.AmountTendered > 0 && !(d.UseCase == "retail" && d.AmountTendered == d.AmountPaid) {
			writeln(formatLine("Tendered", fmt.Sprintf("%s %.2f", d.Currency, d.AmountTendered)))
		}
		if d.ChangeDue > 0 {
			writeln(formatLine("Change", fmt.Sprintf("%s %.2f", d.Currency, d.ChangeDue)))
		}
		// Outstanding balance (on-account sale) or customer credit (negative) — printed so a
		// part-paid/credit sale receipt states what is still owed.
		if d.BalanceDue > 0.004 || d.BalanceDue < -0.004 {
			writeln(formatLine("Balance Due", fmt.Sprintf("%s %.2f", d.Currency, d.BalanceDue)))
		}
		// Customer's overall account position (store credit or amount owing), independent of
		// whether THIS sale was cash or credit.
		if d.CustomerAccountBalance != nil {
			writeln(formatLine(d.CustomerAccountBalanceLabel, fmt.Sprintf("%s %.2f", d.Currency, *d.CustomerAccountBalance)))
		}
	}

	if d.Type == "void" && d.VoidReason != "" {
		separator()
		writeln("Reason: " + d.VoidReason)
	}

	if d.Type == "customer" && d.PaymentMethods.HasAny() {
		separator()
		write(escCenter)
		write(escBold)
		writeln("HOW TO PAY")
		write(escLeft)
		pm := d.PaymentMethods
		if pm.MpesaPaybill != "" {
			writeln(formatLine("M-PESA Paybill", pm.MpesaPaybill))
		}
		if pm.MpesaAccountRef != "" {
			writeln(formatLine("Account No.", pm.MpesaAccountRef))
		}
		if pm.MpesaTill != "" {
			writeln(formatLine("M-PESA Till", pm.MpesaTill))
		}
		if pm.MpesaPochi != "" {
			writeln(formatLine("M-PESA Pochi", pm.MpesaPochi))
		}
		if pm.BankAccountNumber != "" {
			label := pm.BankName
			if label == "" {
				label = "Bank"
			}
			writeln(formatLine(label, pm.BankAccountNumber))
		}
		if pm.BankAccountName != "" {
			write(escCenter)
			writeln(pm.BankAccountName)
			write(escLeft)
		}
	}

	// KRA TIMS Details — the fiscal block, adapted from the KRA-issued paper ETR receipt
	// (see the Jazaribu Retail reference): SCU ID + CU Inv No, THEN the verification QR
	// immediately below, THEN (right after, no other content between) the fiscal barcode —
	// exactly the adjacency a genuine ETR receipt uses. The receipt SIGNATURE is deliberately
	// NEVER printed as plain text (it's already encoded in the QR; printing it in the clear
	// is an avoidable exposure of KRA-issued fiscal proof for no operator benefit).
	if d.Type == "customer" && (d.EtimsInvoiceNumber != "" || d.EtimsCuInvNo != "") {
		separator()
		write(escCenter)
		write(escBold)
		writeln("KRA TIMS Details")
		write(escLeft)
		if d.EtimsScuID != "" {
			writeln("SCU ID: " + d.EtimsScuID)
		}
		if d.EtimsCuInvNo != "" {
			writeln("CU Inv No.: " + d.EtimsCuInvNo)
		} else if d.EtimsInvoiceNumber != "" {
			writeln("CU#: " + d.EtimsInvoiceNumber)
		}
		// KRA verification QR — the scannable proof on compliant ETR paper receipts.
		appendEtimsQR(&buf, d.EtimsQRCodeURL, d.QRRaster)
	}

	// Fiscal barcode: the eTIMS CU Invoice Number once fiscalised, else the order number for
	// non-fiscalised retail sales (ReceiptView.FiscalBarcodeValue — one decision, every
	// surface). Printed immediately after the fiscal block, mirroring the ETR reference layout.
	if d.Type == "customer" && d.BarcodeValue != "" {
		buf.Write(escLF)
		write(escCenter)
		write([]byte{0x1D, 0x68, 60})                                  // GS h — barcode height (dots)
		write([]byte{0x1D, 0x77, 2})                                   // GS w — module width
		write([]byte{0x1D, 0x48, 2})                                   // GS H — HRI (the number) below the bars
		write([]byte{0x1D, 0x66, 0})                                   // GS f — HRI font A
		payload := append([]byte{'{', 'B'}, []byte(d.BarcodeValue)...) // CODE128 code set B
		write([]byte{0x1D, 0x6B, 73, byte(len(payload))})              // GS k m=73 (CODE128) n
		write(payload)
		buf.Write(escLF)
		write(escLeft)
	}

	if d.Type == "customer" {
		if d.Footer != "" {
			separator()
			write(escCenter)
			writeln(d.Footer)
			write(escLeft)
		}
		// Platform-owner (Codevertex) advertisement — always printed on customer receipts.
		pf := d.ProviderFooter.OrDefault()
		separator()
		write(escCenter)
		writeln(pf.Lead)
		writeln(pf.Contact)
		write(escLeft)
	}

	buf.Write(escLF)
	buf.Write(escLF)
	buf.Write(escLF)
	buf.Write(escCut)

	return buf.Bytes()
}

// BuildTestTicket renders a short ESC/POS diagnostic ticket used by the printer-setup
// "Test print" button. It carries no order — just enough to confirm the printer is wired,
// cutting correctly and reachable — so it can be dispatched silently (via the local agent or
// QZ) without opening the browser print dialog.
func BuildTestTicket(stationLabel, paper string, when time.Time) []byte {
	if when.IsZero() {
		when = time.Now()
	}
	var buf bytes.Buffer
	write := func(b []byte) { buf.Write(b) }
	writeln := func(s string) { buf.WriteString(s); buf.Write(escLF) }
	separator := func() { writeln(strings.Repeat("-", 32)) }

	write(escInit)
	write(escFontA)
	write(escBold) // whole-ticket emphasis — same legibility baseline as BuildReceipt
	write(escCenter)
	writeln("*** PRINTER TEST ***")
	write(escLeft)
	separator()
	if stationLabel != "" {
		writeln(formatLine("Station", trimField(stationLabel, 22)))
	}
	if paper != "" {
		writeln(formatLine("Paper", paper))
	}
	writeln(formatLine("Time", when.Format("02 Jan 2006 15:04")))
	separator()
	write(escCenter)
	writeln("If you can read this, the")
	writeln("printer is connected and")
	writeln("printing correctly.")
	write(escLeft)

	buf.Write(escLF)
	buf.Write(escLF)
	buf.Write(escLF)
	buf.Write(escCut)
	return buf.Bytes()
}

// trimField clamps a label to n runes so it never overflows the 32-char line.
func trimField(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// formatLine returns a 32-char wide label+value line with right-aligned value. The LABEL is
// clamped (via trimField) when label+value+1-space-gap would exceed the 32-col budget — labels
// are more often the long, freeform side (a long bank name, a long custom payment label) while
// values are short, so clamping the label avoids silently overflowing past column 32 and
// misaligning the printer's own physical line-wrap.
func formatLine(label, value string) string {
	if maxLabel := 32 - len(value) - 1; maxLabel > 0 && len(label) > maxLabel {
		label = trimField(label, maxLabel)
	}
	gap := 32 - len(label) - len(value)
	if gap < 1 {
		gap = 1
	}
	return label + strings.Repeat(" ", gap) + value
}
