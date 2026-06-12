package handlers

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
)

// bigJPEG returns the bytes of a (w x h) JPEG — stands in for a full-resolution menu thumbnail.
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
// thumbnail and embedded, keeping the PDF small and valid.
func TestMenuImageDownscaledAndBounded(t *testing.T) {
	big := bigJPEG(t, 2000, 1500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png") // mislabeled, like the real logo
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetCompression(true)
	pdf.AddPage()
	f := newMenuImageFetcher(pdf)

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
	const sizeCap = 3 << 20 // 3 MB
	if out.Len() > sizeCap {
		t.Fatalf("PDF too large: %d bytes (cap %d) — downscaling not effective", out.Len(), sizeCap)
	}
	t.Logf("60 thumbnails embedded; PDF = %d KB", out.Len()/1024)
}

// TestMenuImagePrefetchIsConcurrent verifies prefetch downloads images in parallel: a server that
// sleeps per request must not serialise ~36 fetches into 36×delay.
func TestMenuImagePrefetchIsConcurrent(t *testing.T) {
	const perReq = 150 * time.Millisecond
	jpg := bigJPEG(t, 600, 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(perReq)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpg)
	}))
	defer srv.Close()

	urls := make([]string, 0, 36)
	for i := 0; i < 36; i++ {
		urls = append(urls, srv.URL+"/?i="+string(rune('A'+i)))
	}

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	f := newMenuImageFetcher(pdf)

	start := time.Now()
	f.prefetch(urls)
	elapsed := time.Since(start)

	// 36 sequential fetches = 36*150ms = 5.4s. With 12 workers it's ~3 waves (~450ms) + overhead.
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("prefetch not concurrent: %v for 36 images (sequential ~5.4s)", elapsed)
	}
	for _, u := range urls {
		if im := f.get(u); !im.ok {
			t.Fatalf("prefetched image failed to register: %s", u)
		}
	}
	t.Logf("prefetched 36 images in %v", elapsed)
}
