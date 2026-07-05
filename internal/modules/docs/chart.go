package docs

import "strings"

func upper(s string) string { return strings.ToUpper(s) }

func ifEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// drawChart renders a vertical bar chart (values across categories) drawn purely with fpdf
// primitives — fpdf has no chart helper, so this lays out gridlines, bars and labels by hand using
// the tenant brand palette. Used by the Sales-by-Item-Type report to mirror the AccuPOS chart.
func (rc *reportCtx) drawChart(s Section) {
	p := rc.p
	if len(s.Bars) == 0 {
		return
	}
	const chartH = 62.0 // plot area height (mm)
	const labelH = 14.0  // room for rotated/short category labels + axis value
	rc.ensure(chartH + labelH + 8)

	// Max value → axis scale (rounded up to a "nice" number).
	var maxV float64
	for _, b := range s.Bars {
		if b.Value > maxV {
			maxV = b.Value
		}
	}
	if maxV <= 0 {
		maxV = 1
	}
	niceMax := niceCeil(maxV)

	plotX := p.leftX + 20.0 // leave a gutter for y-axis value labels
	plotW := p.contentW - 24.0
	top := rc.y
	base := top + chartH

	// Gridlines + y-axis value labels (0, 25%, 50%, 75%, 100%).
	for i := 0; i <= 4; i++ {
		gy := base - chartH*float64(i)/4.0
		p.setDraw(p.pal.line)
		p.pdf.SetLineWidth(0.15)
		p.pdf.Line(plotX, gy, plotX+plotW, gy)
		val := niceMax * float64(i) / 4.0
		p.textR(plotX-2.0, gy-1.4, formatFloat(val), "", 6.5, p.pal.muted)
	}

	// Bars.
	n := len(s.Bars)
	slot := plotW / float64(n)
	barW := slot * 0.55
	if barW > 26 {
		barW = 26
	}
	for i, b := range s.Bars {
		cx := plotX + slot*float64(i) + slot/2
		bx := cx - barW/2
		bh := chartH * (b.Value / niceMax)
		if bh < 0 {
			bh = 0
		}
		by := base - bh
		if bh > 0.3 {
			p.gradient(bx, by, barW, bh, 0.8, p.pal.sky, p.pal.blue)
		}
		// Value label above the bar.
		p.textC(bx-6, barW+12, by-4.0, formatFloat(b.Value), "B", 6.8, p.pal.navy)
		// Category label under the axis (clipped to the slot).
		p.textC(cx-slot/2, slot, base+2.0, p.clip(b.Label, "", 6.8, slot-1), "B", 6.8, p.pal.grey)
	}

	// Baseline (x-axis).
	p.setDraw(p.pal.navy)
	p.pdf.SetLineWidth(0.3)
	p.pdf.Line(plotX, base, plotX+plotW, base)

	rc.y = base + labelH
}

// niceCeil rounds v up to a visually tidy axis maximum (1/2/2.5/5 × 10ⁿ).
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	mag := 1.0
	for v/mag >= 10 {
		mag *= 10
	}
	for v/mag < 1 {
		mag /= 10
	}
	n := v / mag // 1..10
	switch {
	case n <= 1:
		return 1 * mag
	case n <= 2:
		return 2 * mag
	case n <= 2.5:
		return 2.5 * mag
	case n <= 5:
		return 5 * mag
	default:
		return 10 * mag
	}
}
