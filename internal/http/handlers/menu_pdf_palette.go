package handlers

// menuPDFRGB is an 8-bit RGB colour used by the fpdf menu renderer.
type menuPDFRGB struct{ r, g, b int }

// menuPDFPalette is the RGB colour set for renderMenuPDF. The branded tones (primary / navy /
// tint / border / onBrand) are derived from the tenant primary colour; the neutral ink/muted/
// placeholder tones are fixed so body text stays legible regardless of brand colour.
//
// This mirrors the idea in treasury-api docs/render_palette.go (brand-derived tints) but is a
// self-contained copy in this package — we do NOT import that other service.
type menuPDFPalette struct {
	primary     menuPDFRGB // tenant primary (header band, headings, left-rule)
	navy        menuPDFRGB // darkened brand tone (item names)
	onBrand     menuPDFRGB // text colour over the brand band (white/dark for contrast)
	tint        menuPDFRGB // very light brand tint (card + category bg)
	border      menuPDFRGB // light brand-tinted hairline
	muted       menuPDFRGB // secondary text (descriptions, URL, footer)
	placeholder menuPDFRGB // empty-thumbnail fill
}

// menuPDFDefaultBrand is the indigo used when a tenant has no primary colour, matching the
// platform default (and treasury's defaultBrand{99,102,241}).
var menuPDFDefaultBrand = menuPDFRGB{99, 102, 241}

// newMenuPDFPalette builds a cohesive palette from the tenant primary colour (hex).
// Falls back to the platform default on empty/invalid input. It reuses the existing
// hexToRGB parser from menu_render.go (same package) so there is one hex parser.
func newMenuPDFPalette(primaryHex string) menuPDFPalette {
	base := menuPDFDefaultBrand
	if r, g, b, ok := hexToRGB(primaryHex); ok {
		base = menuPDFRGB{r, g, b}
	}
	return menuPDFPalette{
		primary:     base,
		navy:        menuPDFDarken(base, 0.45),
		onBrand:     menuPDFContrastText(base),
		tint:        menuPDFLighten(base, 0.92),
		border:      menuPDFLighten(base, 0.78),
		muted:       menuPDFRGB{107, 122, 144},
		placeholder: menuPDFRGB{242, 244, 247},
	}
}

// menuPDFDarken scales each channel toward black by factor f (0..1).
func menuPDFDarken(c menuPDFRGB, f float64) menuPDFRGB {
	return menuPDFRGB{
		r: clampChannel(int(float64(c.r) * (1 - f))),
		g: clampChannel(int(float64(c.g) * (1 - f))),
		b: clampChannel(int(float64(c.b) * (1 - f))),
	}
}

// menuPDFLighten scales each channel toward white by factor f (0..1). It delegates to the
// existing lightenChannel helper (menu_render.go) for the per-channel maths.
func menuPDFLighten(c menuPDFRGB, f float64) menuPDFRGB {
	return menuPDFRGB{
		r: lightenChannel(c.r, f),
		g: lightenChannel(c.g, f),
		b: lightenChannel(c.b, f),
	}
}

// menuPDFContrastText picks black or white text for legibility over the brand band, reusing
// the luminance heuristic + hex parser already in menu_render.go.
func menuPDFContrastText(c menuPDFRGB) menuPDFRGB {
	switch contrastText(c.r, c.g, c.b) {
	case "#ffffff":
		return menuPDFRGB{255, 255, 255}
	default:
		return menuPDFRGB{26, 26, 26}
	}
}
