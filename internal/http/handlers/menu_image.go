package handlers

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"sync"

	// Register decoders so image.Decode recognises whatever the URL actually serves,
	// independent of the HTTP Content-Type / file extension.
	_ "image/gif"
	_ "image/png"

	"github.com/go-pdf/fpdf"
)

// menuThumbMaxPx is the longest-edge size (px) every embedded image is downscaled to before
// it goes into the PDF. Item thumbnails render at ~16mm, category icons at ~6mm and the logo
// at ~16mm, so a few hundred px is plenty even when the PDF is zoomed/printed. Embedding the
// full-resolution source images instead produced ~30MB documents that were TRUNCATED on the
// wire (20s WriteTimeout) and never finished downloading.
const menuThumbMaxPx = 256

// menuPrefetchWorkers bounds how many menu images are downloaded + processed concurrently.
// A menu has ~50 images (logo, category icons, item thumbnails); fetching them one-at-a-time
// over fresh TLS connections made a render take 40s+ (and intermittently hit the gateway
// timeout) whenever a few downloads were slow. Prefetching concurrently bounds the wall-clock
// to roughly ceil(N/workers) × per-image time. Kept modest so the peak working set (each worker
// holds one downloaded + decoded image) stays well within the 512Mi pod limit.
const menuPrefetchWorkers = 6

// menuImageFetcher embeds remote images into an fpdf document, best-effort and bounded.
//
// Pipeline: every source image is DOWNLOADED, decoded, downscaled to a thumbnail and re-encoded
// as a small baseline JPEG (the "processing" stage — pure CPU/network, fpdf-free and therefore
// safe to run concurrently via prefetch), then REGISTERED with fpdf lazily in get() on the
// single render goroutine (fpdf is not concurrency-safe). This (1) keeps the document small
// enough to render + stream within the request timeout, (2) normalises every input (PNG/GIF/
// JPEG, even a JPEG mislabeled as image/png) into a format fpdf always accepts, and (3) keeps a
// full render fast by overlapping the many downloads.
type menuImageFetcher struct {
	pdf       *fpdf.Fpdf
	seq       int
	cache     map[string]*menuImage // url -> registered result (filled in get())
	processed map[string][]byte     // url -> thumbnail JPEG bytes (nil = failed); filled by prefetch/get
	mu        sync.Mutex
}

// menuImage is a registered (or failed) image. ok=false means the fetch/decode/registration
// failed and the caller should render a placeholder; name/w/h are then meaningless.
type menuImage struct {
	ok   bool
	name string  // fpdf registered image name (use with pdf.ImageOptions)
	typ  string  // always "JPG" after normalisation
	w    float64 // intrinsic pixel width  (for aspect-ratio fit)
	h    float64 // intrinsic pixel height
}

func newMenuImageFetcher(pdf *fpdf.Fpdf) *menuImageFetcher {
	return &menuImageFetcher{pdf: pdf, cache: map[string]*menuImage{}, processed: map[string][]byte{}}
}

// prefetch concurrently downloads + processes the given URLs into thumbnail JPEG bytes, ready
// for get() to register instantly. It never touches fpdf, so it is safe to fan out. Call it once
// before the render loop with every image URL the document will reference; get() still works for
// any URL that was not prefetched (it processes synchronously as a fallback).
func (f *menuImageFetcher) prefetch(urls []string) {
	seen := make(map[string]struct{}, len(urls))
	todo := make([]string, 0, len(urls))
	f.mu.Lock()
	for _, u := range urls {
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		if _, done := f.processed[u]; done {
			continue
		}
		todo = append(todo, u)
	}
	f.mu.Unlock()
	if len(todo) == 0 {
		return
	}

	sem := make(chan struct{}, menuPrefetchWorkers)
	var wg sync.WaitGroup
	for _, u := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			b := processMenuImage(u)
			f.mu.Lock()
			f.processed[u] = b
			f.mu.Unlock()
		}(u)
	}
	wg.Wait()
}

// processMenuImage downloads url and returns a downscaled baseline-JPEG thumbnail, or nil on any
// failure. Pure CPU/network — it never touches fpdf, so it is safe to call from many goroutines.
func processMenuImage(url string) []byte {
	if url == "" {
		return nil
	}
	data, _ := fetchReceiptLogo(url)
	if data == nil {
		return nil
	}
	// Decode (auto-detects PNG/JPEG/GIF from the bytes), downscale, flatten transparency onto
	// white (JPEG has no alpha) and re-encode as a small baseline JPEG.
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	thumb := flattenOnWhite(downscaleImage(src, menuThumbMaxPx))
	var jbuf bytes.Buffer
	// Quality 75 keeps thumbnails visibly crisp at ~16mm while compressing each to ~5-15KB.
	if err := jpeg.Encode(&jbuf, thumb, &jpeg.Options{Quality: 75}); err != nil {
		return nil
	}
	return jbuf.Bytes()
}

