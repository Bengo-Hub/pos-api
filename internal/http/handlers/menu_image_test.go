package handlers

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-pdf/fpdf"
)

// bigJPEG returns a large (wxh) JPEG — stands in for a full-resolution menu thumbnail.
func bigJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b.Bytes()
}

// TestMenuImageDownscaledAndBounded verifies that a large image served as the WRONG content-type
// (JPEG bytes labeled image/png — exactly the urban-loft logo case) is decoded, downscaled to a
// thumbnail and embedded, keeping the PDF small.
func TestMenuImageDownscaledAndBounded(t *testing.T) {
	big := bigJPEG(t, 2000, 1500) // large source
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png") // mislabeled, like the real logo
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	f := newMenuImageFetcher(pdf)

	// Embed the same large image many times (simulating a full menu of thumbnails).
	for i := 0; i < 60; i++ {
		im := f.get(srv.URL + "?i=" + string(rune('a'+i%26)))
		if !im.ok {
			t.Fatalf("image %d failed to embed", i)
		}
		if im.w > menuThumbMaxPx || im.h > menuThumbMaxPx {
			t.Fatalf("image not downscaled: %.0fx%.0f (max %d)", im.w, im.h, menuThumbMaxPx)
		}
		f.draw(im, 10, 10, 16)
	}

	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("%PDF")) || !bytes.Contains(out.Bytes()[len(out.Bytes())-2048:], []byte("%%EOF")) {
		t.Fatalf("invalid/incomplete PDF")
	}
	// 60 full-res 2000x1500 images would be tens of MB; downscaled thumbnails must keep it tiny.
	const cap = 3 << 20 // 3 MB
	if out.Len() > cap {
		t.Fatalf("PDF too large: %d bytes (cap %d) — downscaling not effective", out.Len(), cap)
	}
	t.Logf("60 thumbnails embedded; PDF = %d KB", out.Len()/1024)
}
