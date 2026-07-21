// Package docs renders professional, tenant-branded A4 report documents (PDF/CSV) for the POS.
//
// It is the POS counterpart of the treasury-api docs engine: the same visual language — a dynamic
// brand-color palette derived from the tenant primary color, a logo+title header with a gradient
// rule, a meta strip, summary cards, zebra-striped data tables, key/value blocks and a footer — but
// shaped for TABULAR REPORTS (sales, Z/reset, item-type, tax, staff) rather than invoice documents.
//
// The engine here (geometry + painter primitives + palette + formatting) is reusable; the report
// model and renderer live in report.go / render.go; the multi-format facade is in service.go.
package docs

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

// Page geometry (A4 portrait / landscape, mm).
const (
	pageWP   = 210.0 // portrait width
	pageWL   = 297.0 // landscape width
	margin   = 13.0
	topY     = 12.0
	bottomMg = 14.0
)

// rgb is an 8-bit RGB color used by the fpdf renderer.
type rgb struct{ r, g, b int }

// palette is the full set of colors used to render a report. The branded shades (sky/blue/navy/
// line/lightBlue) are derived from the tenant primary color; the neutral ink/grey/muted tones and
// white are fixed so body text stays legible regardless of brand color.
type palette struct {
	sky       rgb // lightest brand tone (gradient start)
	blue      rgb // mid brand tone (= tenant primary)
	navy      rgb // darkest brand tone (gradient end, headings)
	ink       rgb // body text
	grey      rgb // secondary text
	muted     rgb // tertiary text
	line      rgb // borders / hairlines
	lightBlue rgb // light brand tint (key cells, header row, zebra)
	zebra     rgb // alternating table row fill
	white     rgb
	bannerSub rgb // banner sub-label tone (light tint over gradient)
}

// defaultBrand is the indigo used when a tenant has no primary color set.
var defaultBrand = rgb{99, 102, 241}

// newPalette builds a cohesive palette from the tenant primary color (hex). Falls back to
// defaultBrand on empty/invalid input.
func newPalette(primaryHex string) palette {
	base := defaultBrand
	if r, g, b, ok := hexToRGB(primaryHex); ok {
		base = rgb{r, g, b}
	}
	return palette{
		sky:       lighten(base, 0.30),
		blue:      base,
		navy:      darken(base, 0.45),
		ink:       rgb{34, 48, 63},
		grey:      rgb{82, 97, 122},
		muted:     rgb{107, 122, 144},
		line:      lighten(base, 0.80),
		lightBlue: lighten(base, 0.90),
		zebra:     lighten(base, 0.95),
		white:     rgb{255, 255, 255},
		bannerSub: lighten(base, 0.78),
	}
}

func darken(c rgb, f float64) rgb {
	return rgb{clamp(int(float64(c.r) * (1 - f))), clamp(int(float64(c.g) * (1 - f))), clamp(int(float64(c.b) * (1 - f)))}
}

func lighten(c rgb, f float64) rgb {
	return rgb{
		clamp(c.r + int(float64(255-c.r)*f)),
		clamp(c.g + int(float64(255-c.g)*f)),
		clamp(c.b + int(float64(255-c.b)*f)),
	}
}

func clamp(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

// hexToRGB parses "#RRGGBB" (or "RRGGBB") into RGB ints.
func hexToRGB(hex string) (int, int, int, bool) {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) != 6 {
		return 0, 0, 0, false
	}
	r, e1 := strconv.ParseInt(hex[0:2], 16, 0)
	g, e2 := strconv.ParseInt(hex[2:4], 16, 0)
	b, e3 := strconv.ParseInt(hex[4:6], 16, 0)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, false
	}
	return int(r), int(g), int(b), true
}

// painter wraps an fpdf document with the palette and the drawing primitives used by the renderer.
type painter struct {
	pdf      *fpdf.Fpdf
	tr       func(string) string
	pal      palette
	leftX    float64
	rightX   float64
	contentW float64
}

func newPainter(pdf *fpdf.Fpdf, pal palette, pageWidth float64) *painter {
	base := pdf.UnicodeTranslatorFromDescriptor("") // UTF-8 → cp1252 (core fonts are 256-glyph)
	// The translator maps UTF-8 → cp1252 single bytes (≤0xFF), which core fonts render — including
	// accented Latin-1 like "é" (Café). We only sanitize INVALID UTF-8 in the input first: a stray
	// byte would decode to U+FFFD which the translator can't map, and passing malformed bytes to
	// MultiCell/SplitText can panic. Translating already-valid UTF-8 never yields a wide byte, so no
	// post-processing (which previously corrupted "é" → "?") is needed.
	tr := func(s string) string {
		return base(strings.ToValidUTF8(s, ""))
	}
	return &painter{pdf: pdf, tr: tr, pal: pal, leftX: margin, rightX: pageWidth - margin, contentW: pageWidth - 2*margin}
}

func (p *painter) setFill(c rgb) { p.pdf.SetFillColor(c.r, c.g, c.b) }
func (p *painter) setText(c rgb) { p.pdf.SetTextColor(c.r, c.g, c.b) }
func (p *painter) setDraw(c rgb) { p.pdf.SetDrawColor(c.r, c.g, c.b) }

// gradient fills a rounded rect with a horizontal linear gradient from c1 to c2.
func (p *painter) gradient(x, y, w, h, r float64, c1, c2 rgb) {
	p.pdf.ClipRoundedRect(x, y, w, h, r, false)
	p.pdf.LinearGradient(x, y, w, h, c1.r, c1.g, c1.b, c2.r, c2.g, c2.b, 0, 0, 1, 0)
	p.pdf.ClipEnd()
}