// get returns the registered image for url, using the prefetched thumbnail when available and
// processing synchronously otherwise. A zero-value (ok=false) result is returned — never an
// error — when the URL is empty or anything failed, so callers can simply check img.ok.
// Must be called on the render goroutine (it registers with the non-thread-safe fpdf).
func (f *menuImageFetcher) get(url string) *menuImage {
	if url == "" {
		return &menuImage{ok: false}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cached, ok := f.cache[url]; ok {
		return cached
	}

	miss := func() *menuImage {
		m := &menuImage{ok: false}
		f.cache[url] = m
		return m
	}

	jb, ok := f.processed[url]
	if !ok {
		// Not prefetched — process now (fallback path).
		jb = processMenuImage(url)
		f.processed[url] = jb
	}
	if jb == nil {
		return miss()
	}

	f.seq++
	name := fmt.Sprintf("menuimg_%d", f.seq)
	info := f.pdf.RegisterImageOptionsReader(name,
		fpdf.ImageOptions{ImageType: "JPG"}, bytes.NewReader(jb))
	if info == nil || info.Width() <= 0 || info.Height() <= 0 {
		// Belt-and-braces: a registration failure records an error in fpdf's internal state,
		// turning every later op into a no-op and failing Output. Clear it so the caller can
		// fall back to a placeholder and the rest of the menu still renders.
		f.pdf.ClearError()
		return miss()
	}

	img := &menuImage{ok: true, name: name, typ: "JPG", w: info.Width(), h: info.Height()}
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

// downscaleImage shrinks src so its longest edge is at most maxDim px, preserving aspect ratio.
// Images already within maxDim are returned unchanged.
//
// It samples directly from src (a few points averaged per destination pixel) and allocates ONLY
// the small destination thumbnail — it deliberately does NOT materialise a full-resolution RGBA
// copy of the source. Doing that (image.NewRGBA at source size) made 12 concurrent renders blow
// the 512Mi pod limit and OOM-kill the process; a full per-pixel box average via At() instead was
// far too slow. Bounded sparse sampling is both memory-light and fast enough for a ~16mm thumbnail.
func downscaleImage(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return src
	}
	if sw <= maxDim && sh <= maxDim {
		return src
	}

	tw, th := sw, sh
	if sw >= sh {
		tw = maxDim
		th = sh * maxDim / sw
	} else {
		th = maxDim
		tw = sw * maxDim / sh
	}
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}

	// At most maxSamples points per axis are averaged within each source box — enough
	// anti-aliasing for a thumbnail while keeping the At() call count (and time) bounded.
	const maxSamples = 3
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for ty := 0; ty < th; ty++ {
		by0 := b.Min.Y + ty*sh/th
		by1 := b.Min.Y + (ty+1)*sh/th
		if by1 <= by0 {
			by1 = by0 + 1
		}
		ny := by1 - by0
		if ny > maxSamples {
			ny = maxSamples
		}
		for tx := 0; tx < tw; tx++ {
			bx0 := b.Min.X + tx*sw/tw
			bx1 := b.Min.X + (tx+1)*sw/tw
			if bx1 <= bx0 {
				bx1 = bx0 + 1
			}
			nx := bx1 - bx0
			if nx > maxSamples {
				nx = maxSamples
			}
			var rr, gg, bb, aa, n uint32
			for sj := 0; sj < ny; sj++ {
				yy := by0 + (by1-by0)*sj/ny
				for si := 0; si < nx; si++ {
					xx := bx0 + (bx1-bx0)*si/nx
					cr, cg, cb, ca := src.At(xx, yy).RGBA() // 16-bit per channel
					rr += cr
					gg += cg
					bb += cb
					aa += ca
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			di := dst.PixOffset(tx, ty)
			dst.Pix[di] = uint8((rr / n) >> 8)
			dst.Pix[di+1] = uint8((gg / n) >> 8)
			dst.Pix[di+2] = uint8((bb / n) >> 8)
			dst.Pix[di+3] = uint8((aa / n) >> 8)
		}
	}
	return dst
}

// flattenOnWhite composites src over a white background, dropping any alpha channel — JPEG has
// no transparency, so a transparent PNG (e.g. a logo) would otherwise encode with a black box.
func flattenOnWhite(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, b, src, b.Min, draw.Over)
	return dst
}
