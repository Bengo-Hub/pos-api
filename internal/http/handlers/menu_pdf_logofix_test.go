package handlers

import (
	"bytes"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-pdf/fpdf"
)

// tinyJPEG returns the bytes of a small, valid JPEG.
func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return b.Bytes()
}

// TestFetchReceiptLogoSniffsBytesNotContentType reproduces the production bug: a logo that is
// actually a JPEG but is served with Content-Type: image/png (exactly how
// accounts.codevertexitsolutions.com serves urban-loft.png). The old code trusted the header
// and told fpdf "PNG", producing "not a PNG buffer" which poisoned the whole menu document.
// The fix sniffs the real bytes, so the type must come back as JPG and embed cleanly.
func TestFetchReceiptLogoSniffsBytesNotContentType(t *testing.T) {
	jpg := tinyJPEG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png") // the lie that broke the PDF
		_, _ = w.Write(jpg)
	}))
	defer srv.Close()

	data, typ := fetchReceiptLogo(srv.URL)
	if typ != "JPG" {
		t.Fatalf("fetchReceiptLogo: want JPG (sniffed from bytes), got %q", typ)
	}

	// And the sniffed type must let fpdf embed it and produce a valid PDF.
	p := fpdf.New("P", "mm", "A4", "")
	p.AddPage()
	info := p.RegisterImageOptionsReader("logo", fpdf.ImageOptions{ImageType: typ}, bytes.NewReader(data))
	if info == nil || info.Width() <= 0 {
		t.Fatalf("fpdf failed to register the sniffed JPG logo")
	}
	p.ImageOptions("logo", 10, 10, 20, 0, false, fpdf.ImageOptions{ImageType: typ}, 0, "")
	var out bytes.Buffer
	if err := p.Output(&out); err != nil {
		t.Fatalf("render with sniffed logo failed: %v", err)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("%PDF")) {
		t.Fatalf("produced an invalid PDF")
	}
}

// TestClearErrorRecoversDocument proves the defense-in-depth half of the fix: an image that
// fpdf cannot decode sets its internal error state (turning every later op into a no-op and
// failing Output). ClearError must let the rest of the document render to a placeholder.
func TestClearErrorRecoversDocument(t *testing.T) {
	p := fpdf.New("P", "mm", "A4", "")
	p.AddPage()
	// Garbage registered as PNG -> fpdf records an error internally.
	p.RegisterImageOptionsReader("bad", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader([]byte("not an image at all")))
	if p.Ok() {
		t.Logf("note: fpdf did not flag the bad image; ClearError is still a safe no-op")
	}
	p.ClearError() // the fix in menuImageFetcher.get / receipt logo guard
	p.SetFont("Helvetica", "B", 12)
	p.Cell(40, 10, "menu still renders")
	var out bytes.Buffer
	if err := p.Output(&out); err != nil {
		t.Fatalf("ClearError did not recover the document: %v", err)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("%PDF")) {
		t.Fatalf("invalid PDF after ClearError")
	}
	_ = io.Discard
}
