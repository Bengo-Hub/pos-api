package docs

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
)

// Format identifies the output format for report generation.
type Format string

const (
	FormatPDF Format = "pdf"
	FormatCSV Format = "csv"
)

const (
	MimePDF = "application/pdf"
	MimeCSV = "text/csv"
)

// DocumentService is the facade for generating POS report documents in all formats. It mirrors the
// treasury-api docs.DocumentService: one Generate entry point dispatching by format.
type DocumentService struct{}

// Generate produces the report bytes in the requested format, returning (bytes, mimeType, error).
func (s *DocumentService) Generate(r *Report, format Format) ([]byte, string, error) {
	switch format {
	case FormatPDF:
		b, err := renderReportPDF(r)
		return b, MimePDF, err
	case FormatCSV:
		b, err := renderReportCSV(r)
		return b, MimeCSV, err
	default:
		return nil, "", fmt.Errorf("unsupported report format: %q", format)
	}
}

// FormatFromString parses the ?format= query param, defaulting to PDF when empty.
func FormatFromString(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "pdf":
		return FormatPDF, nil
	case "csv":
		return FormatCSV, nil
	default:
		return "", fmt.Errorf("unsupported format %q: must be pdf or csv", s)
	}
}

// renderReportCSV flattens the report's tables and key/value blocks into a single CSV. Chart
// sections are emitted as label,value rows so the data is never lost in the export.
func renderReportCSV(r *Report) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	_ = w.Write([]string{r.Title})
	if r.Subtitle != "" {
		_ = w.Write([]string{r.Subtitle})
	}
	if !r.PeriodFrom.IsZero() || !r.PeriodTo.IsZero() {
		_ = w.Write([]string{"Period", formatDate(r.PeriodFrom), formatDate(r.PeriodTo)})
	}
	_ = w.Write([]string{}) // blank spacer

	for _, s := range r.Sections {
		if s.Title != "" {
			_ = w.Write([]string{s.Title})
		}
		switch s.Kind {
		case SectionTable:
			hdr := make([]string, len(s.Columns))
			for i, c := range s.Columns {
				hdr[i] = c.Header
			}
			_ = w.Write(hdr)
			for _, row := range s.Rows {
				rec := make([]string, len(row))
				for i, c := range row {
					rec[i] = c.Text
				}
				_ = w.Write(rec)
			}
			if len(s.Total) > 0 {
				rec := make([]string, len(s.Total))
				for i, c := range s.Total {
					rec[i] = c.Text
				}
				_ = w.Write(rec)
			}
		case SectionKeyValue:
			for _, kv := range s.Pairs {
				_ = w.Write([]string{kv.Label, kv.Value})
			}
		case SectionChart:
			for _, b := range s.Bars {
				_ = w.Write([]string{b.Label, formatFloat(b.Value)})
			}
		}
		_ = w.Write([]string{}) // spacer between sections
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("render report csv: %w", err)
	}
	return buf.Bytes(), nil
}