// box draws a white rounded rect with a hairline border.
func (p *painter) box(x, y, w, h float64) {
	p.setDraw(p.pal.line)
	p.setFill(p.pal.white)
	p.pdf.SetLineWidth(0.2)
	p.pdf.RoundedRect(x, y, w, h, 1.6, "1234", "D")
}

// fillRect paints a plain filled rectangle (no border).
func (p *painter) fillRect(x, y, w, h float64, c rgb) {
	p.setFill(c)
	p.pdf.Rect(x, y, w, h, "F")
}

// text draws a string with its top at y (baseline computed for predictable placement).
func (p *painter) text(x, y float64, s, font string, sz float64, c rgb) {
	p.pdf.SetFont("Helvetica", font, sz)
	p.setText(c)
	p.pdf.Text(x, y+sz*0.3528*0.82, p.tr(s))
}

// textR draws a right-aligned string ending at rx.
func (p *painter) textR(rx, y float64, s, font string, sz float64, c rgb) {
	p.pdf.SetFont("Helvetica", font, sz)
	p.setText(c)
	t := p.tr(s)
	p.pdf.Text(rx-p.pdf.GetStringWidth(t), y+sz*0.3528*0.82, t)
}

// textC draws a string centered within [x, x+w].
func (p *painter) textC(x, w, y float64, s, font string, sz float64, c rgb) {
	p.pdf.SetFont("Helvetica", font, sz)
	p.setText(c)
	t := p.tr(s)
	p.pdf.Text(x+(w-p.pdf.GetStringWidth(t))/2, y+sz*0.3528*0.82, t)
}

// textIncline draws a string ending at the anchor (x, y) — like textR — but rotated `angle`
// degrees counter-clockwise around that anchor. Used for inclined chart axis labels: a positive
// angle (e.g. 40) swings the text down-and-left away from the anchor, so it reads bottom-left to
// top-right and stays legible without overlapping its neighbors, the same convention spreadsheet
// tools use for tilted category-axis labels.
func (p *painter) textIncline(x, y float64, s, font string, sz float64, c rgb, angle float64) {
	p.pdf.TransformBegin()
	p.pdf.TransformRotate(angle, x, y)
	p.textR(x, y, s, font, sz, c)
	p.pdf.TransformEnd()
}

// hline draws a horizontal hairline in the line color.
func (p *painter) hline(x1, y, x2 float64) {
	p.setDraw(p.pal.line)
	p.pdf.SetLineWidth(0.15)
	p.pdf.Line(x1, y, x2, y)
}

// multiCell wraps text at the given x/width. Returns the y after the block.
func (p *painter) multiCell(x, y, w, lineH float64, s, font string, sz float64, c rgb) float64 {
	p.pdf.SetFont("Helvetica", font, sz)
	p.setText(c)
	p.pdf.SetXY(x, y)
	p.pdf.MultiCell(w, lineH, p.tr(s), "", "L", false)
	return p.pdf.GetY()
}

// clip clamps a string to fit within width w for the given font/size, adding an ellipsis.
func (p *painter) clip(s, font string, sz, w float64) string {
	p.pdf.SetFont("Helvetica", font, sz)
	t := p.tr(s)
	if p.pdf.GetStringWidth(t) <= w {
		return t
	}
	for len(t) > 1 && p.pdf.GetStringWidth(t+"…") > w {
		t = t[:len(t)-1]
	}
	return t + "…"
}

// textFit draws s with its top at y, shrinking the font from sz down to minSz until the WHOLE string
// fits within width w — so large values (e.g. "KES 12,345,678.00" in a summary card) render in full
// instead of being clipped to an ellipsis. Only if the value still overflows at minSz is it clipped
// as a last resort. Use this (not clip) for figures that must stay legible in narrow tiles.
func (p *painter) textFit(x, y float64, s, font string, sz, minSz, w float64, c rgb) {
	t := p.tr(s)
	size := sz
	for size > minSz {
		p.pdf.SetFont("Helvetica", font, size)
		if p.pdf.GetStringWidth(t) <= w {
			break
		}
		size -= 0.25
	}
	p.pdf.SetFont("Helvetica", font, size)
	if p.pdf.GetStringWidth(t) > w {
		t = p.clip(s, font, size, w)
	}
	p.setText(c)
	p.pdf.Text(x, y+size*0.3528*0.82, t)
}

// ── formatting ────────────────────────────────────────────────────────────────

// formatFloat formats a float64 with 2 decimals and thousand separators.
func formatFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	dec := s[len(s)-3:]
	intPart := s[:len(s)-3]
	neg := strings.HasPrefix(intPart, "-")
	if neg {
		intPart = intPart[1:]
	}
	out := ""
	for i, c := range intPart {
		pos := len(intPart) - i
		if i > 0 && pos%3 == 0 {
			out += ","
		}
		out += string(c)
	}
	if neg {
		out = "-" + out
	}
	return out + dec
}

// formatQty renders a quantity with no trailing zeros (e.g. "2", "2.5", "12").
func formatQty(v float64) string {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// money formats "CODE 1,234.56".
func money(currency string, v float64) string {
	code := currency
	if fields := strings.Fields(currency); len(fields) > 0 {
		code = fields[0]
	}
	if code == "" {
		code = "KES"
	}
	return code + " " + formatFloat(v)
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("02 Jan 2006")
}

func formatDateTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("02 Jan 2006 15:04")
}

// imgType returns the fpdf image-type string for raw image bytes.
func imgType(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "JPG"
	case len(b) >= 4 && b[0] == 0x47 && b[1] == 0x49 && b[2] == 0x46:
		return "GIF"
	default:
		return "PNG"
	}
}
