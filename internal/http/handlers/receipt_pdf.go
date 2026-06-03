package handlers

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
)

// generateReceiptPDF renders an 80mm thermal-width PDF receipt from the receipt response.
// Returns the PDF bytes, or an error if rendering fails. Mirrors the layout of generateReceiptHTML.
func generateReceiptPDF(rec receiptResponse) ([]byte, error) {
	const pageW = 80.0 // 80mm thermal paper
	const margin = 4.0
	contentW := pageW - 2*margin

	pdf := fpdf.NewCustom(&fpdf.InitType{
		UnitStr: "mm",
		Size:    fpdf.SizeType{Wd: pageW, Ht: 297},
	})
	pdf.SetMargins(margin, 6, margin)
	pdf.SetAutoPageBreak(true, 6)
	pdf.AddPage()

	currency := rec.Currency
	if currency == "" {
		currency = "KES"
	}
	money := func(v float64) string { return fmt.Sprintf("%s %0.2f", currency, v) }

	center := func(s string, style string, size float64) {
		pdf.SetFont("Helvetica", style, size)
		pdf.MultiCell(contentW, size*0.5+1.0, s, "", "C", false)
	}
	line := func(label, value string, style string, size float64) {
		pdf.SetFont("Helvetica", style, size)
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

	// Header
	if rec.ReceiptHeader != "" {
		center(rec.ReceiptHeader, "B", 9)
	}
	if rec.OutletName != "" {
		center(rec.OutletName, "B", 11)
	}
	if rec.OutletAddress != "" {
		center(rec.OutletAddress, "", 8)
	}
	pdf.Ln(1)
	hr()

	// Meta
	line("Receipt:", rec.ReceiptNumber, "", 8)
	line("Order:", rec.OrderNumber, "", 8)
	line("Date:", rec.IssuedAt.Format("2006-01-02 15:04"), "", 8)
	hr()

	// Line items
	pdf.SetFont("Helvetica", "B", 8)
	pdf.CellFormat(contentW*0.5, 4, "Item", "", 0, "L", false, 0, "")
	pdf.CellFormat(contentW*0.2, 4, "Qty", "", 0, "R", false, 0, "")
	pdf.CellFormat(contentW*0.3, 4, "Total", "", 1, "R", false, 0, "")
	for _, l := range rec.Lines {
		pdf.SetFont("Helvetica", "", 8)
		name := l.Name
		if name == "" {
			name = l.SKU
		}
		pdf.CellFormat(contentW*0.5, 4, truncate(name, 24), "", 0, "L", false, 0, "")
		pdf.CellFormat(contentW*0.2, 4, fmt.Sprintf("%.0f", l.Quantity), "", 0, "R", false, 0, "")
		pdf.CellFormat(contentW*0.3, 4, fmt.Sprintf("%0.2f", l.TotalPrice), "", 1, "R", false, 0, "")
	}
	hr()

	// Totals
	line("Subtotal", money(rec.Subtotal), "", 8)
	if rec.DiscountAmount > 0 {
		line("Discount", "-"+money(rec.DiscountAmount), "", 8)
	}
	if rec.VatEnabled || rec.TaxAmount > 0 {
		taxLabel := "Tax"
		if rec.VatRate > 0 {
			taxLabel = fmt.Sprintf("VAT (%.0f%%)", rec.VatRate)
		}
		line(taxLabel, money(rec.TaxAmount), "", 8)
	}
	line("TOTAL", money(rec.TotalAmount), "B", 10)
	hr()

	// Payment
	if rec.PaymentMethod != "" {
		line("Paid via", strings.ReplaceAll(rec.PaymentMethod, "_", " "), "", 8)
	}
	if rec.AmountPaid > 0 {
		line("Amount paid", money(rec.AmountPaid), "", 8)
	}
	if rec.AmountTendered > 0 {
		line("Tendered", money(rec.AmountTendered), "", 8)
	}
	if rec.ChangeDue > 0 {
		line("Change", money(rec.ChangeDue), "", 8)
	}

	// eTIMS
	if rec.EtimsInvoiceNumber != "" {
		hr()
		line("eTIMS Inv", rec.EtimsInvoiceNumber, "", 7)
	}

	// Footer
	if rec.ReceiptFooter != "" {
		pdf.Ln(2)
		center(rec.ReceiptFooter, "I", 8)
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("render receipt pdf: %w", err)
	}
	return buf.Bytes(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
