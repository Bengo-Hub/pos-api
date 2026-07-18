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
	qrcode "github.com/skip2/go-qrcode"

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

	// Header — tenant logo (centered, honours the "show logo" receipt setting) + company name
	if logo, lt := fetchReceiptLogo(brand.LogoURL); logo != nil && rec.ShowLogo {
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
		center(rec.OutletAddress, "", 9)
	}
	if rec.OutletPhones != "" {
		center("Mobile: "+rec.OutletPhones, "", 9)
	}
	if rec.EtimsKraPin != "" {
		center("KRA PIN: "+rec.EtimsKraPin, "B", 9)
	}
	pdf.Ln(1)
	hr()

	// Meta
	line("Receipt:", rec.ReceiptNumber, "", 9)
	line("Order:", rec.OrderNumber, "", 9)
	line("Date:", rec.IssuedAt.Format("2006-01-02 15:04"), "", 9)
	if rec.BillTo != "" {
		custLabel := rec.BillToLabel
		if custLabel == "" {
			custLabel = "Customer"
		}
		line(custLabel+":", rec.BillTo, "", 9)
	}
	if rec.ServedBy != "" {
		line("Served by:", rec.ServedBy, "", 9)
	}
	hr()

	// Line items
	pdf.SetFont("Helvetica", "B", 9)
	pdf.CellFormat(contentW*0.5, 4, "Item", "", 0, "L", false, 0, "")
	pdf.CellFormat(contentW*0.2, 4, "Qty", "", 0, "R", false, 0, "")
	pdf.CellFormat(contentW*0.3, 4, "Total", "", 1, "R", false, 0, "")
	for _, l := range rec.Lines {
		pdf.SetFont("Helvetica", "", 9)
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
	line("Subtotal", money(rec.Subtotal), "", 9)
	if rec.DiscountAmount > 0 {
		line("Discount", "-"+money(rec.DiscountAmount), "", 9)
	}
	if rec.VatEnabled || rec.TaxAmount > 0 {
		taxLabel := "Tax"
		if rec.VatRate > 0 {
			taxLabel = fmt.Sprintf("VAT (%.0f%%)", rec.VatRate)
		}
		line(taxLabel, money(rec.TaxAmount), "", 9)
	}
	if rec.ChargesTotal > 0 {
		line("Charges", money(rec.ChargesTotal), "", 9)
	}
	if rec.RoundOff > 0 {
		line("Round Off", money(rec.RoundOff), "", 9)
	}
	line("TOTAL", money(rec.TotalAmount), "B", 12)
	hr()

	// Payment
	if rec.PaymentMethod != "" {
		line("Paid via", strings.ReplaceAll(rec.PaymentMethod, "_", " "), "", 9)
	}
	if rec.AmountPaid > 0 {
		line("Amount paid", money(rec.AmountPaid), "", 9)
	}
	if rec.AmountTendered > 0 {
		line("Tendered", money(rec.AmountTendered), "", 9)
	}
	if rec.ChangeDue > 0 {
		line("Change", money(rec.ChangeDue), "", 9)
	}

	// KRA TIMS Details (fiscalised sales) — mirrors the paper ETR layout.
	if rec.EtimsInvoiceNumber != "" || rec.EtimsCuInvNo != "" {
		hr()
		center("KRA TIMS Details", "B", 9)
		if rec.EtimsScuID != "" {
			line("SCU ID", rec.EtimsScuID, "", 8)
		}
		if rec.EtimsCuInvNo != "" {
			line("CU Inv No.", rec.EtimsCuInvNo, "", 8)
		} else if rec.EtimsInvoiceNumber != "" {
			line("eTIMS Inv", rec.EtimsInvoiceNumber, "", 8)
		}
		if rec.EtimsRcptSign != "" {
			line("Sign", truncate(rec.EtimsRcptSign, 34), "", 7)
		}
		if rec.EtimsQRCodeURL != "" {
			if qrPNG, qerr := qrcode.Encode(rec.EtimsQRCodeURL, qrcode.Medium, 256); qerr == nil {
				const qrW = 18.0
				if info := pdf.RegisterImageOptionsReader("etimsqr", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(qrPNG)); info != nil && info.Width() > 0 {
					pdf.ImageOptions("etimsqr", (pageW-qrW)/2, pdf.GetY()+1, qrW, qrW, true, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
				} else {
					pdf.ClearError()
				}
			}
		}
	}

	// Payment display ("HOW TO PAY") — M-Pesa/bank details, same block as the HTML/ESC-POS receipts.
	if pm := rec.PaymentMethods; pm != nil {
		hr()
		center("HOW TO PAY", "B", 9)
		if pm.MpesaPaybill != "" {
			line("M-PESA Paybill", pm.MpesaPaybill, "", 9)
		}
		if pm.MpesaAccountRef != "" {
			line("Account No.", pm.MpesaAccountRef, "", 9)
		}
		if pm.MpesaTill != "" {
			line("M-PESA Till", pm.MpesaTill, "", 9)
		}
		if pm.MpesaPochi != "" {
			line("M-PESA Pochi", pm.MpesaPochi, "", 9)
		}
		if pm.BankName != "" || pm.BankAccountNumber != "" {
			label := pm.BankName
			if label == "" {
				label = "Bank"
			}
			line(label, pm.BankAccountNumber, "", 9)
		}
		if pm.BankAccountName != "" {
			center(pm.BankAccountName, "", 8)
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
	// Never below ~8pt — sub-7pt text disappears on low-quality/low-toner printers.
	pdf.Ln(1)
	hr()
	center(lead, "B", 8.2)
	center(contact, "", 7.6)

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
