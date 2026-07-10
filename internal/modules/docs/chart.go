package docs

import "strings"

func upper(s string) string { return strings.ToUpper(s) }

func ifEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// Chart layout constants — fpdf has no chart helper, so both layouts below lay out gridlines,
// bars and labels by hand using the tenant brand palette.
const (
	chartH          = 62.0 // plot area height (mm) for the vertical bar layout
	chartLabelH     = 26.0 // room below the axis for an inclined category label (see textIncline)
	chartLabelAngle = 40.0 // incline for vertical-chart category labels
	maxVerticalBars = 6    // beyond this, a column chart's labels get too cramped to read even
	// inclined — a row-per-item horizontal layout scales to any count instead
	hRowH   = 7.4  // per-row height (mm) for the horizontal ("row per item") layout
	hLabelW = 46.0 // left gutter reserved for item labels in the horizontal layout
	hValueW = 24.0 // right gutter reserved for the value label past the bar end
)

// drawChart renders a bar chart. Vertical (columns, category labels inclined under the axis) for
// up to maxVerticalBars bars — beyond that it flips to a horizontal, row-per-item layout (see
// drawHorizontalChart) so labels stay legible and the chart scales to any number of items instead
// of squeezing bars into unreadable slivers.
func (rc *reportCtx) drawChart(s Section) {
	if len(s.Bars) == 0 {
		return
	}
	if len(s.Bars) > maxVerticalBars {
		rc.drawHorizontalChart(s)
		return
	}
	rc.drawVerticalChart(s)
}

// drawVerticalChart draws bars as columns with inclined category labels under the axis so longer
// item/category names stay legible instead of being clipped to a slot-width sliver.
func (rc *reportCtx) drawVerticalChart(s Section) {
	p := rc.p
	rc.ensure(chartH + chartLabelH + 8)

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
		// Category label, inclined so it reads clearly even when the slot is narrow — anchored at
		// the tick (cx, base+3) and swung down-left, giving it far more effective width than a
		// horizontal label clipped to the slot ever could.
		lbl := p.clip(b.Label, "B", 6.8, 32.0)
		p.textIncline(cx, base+3.0, lbl, "B", 6.8, p.pal.grey, chartLabelAngle)
	}

	// Baseline (x-axis).
	p.setDraw(p.pal.navy)
	p.pdf.SetLineWidth(0.3)
	p.pdf.Line(plotX, base, plotX+plotW, base)

	rc.y = base + chartLabelH
}

// drawHorizontalChart draws one row per bar — label on the left, bar extending right, value at the
// bar's end — so a chart with many items just grows taller (paginating like a table) instead of
// squeezing bars and labels into illegible slivers.
func (rc *reportCtx) drawHorizontalChart(s Section) {
	p := rc.p

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

	plotX := p.leftX + hLabelW
	plotW := p.contentW - hLabelW - hValueW
	if plotW < 20 {
		plotW = 20
	}

	for _, b := range s.Bars {
		rc.ensure(hRowH)
		y := rc.y

		rc.drawCellText(b.Label, p.leftX, plotX, y+2.2, "B", 7.2, p.pal.grey, "R")

		bw := plotW * (b.Value / niceMax)
		if bw < 0 {
			bw = 0
		}
		barH := hRowH * 0.55
		by := y + (hRowH-barH)/2
		if bw > 0.3 {
			p.gradient(plotX, by, bw, barH, 0.6, p.pal.sky, p.pal.blue)
		}
		rc.drawCellText(formatFloat(b.Value), plotX+bw, p.rightX, y+2.2, "B", 7.2, p.pal.navy, "L")

		rc.y += hRowH
	}
	rc.y += 4.0
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
