package docs

import (
	"bytes"
	"fmt"

	"github.com/go-pdf/fpdf"
)

const pageH = 297.0 // A4 height (mm), both orientations

// renderReportPDF builds a premium, tenant-branded A4 report PDF from the Report model.
func renderReportPDF(r *Report) ([]byte, error) {
	orient := "P"
	pw := pageWP
	if r.Landscape {
		orient = "L"
		pw = pageWL
	}
	pdf := fpdf.New(orient, "mm", "A4", "")
	pdf.SetCompression(true)
	pdf.SetMargins(margin, topY, margin)
	pdf.SetAutoPageBreak(false, bottomMg) // manual page breaks (we draw with absolute coords)
	pdf.AddPage()

	p := newPainter(pdf, newPalette(r.PrimaryColor), pw)
	rc := &reportCtx{p: p, r: r}

	rc.drawHeader()
	rc.y = rc.drawMeta(rc.y + 5.0)
	if len(r.Cards) > 0 {
		rc.y = rc.drawCards(rc.y + 5.0)
	}
	for _, s := range r.Sections {
		rc.drawSection(s)
	}
	rc.drawFooter()

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("render report pdf: %w", err)
	}
	return buf.Bytes(), nil
}

// reportCtx threads the painter, report and vertical cursor through the render pipeline.
type reportCtx struct {
	p *painter
	r *Report
	y float64
}

// newPage starts a fresh page and resets the cursor (used when content overflows).
func (rc *reportCtx) newPage() {
	rc.p.pdf.AddPage()
	rc.y = topY
}

// ensure guarantees at least h mm of vertical space, adding a page (and redrawing nothing) if not.
func (rc *reportCtx) ensure(h float64) {
	if rc.y+h > pageH-bottomMg-6.0 {
		rc.newPage()
	}
}

// drawHeader renders the logo (left), title + subtitle (right) and the gradient rule beneath.
func (rc *reportCtx) drawHeader() {
	p, r := rc.p, rc.r
	hy := topY - 1.0
	logoH := 20.0
	if len(r.LogoPNG) > 0 {
		lt := r.LogoType
		if lt == "" {
			lt = imgType(r.LogoPNG)
		}
		if info := p.pdf.RegisterImageOptionsReader("rptlogo", fpdf.ImageOptions{ImageType: lt}, bytes.NewReader(r.LogoPNG)); info != nil && info.Height() > 0 {
			logoW := logoH * info.Width() / info.Height()
			p.pdf.ImageOptions("rptlogo", p.leftX, hy, logoW, logoH, false, fpdf.ImageOptions{ImageType: lt}, 0, "")
		} else {
			p.pdf.ClearError()
		}
	}
	p.textR(p.rightX, hy+0.5, upper(r.Title), "B", 20, p.pal.navy)
	if r.Subtitle != "" {
		p.textR(p.rightX, hy+9.0, upper(r.Subtitle), "", 8, p.pal.blue)
	}
	ruleY := topY + logoH + 1.0
	p.gradient(p.leftX, ruleY, p.contentW, 1.4, 0.7, p.pal.sky, p.pal.navy)
	rc.y = ruleY + 2.0
}

// drawMeta renders the company block (left) and the meta box (right). Returns the y below the taller.
func (rc *reportCtx) drawMeta(y float64) float64 {
	p, r := rc.p, rc.r

	// Left: tenant / outlet identity + address.
	companyW := p.contentW*0.5 - 6.0
	p.text(p.leftX, y, ifEmpty(r.TenantName, "—"), "B", 14, p.pal.navy)
	ly := y + 6.2
	if r.OutletName != "" && r.OutletName != r.TenantName {
		p.text(p.leftX, ly, r.OutletName, "B", 9.5, p.pal.blue)
		ly += 5.0
	}
	if r.Address != "" {
		ly = p.multiCell(p.leftX, ly, companyW, 4.2, r.Address, "", 9, p.pal.grey) + 0.6
	}

	// Right: meta box (period, generated, currency + any extra Meta rows).
	rows := rc.metaRows()
	mbW := p.contentW * 0.44
	mbX := p.rightX - mbW
	keyW := mbW * 0.44
	const rowH = 6.4
	ry := y
	for i, kv := range rows {
		p.fillRect(mbX, ry, keyW, rowH, p.pal.lightBlue)
		if i > 0 {
			p.hline(mbX, ry, mbX+mbW)
		}
		p.text(mbX+2.5, ry+2.1, upper(kv[0]), "B", 7.2, p.pal.blue)
		p.text(mbX+keyW+2.5, ry+2.1, p.clip(kv[1], "B", 8, mbW-keyW-4.5), "B", 8, p.pal.navy)
		ry += rowH
	}
	p.box(mbX, y, mbW, ry-y)

	if ry > ly {
		return ry
	}
	return ly
}

