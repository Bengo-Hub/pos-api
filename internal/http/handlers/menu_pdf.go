package handlers

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
)

// renderMenuPDF renders a true-PDF variant of the branded customer menu.
//
// It is the PDF sibling of generateMenuHTML (menu_render.go) and consumes the EXACT same
// inputs (groups assembled by CatalogHandler.assembleMenuItems, the receiptBrand, and the QR
// PNG bytes). It never re-fetches the catalog; the data path is owned by the handler.
//
// Layout (A4 portrait):
//   - a header band in the tenant primary colour with the tenant + outlet name (+ logo if any)
//   - a QR card ("Scan to view this menu") with the menu URL
//   - one section per category (heading + its icon — image icons are fetched/embedded, emoji
//     icons are drawn as text) followed by its item rows
//   - each item row: thumbnail (fetched/embedded, placeholder on failure), bold name, muted
//     description, right-aligned "KES <price>"
//
// All remote images (logo, category icons, item thumbnails) are best-effort: a fetch failure
// degrades to a placeholder and never aborts the document.
func renderMenuPDF(groups []menuGroup, brand receiptBrand, tenantName, outletName, menuURL string, qrPNG []byte) ([]byte, error) {
	const (
		pageW    = 210.0 // A4 width (mm)
		margin   = 14.0
		contentW = pageW - 2*margin
		leftX    = margin
		rightX   = pageW - margin
	)

	pal := newMenuPDFPalette(brand.PrimaryColor)

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetCompression(true) // zlib/FlateDecode the page content streams (image streams stay JPEG)
	pdf.SetMargins(margin, 14, margin)
	pdf.SetAutoPageBreak(true, 14)
	pdf.AddPage()

	img := newMenuImageFetcher(pdf)
	// Download + process every image the document references up front, concurrently. The render
	// loop below then registers each from memory, so a slow image can't serialise the whole
	// render into a 40s+ request that trips the gateway timeout.
	img.prefetch(collectMenuImageURLs(brand, groups))

	// ── Header band ───────────────────────────────────────────────────────────
	const bandH = 26.0
	bandY := 0.0
	pdf.SetFillColor(pal.primary.r, pal.primary.g, pal.primary.b)
	pdf.Rect(0, bandY, pageW, bandH, "F")

	// Logo (right side of the band, on a white rounded chip) if available.
	logoRight := rightX
	if logo := img.get(brand.LogoURL); logo.ok {
		const logoBox = 16.0
		chipX := rightX - logoBox - 2.0
		chipY := bandY + (bandH-logoBox)/2 - 1.0
		pdf.SetFillColor(255, 255, 255)
		pdf.RoundedRect(chipX-1.5, chipY-1.5, logoBox+3.0, logoBox+3.0, 2.0, "1234", "F")
		img.draw(logo, chipX, chipY, logoBox)
		logoRight = chipX - 4.0
	}

	pdf.SetTextColor(pal.onBrand.r, pal.onBrand.g, pal.onBrand.b)
	pdf.SetXY(leftX, bandY+6.0)
	pdf.SetFont("Helvetica", "B", 20)
	pdf.CellFormat(logoRight-leftX, 9.0, tr(pdf, ifBlank(tenantName, "Menu")), "", 2, "L", false, 0, "")
	if outletName != "" {
		pdf.SetFont("Helvetica", "", 11)
		pdf.CellFormat(logoRight-leftX, 6.0, tr(pdf, outletName), "", 2, "L", false, 0, "")
	}
	pdf.SetTextColor(34, 48, 63)
	pdf.SetY(bandH + 6.0)

	// ── QR card ───────────────────────────────────────────────────────────────
	if len(qrPNG) > 0 {
		drawMenuQRCard(pdf, img, pal, leftX, contentW, qrPNG, menuURL)
	}

	if len(groups) == 0 {
		pdf.Ln(8)
		pdf.SetFont("Helvetica", "I", 11)
		pdf.SetTextColor(pal.muted.r, pal.muted.g, pal.muted.b)
		pdf.CellFormat(contentW, 8, "No items are currently available on this menu.", "", 1, "C", false, 0, "")
	}

	// ── Category sections ─────────────────────────────────────────────────────
	for _, g := range groups {
		drawMenuCategoryHeading(pdf, img, pal, leftX, contentW, g)
		for _, it := range g.Items {
			drawMenuItemRow(pdf, img, pal, leftX, contentW, it)
		}
	}

	// ── Footer: live-prices note + Codevertex platform-owner marketing band ───────
	// Pinned to the bottom of the last page — strategic marketing real-estate for the Codevertex
	// POS suite / Power Suite on every menu a tenant shares or prints. Disable auto page-break so the
	// bottom-anchored block never spills onto an extra blank page.
	const footerH = 22.0
	if pdf.GetY() > menuPageBottom()-footerH {
		pdf.AddPage()
	}
	pdf.SetAutoPageBreak(false, 0)
	pdf.SetY(menuPageBottom() - footerH)

	pdf.SetFont("Helvetica", "I", 8)
	pdf.SetTextColor(pal.muted.r, pal.muted.g, pal.muted.b)
	pdf.CellFormat(contentW, 5, tr(pdf, "Menu generated live — prices in KES."), "", 1, "C", false, 0, "")

	// Hairline divider, then the platform-owner marketing lines.
	yDiv := pdf.GetY() + 1.5
	pdf.SetDrawColor(pal.border.r, pal.border.g, pal.border.b)
	pdf.SetLineWidth(0.3)
	pdf.Line(leftX+contentW*0.30, yDiv, leftX+contentW*0.70, yDiv)
	pdf.Ln(3.5)

	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetTextColor(pal.navy.r, pal.navy.g, pal.navy.b)
	pdf.CellFormat(contentW, 5, tr(pdf, "Powered by Codevertex POS — part of the Codevertex Power Suite"), "", 1, "C", false, 0, "")

	pdf.SetFont("Helvetica", "", 7.5)
	pdf.SetTextColor(pal.muted.r, pal.muted.g, pal.muted.b)
	pdf.CellFormat(contentW, 4, tr(pdf, "ERP · POS · TruLoad · ISP Billing · Books · AI & Automation"), "", 1, "C", false, 0, "")
	pdf.CellFormat(contentW, 4, tr(pdf, "www.codevertexitsolutions.com   ·   +254 742 201 368   ·   info@codevertexitsolutions.com"), "", 1, "C", false, 0, "")

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("render menu pdf: %w", err)
	}
	return buf.Bytes(), nil
}

