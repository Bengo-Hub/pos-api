package printing

import (
	"bytes"

	qrcode "github.com/skip2/go-qrcode"
)

// ESC/POS QR support for the eTIMS verification QR on thermal receipts (the paper-ETR
// "KRA TIMS Details" block ends in a scannable QR on compliant receipts).
//
// Two encodings:
//   - native GS ( k (Model 2) — crisp and fast, supported by Epson-compatible printers
//     (the overwhelming majority of POS thermal printers). Used by default.
//   - raster GS v 0 — a bit-image fallback for printers whose firmware lacks GS ( k
//     (printer_profiles JSON can flag `"qr_native": false` to select it).

// appendNativeQR writes the GS ( k Model-2 QR sequence: model → module size → error
// correction → store data → print. Data longer than the printer's symbol capacity is
// clipped by the printer itself; eTIMS verification URLs are well within range.
func appendNativeQR(buf *bytes.Buffer, data string) {
	if data == "" {
		return
	}
	// Function 65: select model 2 (49 50 = "1","2"; the widely-supported model).
	buf.Write([]byte{0x1D, 0x28, 0x6B, 0x04, 0x00, 0x31, 0x41, 0x32, 0x00})
	// Function 67: module size 4 dots (~scannable 25–33 module symbols on 203dpi heads).
	buf.Write([]byte{0x1D, 0x28, 0x6B, 0x03, 0x00, 0x31, 0x43, 0x04})
	// Function 69: error correction level M (49 = L, 50 = M).
	buf.Write([]byte{0x1D, 0x28, 0x6B, 0x03, 0x00, 0x31, 0x45, 0x32})
	// Function 80: store the data in the symbol storage area. pL/pH cover len(data)+3.
	n := len(data) + 3
	buf.Write([]byte{0x1D, 0x28, 0x6B, byte(n & 0xFF), byte(n >> 8), 0x31, 0x50, 0x30})
	buf.WriteString(data)
	// Function 81: print the stored symbol.
	buf.Write([]byte{0x1D, 0x28, 0x6B, 0x03, 0x00, 0x31, 0x51, 0x30})
}

// appendRasterQR writes the QR as a GS v 0 bit image — the fallback for printers whose
// firmware lacks GS ( k. Each QR module is scaled 3×3 dots (~12mm symbol at 203dpi).
func appendRasterQR(buf *bytes.Buffer, data string) {
	if data == "" {
		return
	}
	q, err := qrcode.New(data, qrcode.Medium)
	if err != nil {
		return
	}
	q.DisableBorder = false
	grid := q.Bitmap() // [][]bool incl. quiet zone
	const scale = 3
	modules := len(grid)
	if modules == 0 {
		return
	}
	widthDots := modules * scale
	widthBytes := (widthDots + 7) / 8
	heightDots := modules * scale

	// GS v 0 m=0 xL xH yL yH — raster bit image, normal size.
	buf.Write([]byte{0x1D, 0x76, 0x30, 0x00,
		byte(widthBytes & 0xFF), byte(widthBytes >> 8),
		byte(heightDots & 0xFF), byte(heightDots >> 8)})
	row := make([]byte, widthBytes)
	for _, moduleRow := range grid {
		for i := range row {
			row[i] = 0
		}
		for x, dark := range moduleRow {
			if !dark {
				continue
			}
			for s := 0; s < scale; s++ {
				dot := x*scale + s
				row[dot/8] |= 0x80 >> (dot % 8)
			}
		}
		for s := 0; s < scale; s++ {
			buf.Write(row)
		}
	}
}

// appendEtimsQR centers and prints the eTIMS verification QR, native by default,
// raster when the target printer is flagged qr_native=false.
func appendEtimsQR(buf *bytes.Buffer, url string, raster bool) {
	if url == "" {
		return
	}
	buf.Write(escCenter)
	if raster {
		appendRasterQR(buf, url)
	} else {
		appendNativeQR(buf, url)
	}
	buf.Write(escLF)
	buf.Write(escLeft)
}
