package handlers

import "github.com/bengobox/pos-service/internal/modules/printing/layouts"

// The receipt data/brand types and image/escape helpers moved to the central layouts
// package (internal/modules/printing/layouts) — the single home of receipt layout logic.
// These aliases keep the menu/report handlers (which share the brand type, logo fetch and
// HTML escaping) unchanged.

// receiptBrand is the tenant branding applied to rendered documents (logo/name/colour).
type receiptBrand = layouts.Brand

// receiptResponse is the JSON receipt payload — layouts.Receipt rendered by every surface.
type receiptResponse = layouts.Receipt

// fetchReceiptLogo best-effort downloads a logo/menu image (PNG/JPG), sniffing the real
// image type from the bytes; returns nil on any failure.
func fetchReceiptLogo(url string) ([]byte, string) { return layouts.FetchLogo(url) }

// htmlEscape escapes user-configured text before embedding it in generated HTML.
func htmlEscape(s string) string { return layouts.Escape(s) }
