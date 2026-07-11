package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"

	"github.com/bengobox/pos-service/internal/modules/printing"
)

// generateReceiptPDF renders an 80mm thermal-width PDF receipt from the receipt response, applying
// tenant branding (logo + primary colour). Mirrors the layout of generateReceiptHTML.
func generateReceiptPDF(rec receiptResponse, brand receiptBrand) ([]byte, error) {
	const pageW = 80.0 // 80mm thermal paper
	const margin = 4.0
	contentW := pageW - 2*margin

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

	// Header — tenant logo (centered) + company name in brand colour
	if logo, lt := fetchReceiptLogo(brand.LogoURL); logo != nil {
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
		center(rec.OutletAddress, "", 8)
	}
	pdf.Ln(1)
	hr()

	// Meta
	line("Receipt:", rec.ReceiptNumber, "", 8)
	line("Order:", rec.OrderNumber, "", 8)
	line("Date:", rec.IssuedAt.Format("2006-01-02 15:04"), "", 8)
	if rec.BillTo != "" {
		custLabel := rec.BillToLabel
		if custLabel == "" {
			custLabel = "Customer"
		}
		line(custLabel+":", rec.BillTo, "", 8)
	}
	if rec.ServedBy != "" {
		line("Served by:", rec.ServedBy, "", 8)
	}
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
	if rec.ChargesTotal > 0 {
		line("Charges", money(rec.ChargesTotal), "", 8)
	}
	if rec.RoundOff > 0 {
		line("Round Off", money(rec.RoundOff), "", 8)
	}
	line("TOTAL", money(rec.TotalAmount), "B", 12)
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

	// Payment display ("HOW TO PAY") — M-Pesa/bank details, same block as the HTML/ESC-POS receipts.
	if pm := rec.PaymentMethods; pm != nil {
		hr()
		center("HOW TO PAY", "B", 8)
		if pm.MpesaPaybill != "" {
			line("M-PESA Paybill", pm.MpesaPaybill, "", 8)
		}
		if pm.MpesaAccountRef != "" {
			line("Account No.", pm.MpesaAccountRef, "", 8)
		}
		if pm.MpesaTill != "" {
			line("M-PESA Till", pm.MpesaTill, "", 8)
		}
		if pm.MpesaPochi != "" {
			line("M-PESA Pochi", pm.MpesaPochi, "", 8)
		}
		if pm.BankName != "" || pm.BankAccountNumber != "" {
			label := pm.BankName
			if label == "" {
				label = "Bank"
			}
			line(label, pm.BankAccountNumber, "", 8)
		}
		if pm.BankAccountName != "" {
			center(pm.BankAccountName, "", 7)
		}
	}

	// Footer
	if rec.ReceiptFooter != "" {
		pdf.Ln(2)
		center(rec.ReceiptFooter, "I", 8)
	}

	// Platform-owner (Codevertex) advertisement — always shown.
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
	pdf.Ln(1)
	hr()
	center(lead, "B", 7.5)
	center(contact, "", 6.8)

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

// receiptHexToRGB parses "#RRGGBB" to RGB ints; returns the default ink colour on failure.
func receiptHexToRGB(hex string) (int, int, int) {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) == 6 {
		if r, err := strconv.ParseInt(hex[0:2], 16, 0); err == nil {
			if g, err := strconv.ParseInt(hex[2:4], 16, 0); err == nil {
				if b, err := strconv.ParseInt(hex[4:6], 16, 0); err == nil {
					return int(r), int(g), int(b)
				}
			}
		}
	}
	return 34, 48, 63 // default ink
}

// brandImageHTTPClient is a shared, connection-pooling client for downloading branding/menu
// images. A menu render fetches ~50 images; reusing keep-alive connections (instead of a fresh
// client + TLS handshake per image) and a tighter timeout keeps the render fast and bounded.
// It is safe for concurrent use (see menuImageFetcher.prefetch).
var brandImageHTTPClient = &http.Client{
	Timeout: 4 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 3 * time.Second,
	},
}

// fetchReceiptLogo best-effort downloads a logo/menu image (PNG/JPG); returns nil on any failure.
func fetchReceiptLogo(url string) ([]byte, string) {
	if url == "" {
		return nil, ""
	}
	resp, err := brandImageHTTPClient.Get(url) //nolint:noctx
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ""
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(&io.LimitedReader{R: resp.Body, N: 5 << 20}); err != nil {
		return nil, ""
	}
	// Determine the encoding from the ACTUAL BYTES, not the HTTP Content-Type / file
	// extension. Logos are frequently mislabeled (e.g. a JPEG uploaded as "logo.png" and
	// served with Content-Type: image/png). fpdf rejects a declared-type/real-bytes
	// mismatch with "not a PNG buffer", which poisons the whole document and fails Output.
	// http.DetectContentType sniffs the leading magic bytes, so the type always matches.
	switch ct := http.DetectContentType(buf.Bytes()); {
	case strings.Contains(ct, "png"):
		return buf.Bytes(), "PNG"
	case strings.Contains(ct, "jpeg"), strings.Contains(ct, "jpg"):
		return buf.Bytes(), "JPG"
	default:
		return nil, ""
	}
}
