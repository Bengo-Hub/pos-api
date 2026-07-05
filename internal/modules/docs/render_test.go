package docs

import (
	"bytes"
	"testing"
	"time"
)

// sampleReport builds a report exercising every section kind (cards, key/value, table + total,
// chart) so the golden test covers the whole renderer.
func sampleReport() *Report {
	from := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 5, 23, 59, 0, 0, time.UTC)
	return &Report{
		Title:        "Reset Summary Report",
		Subtitle:     "Customer Invoice",
		TenantName:   "The Urban Loft Café",
		OutletName:   "Urban Loft — Main",
		Address:      "PAYBILL: 522533\nA/C No.: 7931904",
		PrimaryColor: "#7a3b1d",
		PeriodFrom:   from,
		PeriodTo:     to,
		GeneratedAt:  to,
		Currency:     "KES",
		Meta:         [][2]string{{"Paybill", "522533"}},
		Cards: []Card{
			{Label: "Tendering Total", Value: "KES 28,790.00", Sub: "2 tenders"},
			{Label: "Items Sold", Value: "47", Sub: "1 item type"},
			{Label: "Voids", Value: "0", Sub: "KES 0.00"},
		},
		Sections: []Section{
			{
				Kind:  SectionKeyValue,
				Title: "Tender Summary",
				Pairs: []KV{
					{Label: "01 - M-PESA", Value: "27,190.00"},
					{Label: "04 - CASH", Value: "1,600.00"},
					{Label: "Tendering Total", Value: "28,790.00", Bold: true, Rule: true},
				},
			},
			{
				Kind:    SectionTable,
				Title:   "Item Types",
				Columns: []Column{{Header: "Item Type", Weight: 2}, {Header: "Quantity", Weight: 1, Align: "R"}, {Header: "Amount", Weight: 1, Money: true}},
				Rows: [][]Cell{
					{Text("KITCHEN"), Text("47"), Text("28,790.00")},
				},
				Total: []Cell{BoldText("Report Total"), Text(""), BoldText("28,790.00")},
			},
			{
				Kind:      SectionChart,
				Title:     "Sales by Item Type",
				ValueUnit: "KES",
				Bars: []Bar{
					{Label: "BARISTER", Value: 0},
					{Label: "KITCHEN", Value: 51630},
				},
			},
		},
		Footer: "Powered by BengoBox POS",
	}
}

func TestRenderReportPDF(t *testing.T) {
	svc := &DocumentService{}
	b, mime, err := svc.Generate(sampleReport(), FormatPDF)
	if err != nil {
		t.Fatalf("generate pdf: %v", err)
	}
	if mime != MimePDF {
		t.Fatalf("mime = %q, want %q", mime, MimePDF)
	}
	if !bytes.HasPrefix(b, []byte("%PDF")) {
		t.Fatalf("output is not a PDF (prefix %q)", firstBytes(b, 8))
	}
	if len(b) < 1500 {
		t.Fatalf("PDF suspiciously small: %d bytes", len(b))
	}
}

func TestRenderReportCSV(t *testing.T) {
	svc := &DocumentService{}
	b, mime, err := svc.Generate(sampleReport(), FormatCSV)
	if err != nil {
		t.Fatalf("generate csv: %v", err)
	}
	if mime != MimeCSV {
		t.Fatalf("mime = %q, want %q", mime, MimeCSV)
	}
	if !bytes.Contains(b, []byte("Tender Summary")) || !bytes.Contains(b, []byte("KITCHEN")) {
		t.Fatalf("csv missing expected content:\n%s", b)
	}
}

func TestLandscapeAndEmpty(t *testing.T) {
	svc := &DocumentService{}
	// Landscape wide table with many rows to exercise page-break + header repeat.
	rows := make([][]Cell, 0, 80)
	for i := 0; i < 80; i++ {
		rows = append(rows, []Cell{Text("Staff " + formatQty(float64(i))), Text("12"), Text("3,400.00")})
	}
	r := &Report{
		Title:     "Sales by Staff",
		Landscape: true,
		Sections: []Section{{
			Kind:    SectionTable,
			Title:   "Staff",
			Columns: []Column{{Header: "Server", Weight: 2}, {Header: "Orders", Weight: 1, Align: "R"}, {Header: "Net Sales", Weight: 1, Money: true}},
			Rows:    rows,
			Total:   []Cell{BoldText("Total"), Text("960"), BoldText("272,000.00")},
		}},
	}
	b, _, err := svc.Generate(r, FormatPDF)
	if err != nil {
		t.Fatalf("landscape generate: %v", err)
	}
	if !bytes.HasPrefix(b, []byte("%PDF")) {
		t.Fatalf("landscape output not a PDF")
	}
}

func firstBytes(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}
