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

// menuImageFetcher embeds remote images into an fpdf document, best-effort and bounded.
//
// Every fetched image is DECODED, downscaled to a thumbnail and re-encoded as a small baseline
// JPEG before being registered with fpdf. This (1) keeps the document small enough to render +
// stream within the request timeout, and (2) normalises every input (PNG/GIF/JPEG, and even a
// JPEG mislabeled as image/png) into a format fpdf always accepts — so a single odd image can
// never poison the document. Results are cached per source URL so a repeated icon/thumbnail is
// fetched + processed only once.
type menuImageFetcher struct {
	pdf   *fpdf.Fpdf
	seq   int
	cache map[string]*menuImage // keyed by source URL
	mu    sync.Mutex            // assembleMenuItems fetches in goroutines elsewhere; render is single-goroutine, but guard anyway
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
	return &menuImageFetcher{pdf: pdf, cache: map[string]*menuImage{}}
}

// get downloads, decodes, downscales and registers the image at url, returning a cached result
// on repeat calls. A zero-value (ok=false) result is returned — never an error — when the URL is
// empty or anything about the fetch/decode/registration fails, so callers can simply check img.ok.
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

	// fetchReceiptLogo: shared best-effort HTTP GET (5s timeout, 5MB cap).
	data, _ := fetchReceiptLogo(url)
	if data == nil {
		return miss()
	}

	// Decode (auto-detects PNG/JPEG/GIF from the bytes), downscale to a thumbnail, flatten any
	// transparency onto white (JPEG has no alpha) and re-encode as a small baseline JPEG.
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return miss()
	}
	thumb := flattenOnWhite(downscaleImage(src, menuThumbMaxPx))
	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, thumb, &jpeg.Options{Quality: 82}); err != nil {
		return miss()
	}

	f.seq++
	name := fmt.Sprintf("menuimg_%d", f.seq)
	info := f.pdf.RegisterImageOptionsReader(name,
		fpdf.ImageOptions{ImageType: "JPG"}, bytes.NewReader(jbuf.Bytes()))
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

// downscaleImage shrinks src so its longest edge is at most maxDim px, preserving aspect ratio,
// using an area-average (box) filter — good quality for downscaling and dependency-free.
// Images already within maxDim are returned unchanged.
//
// Performance: it converts the source to *image.RGBA ONCE (a single optimised draw) and then
// reads the packed Pix byte slice directly. Sampling via image.Image.At() instead made a full
// menu render take >2 minutes (per-pixel interface dispatch over millions of pixels under the
// pod CPU limit), tripping the gateway timeout.
func downscaleImage(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return src
	}

	// One fast conversion to a zero-origin RGBA so Pix can be indexed directly below.
	rgba, ok := src.(*image.RGBA)
	if !ok || rgba.Rect.Min != (image.Point{}) {
		conv := image.NewRGBA(image.Rect(0, 0, sw, sh))
		draw.Draw(conv, conv.Bounds(), src, b.Min, draw.Src)
		rgba = conv
	}
	if sw <= maxDim && sh <= maxDim {
		return rgba
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

	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for ty := 0; ty < th; ty++ {
		sy0 := ty * sh / th
		sy1 := (ty + 1) * sh / th
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for tx := 0; tx < tw; tx++ {
			sx0 := tx * sw / tw
			sx1 := (tx + 1) * sw / tw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rr, gg, bb, aa, n uint32
			for yy := sy0; yy < sy1; yy++ {
				row := yy * rgba.Stride
				for xx := sx0; xx < sx1; xx++ {
					i := row + xx*4
					rr += uint32(rgba.Pix[i])
					gg += uint32(rgba.Pix[i+1])
					bb += uint32(rgba.Pix[i+2])
					aa += uint32(rgba.Pix[i+3])
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			di := dst.PixOffset(tx, ty)
			dst.Pix[di] = uint8(rr / n)
			dst.Pix[di+1] = uint8(gg / n)
			dst.Pix[di+2] = uint8(bb / n)
			dst.Pix[di+3] = uint8(aa / n)
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
