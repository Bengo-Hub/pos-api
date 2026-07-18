// Package layouts is the single home of every printable receipt layout (HTML + PDF).
// The registry (registry.go) defines the available layouts; render.go dispatches a
// canonical Receipt to the selected layout's renderer. The ESC/POS thermal bytes live in
// the parent printing package (they are layout-independent — thermal hardware output is
// one fixed 32-column format), but read from the same printing.ReceiptView snapshot.
package layouts

import (
	"bytes"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Line is a single line item on the receipt.
type Line struct {
	SKU        string  `json:"sku"`
	Name       string  `json:"name"`
	Quantity   float64 `json:"quantity"`
	UnitPrice  float64 `json:"unit_price"`
	TotalPrice float64 `json:"total_price"`
}

// PaymentMethods carries the "HOW TO PAY" payment display info shown at the bottom of receipts.
type PaymentMethods struct {
	MpesaPaybill      string `json:"mpesa_paybill,omitempty"`
	MpesaAccountRef   string `json:"mpesa_account_reference,omitempty"`
	MpesaTill         string `json:"mpesa_till,omitempty"`
	MpesaPochi        string `json:"mpesa_pochi,omitempty"`
	BankName          string `json:"bank_name,omitempty"`
	BankAccountNumber string `json:"bank_account_number,omitempty"`
	BankAccountName   string `json:"bank_account_name,omitempty"`
}

// Brand is the tenant branding applied to rendered receipts/documents (logo + name).
// Receipts deliberately ignore the brand primary colour for ink — coloured text prints
// faint on thermal/non-colour printers — but it is kept for other document surfaces.
type Brand struct {
	CompanyName  string
	LogoURL      string
	PrimaryColor string
}

// Receipt is the full receipt payload rendered by every layout — and, JSON-encoded, the
// receipt API response consumed by pos-ui (which renders the same layouts client-side for
// offline-first printing). One struct, every surface.
type Receipt struct {
	ReceiptNumber string    `json:"receipt_number"`
	OrderNumber   string    `json:"order_number"`
	OutletID      uuid.UUID `json:"outlet_id"`
	OutletName    string    `json:"outlet_name,omitempty"`
	OutletAddress string    `json:"outlet_address,omitempty"`
	// Formatted labeled-phone line from the outlet's contact_phones metadata —
	// printed under the address as "Mobile: AIRTEL +2547… · MTN +2567…".
	OutletPhones string `json:"outlet_phones,omitempty"`
	// BillTo is the customer shown on the receipt: the guest/payer name for M-Pesa / card / online
	// payments (where a real customer is identified), or "Walk-in customer" for cash.
	BillTo      string    `json:"bill_to,omitempty"`
	BillToLabel string    `json:"bill_to_label,omitempty"` // "Customer" | "Paid by"
	IssuedAt    time.Time `json:"issued_at"`
	// Timezone is the outlet's IANA timezone (e.g. "Africa/Nairobi"). The frontend formats
	// issued_at in this zone so the printed time matches the local wall-clock regardless of
	// the device/browser timezone. Server-side HTML already renders issued_at in this zone.
	Timezone       string  `json:"timezone,omitempty"`
	Lines          []Line  `json:"lines"`
	Subtotal       float64 `json:"subtotal"`
	TaxAmount      float64 `json:"tax_amount"`
	DiscountAmount float64 `json:"discount_amount"`
	ChargesTotal   float64 `json:"charges_total,omitempty"`
	// Charges is the named breakdown behind ChargesTotal (packaging/service/shipping…) so the
	// receipt can itemise "Shipping(+)" etc. instead of one opaque charges line.
	Charges     map[string]float64 `json:"charges,omitempty"`
	RoundOff    float64            `json:"round_off,omitempty"`
	TotalAmount float64            `json:"total_amount"`
	Currency    string             `json:"currency"`
	AmountPaid  float64            `json:"amount_paid"`
	PaymentMethod string           `json:"payment_method"`
	// PaymentDate is when the shown payment settled — retail receipts print it beside the
	// method, e.g. "Cash (14-07-2026)". Omitted for unpaid bills.
	PaymentDate *time.Time `json:"payment_date,omitempty"`
	// BalanceDue = total − paid (positive: still owed / on account; negative: customer credit).
	BalanceDue         float64 `json:"balance_due"`
	AmountTendered     float64 `json:"amount_tendered"`
	ChangeDue          float64 `json:"change_due"`
	EtimsInvoiceNumber string  `json:"etims_invoice_number,omitempty"`
	EtimsQRCodeURL     string  `json:"etims_qr_code_url,omitempty"`
	// Fiscal identity for the "KRA TIMS Details" block (mirrors paper ETR receipts).
	// kra_pin prints in the business header; scu_id / cu_inv_no / rcpt_sign in the block.
	EtimsKraPin   string `json:"etims_kra_pin,omitempty"`
	EtimsScuID    string `json:"etims_scu_id,omitempty"`
	EtimsCuInvNo  string `json:"etims_cu_inv_no,omitempty"`
	EtimsRcptSign string `json:"etims_rcpt_sign,omitempty"`
	// EtimsQRPNG is the verification QR rendered server-side as a data: URI so every
	// surface (HTML/PDF/client print) embeds a real scannable image — etims_qr_code_url
	// is a KRA verification LINK, not an image.
	EtimsQRPNG     string          `json:"etims_qr_png,omitempty"`
	PaymentMethods *PaymentMethods `json:"payment_methods,omitempty"`
	// Configurable receipt/printer settings sourced from the outlet's POS settings.
	ReceiptHeader string  `json:"receipt_header,omitempty"`
	ReceiptFooter string  `json:"receipt_footer,omitempty"`
	VatEnabled    bool    `json:"vat_enabled"`
	VatRate       float64 `json:"vat_rate,omitempty"`
	PaperWidth    string  `json:"paper_width,omitempty"`
	ServedBy      string  `json:"served_by,omitempty"`
	// UseCase is the outlet's use case ("retail", "hospitality", …). Kept for retail-specific
	// content (barcode, Amount Paid line); layout selection is driven by Layout, not UseCase.
	UseCase string `json:"use_case,omitempty"`
	// Layout is the RESOLVED receipt layout id (registry.go) the receipt should render with —
	// the outlet's receipt_format setting resolved through layouts.Resolve. pos-ui branches on
	// this (falling back to a thermal layout when absent, e.g. old offline-cached payloads).
	Layout string `json:"layout,omitempty"`
	// ShowLogo mirrors the Receipt & Printing "show logo" setting (default true).
	ShowLogo bool `json:"show_logo"`
	// BarcodePNG is a Code 128 barcode of the order number as a data: URI, populated for
	// retail receipts so every surface (server HTML/PDF + client print) shows the same bars.
	BarcodePNG string `json:"barcode_png,omitempty"`
	// Platform-owner (Codevertex) advertisement lines printed at the very bottom of the receipt.
	ProviderFooterLead    string `json:"provider_footer_lead,omitempty"`
	ProviderFooterContact string `json:"provider_footer_contact,omitempty"`
}

// escape escapes user-configured text (header/footer/item names) before embedding it in
// receipt HTML, preventing layout breakage or injection from settings values.
func escape(s string) string { return html.EscapeString(s) }

// Escape is the exported form for other packages sharing receipt/menu HTML generation.
func Escape(s string) string { return escape(s) }

// brandImageHTTPClient is a shared, connection-pooling client for downloading branding/menu
// images. A menu render fetches ~50 images; reusing keep-alive connections (instead of a fresh
// client + TLS handshake per image) and a tighter timeout keeps the render fast and bounded.
// It is safe for concurrent use.
var brandImageHTTPClient = &http.Client{
	Timeout: 4 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 3 * time.Second,
	},
}

// FetchLogo best-effort downloads a logo/menu image (PNG/JPG); returns nil on any failure.
// The image type is sniffed from the ACTUAL BYTES, not the HTTP Content-Type / extension —
// logos are frequently mislabeled (a JPEG uploaded as "logo.png"), and fpdf rejects a
// declared-type/real-bytes mismatch with "not a PNG buffer", poisoning the whole document.
func FetchLogo(url string) ([]byte, string) {
	if url == "" {
		return nil, ""
	}
	resp, err := brandImageHTTPClient.Get(url) //nolint:noctx
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ""
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(&io.LimitedReader{R: resp.Body, N: 5 << 20}); err != nil {
		return nil, ""
	}
	switch ct := http.DetectContentType(buf.Bytes()); {
	case strings.Contains(ct, "png"):
		return buf.Bytes(), "PNG"
	case strings.Contains(ct, "jpeg"), strings.Contains(ct, "jpg"):
		return buf.Bytes(), "JPG"
	default:
		return nil, ""
	}
}