// collectMenuImageURLs returns every remote image URL the PDF will embed — the brand logo, each
// category icon image, and each item thumbnail — so they can be prefetched concurrently.
func collectMenuImageURLs(brand receiptBrand, groups []menuGroup) []string {
	urls := make([]string, 0, len(groups)*8+1)
	if u := strings.TrimSpace(brand.LogoURL); u != "" {
		urls = append(urls, u)
	}
	for _, g := range groups {
		if u := strings.TrimSpace(g.ImageURL); u != "" {
			urls = append(urls, u)
		}
		for _, it := range g.Items {
			if u := strings.TrimSpace(it.ImageURL); u != "" {
				urls = append(urls, u)
			}
		}
	}
	return urls
}

// drawMenuQRCard renders the "Scan to view this menu" card: QR thumbnail (left), caption + URL.
func drawMenuQRCard(pdf *fpdf.Fpdf, img *menuImageFetcher, pal menuPDFPalette, leftX, contentW float64, qrPNG []byte, menuURL string) {
	const cardH = 30.0
	y := pdf.GetY()

	pdf.SetFillColor(pal.tint.r, pal.tint.g, pal.tint.b)
	pdf.SetDrawColor(pal.border.r, pal.border.g, pal.border.b)
	pdf.SetLineWidth(0.3)
	pdf.RoundedRect(leftX, y, contentW, cardH, 2.5, "1234", "FD")

	// QR: register the raw PNG bytes directly (no HTTP fetch — they are already in memory).
	const qrBox = 22.0
	qrX := leftX + 4.0
	qrY := y + (cardH-qrBox)/2
	pdf.SetFillColor(255, 255, 255)
	pdf.RoundedRect(qrX-1.5, qrY-1.5, qrBox+3.0, qrBox+3.0, 1.5, "1234", "F")
	info := pdf.RegisterImageOptionsReader("menuqr",
		fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(qrPNG))
	if info != nil && info.Width() > 0 {
		pdf.ImageOptions("menuqr", qrX, qrY, qrBox, qrBox, false,
			fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
	}

	textX := qrX + qrBox + 6.0
	textW := leftX + contentW - textX - 4.0
	pdf.SetXY(textX, y+8.0)
	pdf.SetFont("Helvetica", "B", 12)
	pdf.SetTextColor(pal.navy.r, pal.navy.g, pal.navy.b)
	pdf.CellFormat(textW, 6.0, tr(pdf, "Scan to view this menu"), "", 2, "L", false, 0, "")
	if menuURL != "" {
		pdf.SetFont("Helvetica", "", 8)
		pdf.SetTextColor(pal.muted.r, pal.muted.g, pal.muted.b)
		pdf.MultiCell(textW, 4.0, tr(pdf, menuURL), "", "L", false)
	}

	pdf.SetTextColor(34, 48, 63)
	pdf.SetY(y + cardH + 6.0)
}

// drawMenuCategoryHeading renders one category heading: a tinted bar with a brand left-rule,
// the category icon (image or emoji/text), and the category name.
func drawMenuCategoryHeading(pdf *fpdf.Fpdf, img *menuImageFetcher, pal menuPDFPalette, leftX, contentW float64, g menuGroup) {
	const headH = 9.0
	pdf.Ln(3)
	y := pdf.GetY()

	pdf.SetFillColor(pal.tint.r, pal.tint.g, pal.tint.b)
	pdf.Rect(leftX, y, contentW, headH, "F")
	// Brand left-rule.
	pdf.SetFillColor(pal.primary.r, pal.primary.g, pal.primary.b)
	pdf.Rect(leftX, y, 1.6, headH, "F")

	textX := leftX + 5.0
	if u := strings.TrimSpace(g.ImageURL); u != "" {
		if ci := img.get(u); ci.ok {
			const iconBox = 6.0
			img.draw(ci, leftX+4.0, y+(headH-iconBox)/2, iconBox)
			textX = leftX + 4.0 + iconBox + 2.0
		}
	} else if ic := strings.TrimSpace(g.Icon); ic != "" {
		// Emoji/text icon: drawn inline as text (fpdf core fonts can't render emoji glyphs,
		// but tr() degrades unsupported runes rather than failing).
		pdf.SetXY(leftX+4.0, y+1.0)
		pdf.SetFont("Helvetica", "B", 11)
		pdf.SetTextColor(pal.primary.r, pal.primary.g, pal.primary.b)
		pdf.CellFormat(7.0, headH-2.0, tr(pdf, ic), "", 0, "L", false, 0, "")
		textX = leftX + 12.0
	}

	pdf.SetXY(textX, y)
	pdf.SetFont("Helvetica", "B", 12)
	pdf.SetTextColor(pal.primary.r, pal.primary.g, pal.primary.b)
	pdf.CellFormat(leftX+contentW-textX, headH, tr(pdf, g.CategoryName), "", 1, "L", false, 0, "")
	pdf.SetTextColor(34, 48, 63)
	pdf.Ln(1.5)
}

// drawMenuItemRow renders one item: thumbnail (or placeholder), bold name + muted description,
// right-aligned KES price, and a hairline separator. Designed to avoid splitting across pages.
func drawMenuItemRow(pdf *fpdf.Fpdf, img *menuImageFetcher, pal menuPDFPalette, leftX, contentW float64, it catalogItemDTO) {
	const (
		thumb  = 16.0
		rowH   = 18.0
		priceW = 30.0
		gap    = 4.0
	)

	// Keep a row together: if it would split, break to a new page first.
	if pdf.GetY()+rowH > menuPageBottom() {
		pdf.AddPage()
	}
	y := pdf.GetY()

	// Thumbnail or placeholder.
	thumbY := y + (rowH-thumb)/2
	if im := img.get(it.ImageURL); im.ok {
		img.draw(im, leftX, thumbY, thumb)
	} else {
		pdf.SetFillColor(pal.placeholder.r, pal.placeholder.g, pal.placeholder.b)
		pdf.RoundedRect(leftX, thumbY, thumb, thumb, 1.5, "1234", "F")
	}

	bodyX := leftX + thumb + gap
	bodyW := contentW - thumb - gap - priceW - gap

	// Name (bold).
	pdf.SetXY(bodyX, y+2.0)
	pdf.SetFont("Helvetica", "B", 11)
	pdf.SetTextColor(pal.navy.r, pal.navy.g, pal.navy.b)
	pdf.CellFormat(bodyW, 5.0, tr(pdf, it.Name), "", 2, "L", false, 0, "")

	// Description (muted, wrapped, max 2 lines).
	if d := strings.TrimSpace(it.Description); d != "" {
		pdf.SetFont("Helvetica", "", 8.5)
		pdf.SetTextColor(pal.muted.r, pal.muted.g, pal.muted.b)
		pdf.SetX(bodyX)
		pdf.MultiCell(bodyW, 3.8, tr(pdf, clampLines(d, 160)), "", "L", false)
	}

	// Price (right-aligned, vertically centred on the row).
	pdf.SetXY(leftX+contentW-priceW, y+(rowH/2)-3.0)
	pdf.SetFont("Helvetica", "B", 11)
	pdf.SetTextColor(pal.navy.r, pal.navy.g, pal.navy.b)
	pdf.CellFormat(priceW, 6.0, tr(pdf, "KES "+formatKES(it.Price)), "", 0, "R", false, 0, "")

	// Advance to the row bottom + hairline separator.
	nextY := y + rowH
	pdf.SetDrawColor(238, 241, 245)
	pdf.SetLineWidth(0.2)
	pdf.Line(leftX, nextY, leftX+contentW, nextY)
	pdf.SetTextColor(34, 48, 63)
	pdf.SetY(nextY + 1.0)
}

// menuPageBottom is the A4 page-break threshold (page height minus the bottom margin).
func menuPageBottom() float64 { return 297.0 - 14.0 }

// tr translates a UTF-8 string into the PDF's core-font encoding (cp1252) so accented/symbol
// characters render instead of corrupting. Unsupported runes (e.g. emoji) are dropped.
func tr(pdf *fpdf.Fpdf, s string) string {
	return pdf.UnicodeTranslatorFromDescriptor("")(s)
}

// ifBlank returns def when s is empty after trimming.
func ifBlank(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// clampLines hard-truncates a description so a long blurb cannot blow out the row height.
func clampLines(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max-1]) + "…"
}