// metaRows builds the meta-box rows, skipping empties.
func (rc *reportCtx) metaRows() [][2]string {
	r := rc.r
	rows := [][2]string{}
	if !r.PeriodFrom.IsZero() || !r.PeriodTo.IsZero() {
		rows = append(rows, [2]string{"Period", formatDate(r.PeriodFrom) + " - " + formatDate(r.PeriodTo)})
	}
	gen := r.GeneratedAt
	rows = append(rows, [2]string{"Generated", formatDateTime(gen)})
	if r.Currency != "" {
		rows = append(rows, [2]string{"Currency", r.Currency})
	}
	rows = append(rows, r.Meta...)
	return rows
}

// drawCards renders a row of summary tiles.
func (rc *reportCtx) drawCards(y float64) float64 {
	p := rc.p
	n := len(rc.r.Cards)
	if n == 0 {
		return y
	}
	gap := 5.0
	cw := (p.contentW - gap*float64(n-1)) / float64(n)
	ch := 18.0
	rc.ensure(ch + 4)
	y = rc.y
	x := p.leftX
	for _, c := range rc.r.Cards {
		p.box(x, y, cw, ch)
		p.fillRect(x, y, 1.4, ch, p.pal.blue) // accent bar
		p.text(x+4, y+3.0, upper(c.Label), "B", 7, p.pal.muted)
		p.text(x+4, y+8.0, p.clip(c.Value, "B", 13, cw-8), "B", 13, p.pal.navy)
		if c.Sub != "" {
			p.text(x+4, y+14.0, p.clip(c.Sub, "", 7.5, cw-8), "", 7.5, p.pal.grey)
		}
		x += cw + gap
	}
	return y + ch
}

// drawSection dispatches to the right renderer for the section kind.
func (rc *reportCtx) drawSection(s Section) {
	rc.y += 6.0
	if s.Title != "" {
		rc.ensure(10)
		rc.p.text(rc.p.leftX, rc.y, upper(s.Title), "B", 10.5, rc.p.pal.navy)
		rc.p.gradient(rc.p.leftX, rc.y+5.2, 26.0, 0.9, 0.4, rc.p.pal.sky, rc.p.pal.navy)
		rc.y += 8.0
		if s.Note != "" {
			rc.p.text(rc.p.leftX, rc.y, s.Note, "I", 8.5, rc.p.pal.grey)
			rc.y += 5.0
		}
	}
	switch s.Kind {
	case SectionTable:
		rc.drawTable(s)
	case SectionKeyValue:
		rc.drawKeyValue(s)
	case SectionChart:
		rc.drawChart(s)
	}
}

