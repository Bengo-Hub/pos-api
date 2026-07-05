package handlers

import (
	"bytes"
	"testing"
	"time"

	"github.com/bengobox/pos-service/internal/modules/docs"
)

// buildSampleReports assembles one docs.Report per report handler using the SAME section shapes the
// ReportPDFHandler builds, with synthetic data. This exercises the assembly + the package amount/
// quantity formatters end-to-end through the docs engine without needing a database.
func buildSampleReports() map[string]*docs.Report {
	from := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 5, 23, 59, 0, 0, time.UTC)
	base := func(title, sub string, landscape bool) *docs.Report {
		return &docs.Report{
			Title: title, Subtitle: sub, TenantName: "The Urban Loft Café",
			OutletName: "Urban Loft — Main", PrimaryColor: "#7a3b1d",
			PeriodFrom: from, PeriodTo: to, GeneratedAt: to, Currency: "KES", Landscape: landscape,
		}
	}

	reports := map[string]*docs.Report{}

	// 1. Reset Summary.
	rs := base("Reset Summary Report", "Z / Reset", false)
	rs.Cards = []docs.Card{
		{Label: "Tendering Total", Value: "KES " + fmtAmount(28790)},
		{Label: "Items Sold", Value: fmtQty(47)},
		{Label: "Voids", Value: fmtQty(0), Sub: "KES " + fmtAmount(0)},
	}
	rs.Sections = []docs.Section{
		{Kind: docs.SectionKeyValue, Title: "Tender Summary", Pairs: []docs.KV{
			{Label: "01 - M-PESA", Value: fmtAmount(27190)},
			{Label: "02 - CASH", Value: fmtAmount(1600)},
			{Label: "Tendering Total", Value: fmtAmount(28790), Bold: true, Rule: true},
		}},
		{Kind: docs.SectionTable, Title: "Item Types",
			Columns: []docs.Column{{Header: "Item Type", Weight: 2}, {Header: "Quantity", Weight: 1, Align: "R"}, {Header: "Amount", Weight: 1, Money: true}},
			Rows:    [][]docs.Cell{{docs.Text("KITCHEN"), docs.Text(fmtQty(47)), docs.Text(fmtAmount(28790))}},
			Total:   []docs.Cell{docs.BoldText("Report Total"), docs.BoldText(fmtQty(47)), docs.BoldText(fmtAmount(28790))}},
		{Kind: docs.SectionKeyValue, Title: "Totals", Pairs: []docs.KV{
			{Label: "Taxable", Value: fmtAmount(24818)}, {Label: "Tax", Value: fmtAmount(3972)},
			{Label: "Total", Value: fmtAmount(28790), Bold: true, Rule: true},
		}},
	}
	reports["reset-summary"] = rs

	// 2. Sales by Item Type (table groups + chart).
	sit := base("Sales by Item Type", "", false)
	sit.Sections = []docs.Section{
		{Kind: docs.SectionTable, Title: "KITCHEN",
			Columns: []docs.Column{{Header: "Item", Weight: 1.2}, {Header: "Description", Weight: 2.2}, {Header: "Quantity", Weight: 1, Align: "R"}, {Header: "Amount", Weight: 1.2, Money: true}},
			Rows:    [][]docs.Cell{{docs.Text("SKU1"), docs.Text("Burger"), docs.Text(fmtQty(12)), docs.Text(fmtAmount(6000))}},
			Total:   []docs.Cell{docs.BoldText("Subtotal"), docs.Text(""), docs.BoldText(fmtQty(12)), docs.BoldText(fmtAmount(6000))}},
		{Kind: docs.SectionChart, Title: "Sales by Item Type", ValueUnit: "KES", Bars: []docs.Bar{{Label: "KITCHEN", Value: 6000}, {Label: "BAR", Value: 3200}}},
	}
	reports["sales-by-item-type"] = sit

	// 3. Daily Sales.
	ds := base("Daily Sales", "", false)
	ds.Cards = []docs.Card{{Label: "Gross Revenue", Value: "KES " + fmtAmount(28790)}, {Label: "Orders", Value: "14"}}
	ds.Sections = []docs.Section{{Kind: docs.SectionTable, Title: "Sales by Day",
		Columns: []docs.Column{{Header: "Date", Weight: 1.6}, {Header: "Orders", Weight: 1, Align: "R"}, {Header: "Net", Weight: 1.2, Money: true}, {Header: "VAT", Weight: 1.2, Money: true}, {Header: "Gross", Weight: 1.2, Money: true}},
		Rows:    [][]docs.Cell{{docs.Text("2026-07-05"), docs.Text("14"), docs.Text(fmtAmount(24818)), docs.Text(fmtAmount(3972)), docs.Text(fmtAmount(28790))}},
		Total:   []docs.Cell{docs.BoldText("Total"), docs.BoldText("14"), docs.BoldText(fmtAmount(24818)), docs.BoldText(fmtAmount(3972)), docs.BoldText(fmtAmount(28790))}}}
	reports["daily-sales"] = ds

	// 4. Shift report.
	sh := base("Shift Report", "X Report", false)
	sh.Sections = []docs.Section{
		{Kind: docs.SectionKeyValue, Title: "Shift", Pairs: []docs.KV{{Label: "Opened", Value: "05 Jul 2026 08:00"}, {Label: "Closed", Value: "05 Jul 2026 17:00"}}},
		{Kind: docs.SectionKeyValue, Title: "Cash", Pairs: []docs.KV{{Label: "Opening Float", Value: fmtAmount(2000)}, {Label: "Variance", Value: fmtAmount(-50), Bold: true, Rule: true}}},
	}
	reports["shift-report"] = sh

	// 5. Sales by Staff (landscape).
	st := base("Sales by Staff", "", true)
	st.Sections = []docs.Section{{Kind: docs.SectionTable, Title: "Servers",
		Columns: []docs.Column{{Header: "Server", Weight: 2.2}, {Header: "Orders", Weight: 1, Align: "R"}, {Header: "Net Sales", Weight: 1.4, Money: true}, {Header: "Discounts", Weight: 1.4, Money: true}, {Header: "Voids", Weight: 1, Align: "R"}, {Header: "Avg Ticket", Weight: 1.4, Money: true}},
		Rows:    [][]docs.Cell{{docs.Text("Jane"), docs.Text("14"), docs.Text(fmtAmount(28790)), docs.Text(fmtAmount(0)), docs.Text("0"), docs.Text(fmtAmount(2056.43))}},
		Total:   []docs.Cell{docs.BoldText("Total"), docs.BoldText("14"), docs.BoldText(fmtAmount(28790)), docs.BoldText(fmtAmount(0)), docs.BoldText("0"), docs.BoldText(fmtAmount(2056.43))}}}
	reports["sales-by-staff"] = st

	// 6. Tax document.
	tx := base("Tax Report", "eTIMS / VAT", false)
	tx.Sections = []docs.Section{{Kind: docs.SectionTable, Title: "Tax by Code & Rate",
		Columns: []docs.Column{{Header: "KRA Code", Weight: 1.4}, {Header: "Rate", Weight: 1, Align: "R"}, {Header: "Taxable Amount", Weight: 1.6, Money: true}, {Header: "Tax Amount", Weight: 1.6, Money: true}},
		Rows:    [][]docs.Cell{{docs.Text("A"), docs.Text(fmtQty(16) + "%"), docs.Text(fmtAmount(24818)), docs.Text(fmtAmount(3972))}},
		Total:   []docs.Cell{docs.BoldText("Total"), docs.Text(""), docs.BoldText(fmtAmount(24818)), docs.BoldText(fmtAmount(3972))}}}
	reports["tax-document"] = tx

	// 7. Most profitable (landscape).
	mp := base("Most Profitable Items", "", true)
	mp.Sections = []docs.Section{{Kind: docs.SectionTable, Title: "Profitability Ranking",
		Columns: []docs.Column{{Header: "Item", Weight: 2.4}, {Header: "Qty", Weight: 1, Align: "R"}, {Header: "Revenue", Weight: 1.4, Money: true}, {Header: "Cost", Weight: 1.4, Money: true}, {Header: "Profit", Weight: 1.4, Money: true}, {Header: "Margin", Weight: 1, Align: "R"}},
		Rows:    [][]docs.Cell{{docs.Text("Burger"), docs.Text(fmtQty(12)), docs.Text(fmtAmount(6000)), docs.Text(fmtAmount(2400)), docs.Text(fmtAmount(3600)), docs.Text(fmtQty(60) + "%")}},
		Total:   []docs.Cell{docs.BoldText("Total"), docs.Text(""), docs.BoldText(fmtAmount(6000)), docs.BoldText(fmtAmount(2400)), docs.BoldText(fmtAmount(3600)), docs.BoldText(fmtQty(60) + "%")}}}
	reports["most-profitable-document"] = mp

	return reports
}

