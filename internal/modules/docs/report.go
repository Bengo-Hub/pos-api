package docs

import "time"

// Report is the canonical input model for every POS report document. Handlers map the data their
// existing JSON endpoints already compute into this model and hand it to the DocumentService — the
// renderer is entirely data-driven, so a new report is just a new assembly of Sections.
type Report struct {
	Title    string // e.g. "Reset Summary Report"
	Subtitle string // e.g. "Customer Invoice" / caption under the title

	// Branding (resolved by the handler from tenant branding).
	TenantName   string
	OutletName   string
	Address      string // outlet / tenant address line(s)
	PrimaryColor string // hex "#RRGGBB"; empty → default indigo
	LogoPNG      []byte // pre-fetched logo bytes (PNG/JPG); empty → no logo
	LogoType     string // "PNG" | "JPG" | "" (sniffed)

	// Meta.
	PeriodFrom  time.Time
	PeriodTo    time.Time
	GeneratedAt time.Time
	Currency    string      // e.g. "KES"
	Meta        [][2]string // extra key/value rows for the meta box (e.g. Paybill, A/C No.)

	// Layout.
	Landscape bool // wide tables (staff, tax) render on A4 landscape

	// Content.
	Cards    []Card    // optional summary tiles across the top
	Sections []Section // the body, rendered in order
	Footer   string    // footer note (defaults to a generated-by line)
}

// Card is a summary tile (label + big value + optional sub-caption).
type Card struct {
	Label string
	Value string
	Sub   string
}

// SectionKind selects how a Section renders.
type SectionKind int

const (
	SectionTable    SectionKind = iota // a columnar data table (with optional total row)
	SectionKeyValue                    // a label→value block (tender summary, totals)
	SectionChart                       // a vertical bar chart
)

// Section is one titled block of the report.
type Section struct {
	Kind  SectionKind
	Title string // section heading (empty = no heading)
	Note  string // optional caption under the heading

	// Table.
	Columns []Column
	Rows    [][]Cell
	Total   []Cell // optional bold summary/total row (same arity as Columns; empty = none)

	// KeyValue.
	Pairs []KV

	// Chart.
	Bars      []Bar
	ValueUnit string // optional axis/label unit (e.g. currency code)
}

// Column defines a table column.
type Column struct {
	Header string
	Weight float64 // relative width weight (columns share contentW proportionally)
	Align  string  // "L" | "R" | "C" (default "L")
	Money  bool    // right-align + treat as money (styling hint)
}

// Cell is a single table cell.
type Cell struct {
	Text string
	Bold bool
}

// KV is a label→value row in a key/value block.
type KV struct {
	Label string
	Value string
	Bold  bool // render bold (subtotal / grand-total rows)
	Rule  bool // draw a hairline above this row (separates a total)
}

// Bar is one bar in a chart section.
type Bar struct {
	Label string
	Value float64
}

// Text builds a plain cell.
func Text(s string) Cell { return Cell{Text: s} }

// BoldText builds a bold cell.
func BoldText(s string) Cell { return Cell{Text: s, Bold: true} }
