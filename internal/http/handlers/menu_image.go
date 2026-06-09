package handlers

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/go-pdf/fpdf"
)

// menuImageFetcher embeds remote images into an fpdf document, best-effort and bounded.
//
// It reuses fetchReceiptLogo (receipt_pdf.go) for the actual HTTP download + PNG/JPG
// content-type sniffing, so there is a single image-fetch code path in this package.
// Every fetched image is registered with fpdf under a stable, per-URL name and cached so
// the same image URL (e.g. a repeated category icon) is downloaded + registered only once.
//
// Failures NEVER abort the document: a failed/unsupported image is recorded as a negative
// cache entry and the caller draws a placeholder instead.
type menuImageFetcher struct {
	pdf   *fpdf.Fpdf
	seq   int
	cache map[string]*menuImage // keyed by source URL
	mu    sync.Mutex            // assembleMenuItems fetches in goroutines elsewhere; render is single-goroutine, but guard anyway
}

// menuImage is a registered (or failed) image. ok=false means the fetch/registration failed
// and the caller should render a placeholder; name/w/h are then meaningless.
type menuImage struct {
	ok   bool
	name string  // fpdf registered image name (use with pdf.ImageOptions)
	typ  string  // "PNG" | "JPG"
	w    float64 // intrinsic pixel width  (for aspect-ratio fit)
	h    float64 // intrinsic pixel height
}

func newMenuImageFetcher(pdf *fpdf.Fpdf) *menuImageFetcher {
	return &menuImageFetcher{pdf: pdf, cache: map[string]*menuImage{}}
}

// get downloads, decodes and registers the image at url, returning a cached result on repeat
// calls. A zero-value (ok=false) result is returned — never an error — when the URL is empty
// or anything about the fetch/registration fails, so callers can simply check img.ok.
func (f *menuImageFetcher) get(url string) *menuImage {
	if url == "" {
		return &menuImage{ok: false}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cached, ok := f.cache[url]; ok {
		return cached
	}

	// fetchReceiptLogo: shared best-effort HTTP GET (5s timeout, 5MB cap) + PNG/JPG sniff.
	data, typ := fetchReceiptLogo(url)
	if data == nil || typ == "" {
		miss := &menuImage{ok: false}
		f.cache[url] = miss
		return miss
	}

	f.seq++
	name := fmt.Sprintf("menuimg_%d", f.seq)
	info := f.pdf.RegisterImageOptionsReader(name,
		fpdf.ImageOptions{ImageType: typ}, bytes.NewReader(data))
	if info == nil || info.Width() <= 0 || info.Height() <= 0 {
		miss := &menuImage{ok: false}
		f.cache[url] = miss
		return miss
	}

	img := &menuImage{ok: true, name: name, typ: typ, w: info.Width(), h: info.Height()}
	f.cache[url] = img
	return img
}

// draw places a previously-fetched image into a square box (boxX,boxY,box×box mm), scaled to
// fit while preserving aspect ratio and centred within the box. No-op when img is nil/not ok.
func (f *menuImageFetcher) draw(img *menuImage, boxX, boxY, box float64) {
	if img == nil || !img.ok {
		return
	}
	w, h := box, box
	if img.w > img.h {
		h = box * img.h / img.w
	} else if img.h > img.w {
		w = box * img.w / img.h
	}
	x := boxX + (box-w)/2
	y := boxY + (box-h)/2
	f.pdf.ImageOptions(img.name, x, y, w, h, false,
		fpdf.ImageOptions{ImageType: img.typ}, 0, "")
}
