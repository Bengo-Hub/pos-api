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
	escInit    = []byte{0x1B, 0x40}          // Initialize printer
	escCut     = []byte{0x1D, 0x56, 0x42, 0} // Full cut
	escBold    = []byte{0x1B, 0x45, 0x01}    // Bold on
	escBoldOff = []byte{0x1B, 0x45, 0x00}    // Bold off
	escCenter  = []byte{0x1B, 0x61, 0x01}    // Center align
	escLeft    = []byte{0x1B, 0x61, 0x00}    // Left align
	escLF      = []byte{0x0A}                // Line feed
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
	Type               string // "customer", "kitchen_ticket", "waiter_copy", "void"
	OutletName         string
	OutletAddress      string
	OutletPhones       string // formatted labeled phones, printed as "Mobile: …" under the address
	OrderNumber        string
	BillTo             string
	BillToLabel        string // "Customer" or "Paid by"
	ServedBy           string
	TableRef           string
	DateTime           time.Time
	Header             string // custom header text from OutletSetting
	Footer             string // custom footer text from OutletSetting
	Items              []ReceiptItem
	Subtotal           float64
	TaxTotal           float64
	VatRate            float64 // percentage, e.g. 16 — 0 means "unknown", falls back to the plain "Tax" label
	DiscountTotal      float64
	ChargesTotal       float64
	RoundOff           float64
	TotalAmount        float64
	PaymentMethod      string
	PaymentDate        *time.Time // when the payment settled — retail prints it beside the method
	AmountPaid         float64
	BalanceDue         float64 // total − paid; printed when non-zero (on-account / customer credit)
	AmountTendered     float64
	ChangeDue          float64
	Currency           string
	EtimsInvoiceNumber string
	// Fiscal identity ("KRA TIMS Details" block, mirroring paper ETR receipts).
	EtimsKraPin   string // printed in the business header as "KRA PIN: …"
	EtimsScuID    string // "SCU ID" line
	EtimsCuInvNo  string // "CU Inv No." line — {SCU ID}/{receipt no}
	EtimsRcptSign string
	PaymentMethods     *ReceiptPaymentMethods // "HOW TO PAY" block (M-Pesa/bank), customer receipts only
	VoidReason         string
	ProviderFooter     ProviderFooter // platform-owner (Codevertex) advertisement, customer receipts only
	// UseCase — "retail" additionally prints a native Code 128 barcode of the order number
	// and the payment date beside the method (the BOI/GoDigital receipt design).
	UseCase string
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
	write(escCenter)
	// Shop name in double-height+bold — the biggest, crispest thing on the receipt so it stays
	// legible on low-DPI thermal heads (addresses the "blurry receipt" complaint).
	write(escDoubleHW)
	write(escBold)
	if d.OutletName != "" {
		writeln(d.OutletName)
	} else {
		writeln("RECEIPT")
	}
	write(escBoldOff)
	write(escSizeReset)
	if d.OutletAddress != "" {
		writeln(d.OutletAddress)
	}
	if d.OutletPhones != "" {
		writeln("Mobile: " + d.OutletPhones)
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
		write(escBoldOff)
		write(escLeft)
	case "waiter_copy":
		write(escCenter)
		write(escBold)
		writeln("** WAITER COPY **")
		write(escBoldOff)
		write(escLeft)
	case "void":
		write(escCenter)
		write(escBold)
		writeln("** VOID RECEIPT **")
		write(escBoldOff)
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
		nameQty := fmt.Sprintf("%-3s %s", qty, item.Name)
		if d.Type == "kitchen_ticket" || d.Type == "waiter_copy" {
			// Kitchen/waiter tickets show no prices
			writeln(nameQty)
		} else {
			total := fmt.Sprintf("%s %.2f", d.Currency, item.Total)
			// Right-align price in 32-char width
			gap := 32 - len(nameQty) - len(total)
			if gap < 1 {
				gap = 1
			}
			writeln(nameQty + strings.Repeat(" ", gap) + total)
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
		write(escBoldOff)
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
	}

	if d.Type == "void" && d.VoidReason != "" {
		separator()
		writeln("Reason: " + d.VoidReason)
	}

	if d.Type == "customer" && (d.EtimsInvoiceNumber != "" || d.EtimsCuInvNo != "") {
		separator()
		write(escCenter)
		write(escBold)
		writeln("KRA TIMS Details")
		write(escBoldOff)
		write(escLeft)
		if d.EtimsScuID != "" {
			writeln("SCU ID: " + d.EtimsScuID)
		}
		if d.EtimsCuInvNo != "" {
			writeln("CU Inv No.: " + d.EtimsCuInvNo)
		} else if d.EtimsInvoiceNumber != "" {
			writeln("CU#: " + d.EtimsInvoiceNumber)
		}
		if d.EtimsRcptSign != "" {
			writeln("Sign: " + d.EtimsRcptSign)
		}
	}

	if d.Type == "customer" && d.PaymentMethods.HasAny() {
		separator()
		write(escCenter)
		write(escBold)
		writeln("HOW TO PAY")
		write(escBoldOff)
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

	// Retail customer receipts print a native Code 128 barcode of the order number (GS k) —
	// scannable for returns/lookups, mirroring the barcode on the HTML/PDF retail template.
	if d.Type == "customer" && d.UseCase == "retail" && d.OrderNumber != "" {
		buf.Write(escLF)
		write(escCenter)
		write([]byte{0x1D, 0x68, 60})   // GS h — barcode height (dots)
		write([]byte{0x1D, 0x77, 2})    // GS w — module width
		write([]byte{0x1D, 0x48, 2})    // GS H — HRI (the number) below the bars
		write([]byte{0x1D, 0x66, 0})    // GS f — HRI font A
		payload := append([]byte{'{', 'B'}, []byte(d.OrderNumber)...) // CODE128 code set B
		write([]byte{0x1D, 0x6B, 73, byte(len(payload))})             // GS k m=73 (CODE128) n
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
	write(escCenter)
	write(escBold)
	writeln("*** PRINTER TEST ***")
	write(escBoldOff)
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

// formatLine returns a 32-char wide label+value line with right-aligned value.
func formatLine(label, value string) string {
	gap := 32 - len(label) - len(value)
	if gap < 1 {
		gap = 1
	}
	return label + strings.Repeat(" ", gap) + value
}
