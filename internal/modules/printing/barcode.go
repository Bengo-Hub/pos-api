package printing

import (
	"bytes"
	"encoding/base64"
	"image/png"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/code128"
)

// Code128PNG renders text as a Code 128 barcode PNG of roughly width×height pixels.
// Used by the retail receipt templates (PDF image + HTML/JSON data URI); ESC/POS thermal
// printers draw the same barcode natively via GS k instead.
func Code128PNG(text string, width, height int) ([]byte, error) {
	bc, err := code128.Encode(text)
	if err != nil {
		return nil, err
	}
	scaled, err := barcode.Scale(bc, width, height)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, scaled); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Code128DataURI is Code128PNG as a data: URI for direct <img> embedding; "" on failure
// (renderers then just print the number without bars — never fail a receipt over a barcode).
func Code128DataURI(text string, width, height int) string {
	b, err := Code128PNG(text, width, height)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}