// drawTable renders a zebra-striped columnar table with an optional bold total row.
func (rc *reportCtx) drawTable(s Section) {
	p := rc.p
	if len(s.Columns) == 0 {
		return
	}
	// Column x-offsets from relative weights.
	var total float64
	for _, c := range s.Columns {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		total += w
	}
	xs := make([]float64, len(s.Columns)+1)
	xs[0] = p.leftX
	for i, c := range s.Columns {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		xs[i+1] = xs[i] + p.contentW*w/total
	}

	const headH = 7.0
	const rowH = 6.2

	drawHeaderRow := func() {
		rc.ensure(headH + rowH)
		p.fillRect(p.leftX, rc.y, p.contentW, headH, p.pal.lightBlue)
		for i, c := range s.Columns {
			rc.drawCellText(c.Header, xs[i], xs[i+1], rc.y+1.8, "B", 7.6, p.pal.navy, colAlign(c))
		}
		rc.y += headH
	}
	drawHeaderRow()

	for idx, row := range s.Rows {
		if rc.y+rowH > pageH-bottomMg-6.0 {
			rc.newPage()
			drawHeaderRow()
		}
		if idx%2 == 1 {
			p.fillRect(p.leftX, rc.y, p.contentW, rowH, p.pal.zebra)
		}
		for i, c := range s.Columns {
			if i >= len(row) {
				break
			}
			font := ""
			if row[i].Bold {
				font = "B"
			}
			rc.drawCellText(row[i].Text, xs[i], xs[i+1], rc.y+1.6, font, 8, p.pal.ink, colAlign(c))
		}
		rc.y += rowH
	}

	if len(s.Total) > 0 {
		if rc.y+rowH+1 > pageH-bottomMg-6.0 {
			rc.newPage()
			drawHeaderRow()
		}
		p.setDraw(p.pal.navy)
		p.pdf.SetLineWidth(0.3)
		p.pdf.Line(p.leftX, rc.y, p.rightX, rc.y)
		rc.y += 0.6
		for i, c := range s.Columns {
			if i >= len(s.Total) {
				break
			}
			rc.drawCellText(s.Total[i].Text, xs[i], xs[i+1], rc.y+1.6, "B", 8.5, p.pal.navy, colAlign(c))
		}
		rc.y += rowH
	}
}

// drawCellText draws a cell's text within [x0,x1] with 2mm padding, honoring alignment.
func (rc *reportCtx) drawCellText(s string, x0, x1, y float64, font string, sz float64, c rgb, align string) {
	p := rc.p
	pad := 2.0
	w := x1 - x0 - 2*pad
	if w < 2 {
		w = 2
	}
	t := p.clip(s, font, sz, w)
	switch align {
	case "R":
		p.textR(x1-pad, y, t, font, sz, c)
	case "C":
		p.textC(x0+pad, w, y, t, font, sz, c)
	default:
		p.text(x0+pad, y, t, font, sz, c)
	}
}

func colAlign(c Column) string {
	if c.Align != "" {
		return c.Align
	}
	if c.Money {
		return "R"
	}
	return "L"
}

// drawKeyValue renders a label→value block (tender summary, totals) inside a bordered box.
func (rc *reportCtx) drawKeyValue(s Section) {
	p := rc.p
	rowH := 6.0
	boxW := p.contentW * 0.62
	if boxW > 120 {
		boxW = 120
	}
	rc.ensure(float64(len(s.Pairs))*rowH + 4)
	top := rc.y
	y := rc.y
	for _, kv := range s.Pairs {
		if y+rowH > pageH-bottomMg-6.0 {
			p.box(p.leftX, top, boxW, y-top)
			rc.newPage()
			top, y = rc.y, rc.y
		}
		if kv.Rule {
			p.setDraw(p.pal.line)
			p.pdf.SetLineWidth(0.25)
			p.pdf.Line(p.leftX+2, y, p.leftX+boxW-2, y)
		}
		font := ""
		col := p.pal.ink
		if kv.Bold {
			font, col = "B", p.pal.navy
		}
		p.text(p.leftX+3, y+1.6, kv.Label, font, 9, col)
		p.textR(p.leftX+boxW-3, y+1.6, kv.Value, font, 9, col)
		y += rowH
	}
	p.box(p.leftX, top, boxW, y-top)
	rc.y = y
}

// drawFooter draws the generated-by footer line at the bottom of the current page.
func (rc *reportCtx) drawFooter() {
	p := rc.p
	note := rc.r.Footer
	if note == "" {
		note = "Generated by BengoBox POS · " + formatDateTime(rc.r.GeneratedAt)
	}
	fy := pageH - bottomMg + 1.0
	p.hline(p.leftX, fy, p.rightX)
	p.text(p.leftX, fy+1.5, note, "", 7, p.pal.muted)
	p.textR(p.rightX, fy+1.5, ifEmpty(rc.r.TenantName, ""), "", 7, p.pal.muted)
}
