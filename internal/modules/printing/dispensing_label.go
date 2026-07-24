package printing

import (
	"bytes"
	"strconv"
	"strings"
	"time"
)

// DispensingLabelJobType is the printing.Queue JobType for a pharmacy dispensing label.
// Kept separate from the receipt-format layouts (layouts/registry.go): a label is a distinct
// print ACTION on a distinct physical medium (small-format sticker, one drug per label), not
// a receipt_format setting choice for a whole order — it reuses the same queue/agent/ESC-POS
// pipeline, not the "which receipt layout" picker.
const DispensingLabelJobType = "dispensing_label"

// DispensingLabelData holds everything printed on one drug's dispensing label.
type DispensingLabelData struct {
	OutletName     string
	DrugName       string
	Dosage         string
	Instructions   string
	Quantity       int
	PatientName    string
	LotNumber      string
	ExpiryDate     *time.Time
	PrescriberName string
	DispensedBy    string
	DispensedAt    time.Time
	// BarcodeValue — when non-empty, a scan-back Code128 barcode is printed (the prescription
	// number), letting a pharmacist rescan the label to pull up the prescription record.
	BarcodeValue string
}

// BuildDispensingLabel renders an ESC/POS byte buffer for one drug's dispensing label —
// dosage/instructions, patient, batch/lot + expiry (from Phase 2's now-populated
// POSOrderLine/PrescriptionLine fields), prescriber, dispensed-by, and a scan-back barcode.
// Reuses the exact ESC/POS command set BuildReceipt already uses (escInit/escBold/escCut/
// native CODE128 via GS k) so it flows through the same print pipeline unmodified.
func BuildDispensingLabel(d DispensingLabelData) []byte {
	var buf bytes.Buffer
	write := func(b []byte) { buf.Write(b) }
	writeln := func(s string) { buf.WriteString(s); buf.Write(escLF) }
	separator := func() { writeln(strings.Repeat("-", 32)) }

	write(escInit)
	write(escFontA)

	write(escCenter)
	write(escBold)
	writeln(trimField(d.OutletName, 32))
	writeln("DISPENSING LABEL")
	write(escLeft)
	separator()

	write(escDoubleHW)
	writeln(trimField(d.DrugName, 16)) // double-width halves the effective column budget
	write(escSizeReset)

	if d.Dosage != "" {
		write(escBold)
		writeln(trimField(d.Dosage, 32))
		write(escBoldOff)
	}
	if d.Instructions != "" {
		for _, line := range wrapLabelText(d.Instructions, 32) {
			writeln(line)
		}
	}
	if d.Quantity > 0 {
		writeln(formatLine("Qty", strconv.Itoa(d.Quantity)))
	}
	separator()
	writeln(formatLine("Patient", trimField(d.PatientName, 22)))
	if d.LotNumber != "" {
		writeln(formatLine("Batch", trimField(d.LotNumber, 20)))
	}
	if d.ExpiryDate != nil {
		writeln(formatLine("Expiry", d.ExpiryDate.Format("02 Jan 2006")))
	}
	if d.PrescriberName != "" {
		writeln(formatLine("Prescriber", trimField(d.PrescriberName, 18)))
	}
	if d.DispensedBy != "" {
		writeln(formatLine("Dispensed by", trimField(d.DispensedBy, 16)))
	}
	writeln(formatLine("Date", d.DispensedAt.Format("02 Jan 2006 15:04")))

	// WARNING banner — dosage/label warnings are a pharmacy-safety norm on dispensed medicine.
	separator()
	write(escBold)
	write(escCenter)
	writeln("Keep out of reach of children")
	write(escBoldOff)
	write(escLeft)

	if d.BarcodeValue != "" {
		buf.Write(escLF)
		write(escCenter)
		write([]byte{0x1D, 0x68, 50}) // GS h — barcode height (dots), shorter than the receipt's
		write([]byte{0x1D, 0x77, 2})  // GS w — module width
		write([]byte{0x1D, 0x48, 2})  // GS H — HRI (the number) below the bars
		write([]byte{0x1D, 0x66, 0})  // GS f — HRI font A
		payload := append([]byte{'{', 'B'}, []byte(d.BarcodeValue)...)
		write([]byte{0x1D, 0x6B, 73, byte(len(payload))}) // GS k m=73 (CODE128) n
		write(payload)
		buf.Write(escLF)
		write(escLeft)
	}

	buf.Write(escLF)
	buf.Write(escCut)
	return buf.Bytes()
}

// wrapLabelText greedily wraps s into lines of at most width runes, breaking on spaces.
func wrapLabelText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		line += " " + w
	}
	lines = append(lines, line)
	return lines
}
