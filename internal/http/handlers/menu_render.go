package handlers

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"

	// NOTE FOR INTEGRATOR: this dependency must be added to go.mod.
	// Run:  go get github.com/skip2/go-qrcode@v0.0.0-20200617195104-da1b6568686e
	// (then `go mod tidy`). It is a tiny, dependency-free pure-Go QR encoder.
	qrcode "github.com/skip2/go-qrcode"
)

// publicMenuURL builds the absolute URL the QR code should encode. It prefers the
// PUBLIC_BASE_URL env var (the externally reachable origin, e.g. https://app.example.com)
// so the QR works even when the service sits behind a reverse proxy / ingress that rewrites
// Host. Falls back to reconstructing the origin from the inbound request.
func publicMenuURL(r *http.Request) string {
	path := r.URL.RequestURI() // includes path + raw query
	if base := strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"); base != "" {
		return base + path
	}
	scheme := "https"
	// Respect a proxy-supplied scheme; default to https for QR targets.
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	} else if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		host = xfh
	}
	return fmt.Sprintf("%s://%s%s", scheme, host, path)
}

// qrPNGBytes encodes content as a QR PNG and returns the raw PNG bytes. size is the PNG edge
// length in pixels. Used by the PDF renderer (fpdf embeds raw bytes via an io.Reader); the HTML
// renderer wraps these bytes in a data: URI via qrPNGDataURI.
func qrPNGBytes(content string, size int) ([]byte, error) {
	if size <= 0 {
		size = 256
	}
	return qrcode.Encode(content, qrcode.Medium, size)
}

