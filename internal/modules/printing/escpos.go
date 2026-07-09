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
)

// ReceiptData holds all data needed to render a receipt.
type ReceiptData struct {
	Type               string // "customer", "kitchen_ticket", "waiter_copy", "void"
	OutletName         string
	OutletAddress      string
	OrderNumber        string
	BillTo             string
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
	AmountTendered     float64
	ChangeDue          float64
	Currency           string
	EtimsInvoiceNumber string
	PaymentMethods     *ReceiptPaymentMethods // "HOW TO PAY" block (M-Pesa/bank), customer receipts only
	VoidReason         string
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
	write(escBold)

	if d.OutletName != "" {
		writeln(d.OutletName)
	} else {
		writeln("RECEIPT")
	}
	write(escBoldOff)
	if d.OutletAddress != "" {
		writeln(d.OutletAddress)
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
		writeln(fmt.Sprintf("Customer: %s", d.BillTo))
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
		write(escBold)
		writeln(formatLine("TOTAL", fmt.Sprintf("%s %.2f", d.Currency, d.TotalAmount)))
		write(escBoldOff)
		if d.PaymentMethod != "" {
			writeln(formatLine("Payment", d.PaymentMethod))
		}
		if d.AmountTendered > 0 {
			writeln(formatLine("Tendered", fmt.Sprintf("%s %.2f", d.Currency, d.AmountTendered)))
		}
		if d.ChangeDue > 0 {
			writeln(formatLine("Change", fmt.Sprintf("%s %.2f", d.Currency, d.ChangeDue)))
		}
	}

	if d.Type == "void" && d.VoidReason != "" {
		separator()
		writeln("Reason: " + d.VoidReason)
	}

	if d.Type == "customer" && d.EtimsInvoiceNumber != "" {
		separator()
		write(escCenter)
		writeln("KRA eTIMS Invoice")
		writeln("CU#: " + d.EtimsInvoiceNumber)
		write(escLeft)
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

	if d.Footer != "" && d.Type == "customer" {
		separator()
		write(escCenter)
		writeln(d.Footer)
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