func TestReportPDFHandlersGeneratePDF(t *testing.T) {
	svc := &docs.DocumentService{}
	for name, rep := range buildSampleReports() {
		rep := rep
		t.Run(name, func(t *testing.T) {
			b, mime, err := svc.Generate(rep, docs.FormatPDF)
			if err != nil {
				t.Fatalf("%s: generate pdf: %v", name, err)
			}
			if mime != docs.MimePDF {
				t.Fatalf("%s: mime = %q, want %q", name, mime, docs.MimePDF)
			}
			if !bytes.HasPrefix(b, []byte("%PDF")) {
				t.Fatalf("%s: output is not a PDF", name)
			}
			if len(b) < 1000 {
				t.Fatalf("%s: PDF suspiciously small: %d bytes", name, len(b))
			}
			// CSV must also render for the ?format=csv path.
			c, _, cerr := svc.Generate(rep, docs.FormatCSV)
			if cerr != nil {
				t.Fatalf("%s: generate csv: %v", name, cerr)
			}
			if !bytes.Contains(c, []byte(rep.Title)) {
				t.Fatalf("%s: csv missing title", name)
			}
		})
	}
}

func TestFmtAmountAndQty(t *testing.T) {
	if got := fmtAmount(1234.5); got != "1,234.50" {
		t.Fatalf("fmtAmount(1234.5) = %q, want 1,234.50", got)
	}
	if got := fmtAmount(-2500.1); got != "-2,500.10" {
		t.Fatalf("fmtAmount(-2500.1) = %q", got)
	}
	if got := fmtQty(2.5); got != "2.5" {
		t.Fatalf("fmtQty(2.5) = %q, want 2.5", got)
	}
	if got := fmtQty(12); got != "12" {
		t.Fatalf("fmtQty(12) = %q, want 12", got)
	}
}