// qrPNGDataURI encodes content as a QR PNG and returns a base64 data: URI suitable for
// embedding directly in an <img src>. size is the PNG edge length in pixels.
func qrPNGDataURI(content string, size int) (string, error) {
	png, err := qrPNGBytes(content, size)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

// menuPalette holds the CSS colour strings derived from the tenant primary colour.
// It mirrors the idea in treasury-api docs/render_palette.go (brand-derived tints) but in
// plain CSS hex — we do NOT import that package (different service).
type menuPalette struct {
	Primary string // tenant primary (header band)
	OnBrand string // text colour on the header band (white/dark for contrast)
	Tint    string // very light brand tint (category heading background)
	Border  string // light brand-tinted hairline
}

// defaultBrandHex matches the platform document default (indigo) used when a tenant has no
// primary colour, consistent with treasury's defaultBrand{99,102,241}.
const defaultBrandHex = "#6366f1"

// buildMenuPalette derives a small CSS palette from the tenant primary hex.
func buildMenuPalette(primaryHex string) menuPalette {
	primary := strings.TrimSpace(primaryHex)
	if !looksLikeHex(primary) {
		primary = defaultBrandHex
	}
	r, g, b, ok := hexToRGB(primary)
	if !ok {
		r, g, b = 99, 102, 241
		primary = defaultBrandHex
	}
	return menuPalette{
		Primary: primary,
		OnBrand: contrastText(r, g, b),
		Tint:    rgbToHex(lightenChannel(r, 0.90), lightenChannel(g, 0.90), lightenChannel(b, 0.90)),
		Border:  rgbToHex(lightenChannel(r, 0.78), lightenChannel(g, 0.78), lightenChannel(b, 0.78)),
	}
}

// generateMenuHTML renders the branded, print-friendly customer menu document.
//
//   - header band in the tenant primary colour with tenant + outlet name
//   - QR code ("Scan to view this menu") near the top
//   - one section per category (category heading), each item: image, bold name,
//     muted description, right-aligned KES price
//   - responsive, A4-ish, white background, legible; print CSS hides nothing
func generateMenuHTML(groups []menuGroup, brand receiptBrand, tenantName, outletName, menuURL, qrDataURI string) []byte {
	pal := buildMenuPalette(brand.PrimaryColor)

	var buf bytes.Buffer
	buf.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	buf.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	buf.WriteString(`<title>` + htmlEscape(tenantName) + ` — Menu</title>`)
	buf.WriteString(fmt.Sprintf(`<style>
:root{--brand:%s;--on-brand:%s;--tint:%s;--brand-border:%s}
@page{size:A4;margin:12mm}
*{box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
  margin:0;background:#fff;color:#22303f;-webkit-print-color-adjust:exact;print-color-adjust:exact}
.page{max-width:820px;margin:0 auto;padding:0 16px 48px}
.brandband{background:var(--brand);color:var(--on-brand);padding:24px 20px;border-radius:0 0 14px 14px;
  display:flex;justify-content:space-between;align-items:center;gap:16px}
.brandband .titles{min-width:0}
.brandband .logo{height:48px;width:auto;background:#fff;border-radius:8px;padding:4px;flex:none}
.tenant{font-size:26px;font-weight:800;line-height:1.1;margin:0;word-break:break-word}
.outlet{font-size:15px;font-weight:600;opacity:.9;margin:4px 0 0}
.qrcard{display:flex;align-items:center;gap:14px;margin:18px 0 8px;padding:12px 16px;
  border:1px solid var(--brand-border);border-radius:12px;background:var(--tint)}
.qrcard img{width:96px;height:96px;flex:none;background:#fff;border-radius:8px;padding:4px}
.qrcard .cap{font-size:14px;font-weight:600}
.qrcard .url{font-size:11px;color:#6b7a90;word-break:break-all;margin-top:2px}
.cat{margin:28px 0 10px}
.cat h2{font-size:18px;font-weight:800;color:var(--brand);margin:0;padding:8px 12px;
  background:var(--tint);border-radius:8px;border-left:4px solid var(--brand);display:flex;align-items:center;gap:8px}
.cat-icon-img{width:22px;height:22px;object-fit:contain;border-radius:4px}
.cat-icon{font-size:18px;line-height:1}
.item{display:flex;gap:14px;padding:12px 4px;border-bottom:1px solid #eef1f5;page-break-inside:avoid}
.item .thumb{width:72px;height:72px;flex:none;border-radius:10px;object-fit:cover;background:#f2f4f7}
.item .body{flex:1;min-width:0}
.item .name{font-size:15px;font-weight:700;margin:0}
.item .desc{font-size:13px;color:#6b7a90;margin:3px 0 0;line-height:1.35}
.item .price{font-size:15px;font-weight:800;white-space:nowrap;text-align:right;flex:none;align-self:flex-start}
.empty{padding:40px 0;text-align:center;color:#6b7a90}
.footer{margin-top:36px;text-align:center;color:#9aa6b6;font-size:12px}
@media print{.page{max-width:none;padding:0}.brandband{border-radius:0}}
</style></head><body><div class="page">`,
		pal.Primary, pal.OnBrand, pal.Tint, pal.Border))

	// Header band: tenant + outlet name (+ logo if available).
	buf.WriteString(`<div class="brandband"><div class="titles">`)
	buf.WriteString(`<p class="tenant">` + htmlEscape(tenantName) + `</p>`)
	if outletName != "" {
		buf.WriteString(`<p class="outlet">` + htmlEscape(outletName) + `</p>`)
	}
	buf.WriteString(`</div>`)
	if brand.LogoURL != "" {
		buf.WriteString(`<img class="logo" src="` + htmlAttr(brand.LogoURL) + `" alt="logo">`)
	}
	buf.WriteString(`</div>`) // .brandband

	// QR card: "Scan to view this menu".
	if qrDataURI != "" {
		buf.WriteString(`<div class="qrcard">`)
		buf.WriteString(`<img src="` + htmlAttr(qrDataURI) + `" alt="Scan to view this menu">`)
		buf.WriteString(`<div><div class="cap">Scan to view this menu</div>`)
		if menuURL != "" {
			buf.WriteString(`<div class="url">` + htmlEscape(menuURL) + `</div>`)
		}
		buf.WriteString(`</div></div>`)
	}

	if len(groups) == 0 {
		buf.WriteString(`<p class="empty">No items are currently available on this menu.</p>`)
	}

	for _, g := range groups {
		buf.WriteString(`<section class="cat"><h2>` + menuCategoryIconHTML(g) + htmlEscape(g.CategoryName) + `</h2></section>`)
		for _, it := range g.Items {
			buf.WriteString(`<div class="item">`)
			if it.ImageURL != "" {
				buf.WriteString(`<img class="thumb" src="` + htmlAttr(it.ImageURL) + `" alt="" loading="lazy">`)
			} else {
				buf.WriteString(`<div class="thumb"></div>`)
			}
			buf.WriteString(`<div class="body">`)
			buf.WriteString(`<p class="name">` + htmlEscape(it.Name) + `</p>`)
			if it.Description != "" {
				buf.WriteString(`<p class="desc">` + htmlEscape(it.Description) + `</p>`)
			}
			buf.WriteString(`</div>`)
			buf.WriteString(fmt.Sprintf(`<div class="price">KES %s</div>`, formatKES(it.Price)))
			buf.WriteString(`</div>`) // .item
		}
	}

	buf.WriteString(`<p class="footer">Menu generated live — prices in KES.</p>`)
	buf.WriteString(`</div></body></html>`)
	return buf.Bytes()
}

// menuCategoryIconHTML renders a category's icon for its heading: <img> for an image-URL icon,
// an inline span for an emoji/text icon, or empty when absent.
func menuCategoryIconHTML(g menuGroup) string {
	if u := strings.TrimSpace(g.ImageURL); u != "" {
		return `<img class="cat-icon-img" src="` + htmlAttr(u) + `" alt="">`
	}
	if ic := strings.TrimSpace(g.Icon); ic != "" {
		return `<span class="cat-icon">` + htmlEscape(ic) + `</span>`
	}
	return ""
}

// formatKES renders a price as a thousands-separated integer (prices are pre-rounded
// to whole numbers in assembleMenuItems, matching POS no-decimal display rules).
func formatKES(v float64) string {
	n := int64(v + 0.5)
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// htmlAttr escapes a value for use inside an HTML attribute (img src etc.).
func htmlAttr(s string) string {
	r := strings.NewReplacer(`&`, "&amp;", `"`, "&quot;", `'`, "&#39;", `<`, "&lt;", `>`, "&gt;")
	return r.Replace(s)
}

// --- tiny colour helpers (CSS hex) — adapted from the treasury palette idea, not imported ---

func looksLikeHex(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "#") && (len(s) == 4 || len(s) == 7)
}

// hexToRGB parses #rgb or #rrggbb into 0-255 channels.
func hexToRGB(hex string) (r, g, b int, ok bool) {
	hex = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(hex), "#"))
	switch len(hex) {
	case 3:
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	case 6:
	default:
		return 0, 0, 0, false
	}
	var rv, gv, bv int
	if _, err := fmt.Sscanf(hex, "%02x%02x%02x", &rv, &gv, &bv); err != nil {
		return 0, 0, 0, false
	}
	return rv, gv, bv, true
}

func rgbToHex(r, g, b int) string {
	return fmt.Sprintf("#%02x%02x%02x", clampChannel(r), clampChannel(g), clampChannel(b))
}

// lightenChannel scales a single channel toward white by factor f (0..1).
func lightenChannel(c int, f float64) int {
	return clampChannel(c + int(float64(255-c)*f))
}

func clampChannel(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

// contrastText picks black or white text for legibility on the brand band using the
// standard luminance heuristic.
func contrastText(r, g, b int) string {
	lum := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b))
	if lum > 160 {
		return "#1a1a1a"
	}
	return "#ffffff"
}
