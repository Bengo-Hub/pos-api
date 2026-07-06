package handlers

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entbillsplit "github.com/bengobox/pos-service/internal/ent/billsplit"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
)

// ReceiptHandler handles receipt generation endpoints.
type ReceiptHandler struct {
	log      *zap.Logger
	client   *ent.Client
	cache    *sharedcache.Aside // tenant branding cache (auth-api source)
	authURL  string
	auditSvc *audit.Service
}

// NewReceiptHandler creates a new ReceiptHandler.
func NewReceiptHandler(log *zap.Logger, client *ent.Client, cache *sharedcache.Aside, authURL string) *ReceiptHandler {
	return &ReceiptHandler{log: log, client: client, cache: cache, authURL: authURL}
}

// SetAuditService wires the centralized audit trail for receipt reprints.
func (h *ReceiptHandler) SetAuditService(a *audit.Service) { h.auditSvc = a }

// ReprintReceipt handles POST /{tenantID}/pos/orders/{orderID}/receipt/reprint.
// Increments the order's reprint counter and records a receipt.reprint audit
// entry (duplicate receipts are a cash-skimming vector). Returns the new count.
func (h *ReceiptHandler) ReprintReceipt(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order id", http.StatusBadRequest)
		return
	}
	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	updated, err := order.Update().AddReprintCount(1).Save(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if h.auditSvc != nil {
		var actor uuid.UUID
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			actor, _ = uuid.Parse(claims.Subject)
		}
		oid := order.OutletID
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: actor,
			Action:      "receipt.reprint",
			EntityType:  "pos_order",
			EntityID:    orderID.String(),
			After:       map[string]any{"reprint_count": updated.ReprintCount, "order_number": order.OrderNumber},
		})
	}
	jsonOK(w, map[string]any{"reprint_count": updated.ReprintCount, "is_duplicate": updated.ReprintCount > 0})
}

// receiptBrand is the tenant branding applied to the receipt PDF.
type receiptBrand struct {
	CompanyName  string
	LogoURL      string
	PrimaryColor string
}

// branding fetches tenant branding (logo/name/primary-color) from the shared cache, mirroring the
// documents module. Best-effort: returns a zero-value brand if anything is unavailable.
func (h *ReceiptHandler) branding(ctx context.Context, tenantID uuid.UUID) receiptBrand {
	var b receiptBrand
	if h.cache == nil || h.authURL == "" {
		return b
	}
	t, err := h.client.Tenant.Query().Where(enttenant.ID(tenantID)).Only(ctx)
	if err != nil {
		return b
	}
	b.CompanyName = t.Name
	td, err := sharedcache.GetTenantDetails(ctx, h.cache, h.authURL, t.Slug, sharedcache.DefaultTenantTTL)
	if err != nil {
		return b
	}
	tb := sharedcache.GetTenantBranding(td)
	if tb.Name != "" {
		b.CompanyName = tb.Name
	}
	b.LogoURL = tb.LogoURL
	b.PrimaryColor = tb.PrimaryColor
	return b
}

// receiptLine is a single line item in the receipt.
type receiptLine struct {
	SKU        string  `json:"sku"`
	Name       string  `json:"name"`
	Quantity   float64 `json:"quantity"`
	UnitPrice  float64 `json:"unit_price"`
	TotalPrice float64 `json:"total_price"`
}

// receiptPaymentMethods carries the payment display info shown at the bottom of receipts.
type receiptPaymentMethods struct {
	MpesaPaybill      string `json:"mpesa_paybill,omitempty"`
	MpesaAccountRef   string `json:"mpesa_account_reference,omitempty"`
	MpesaTill         string `json:"mpesa_till,omitempty"`
	MpesaPochi        string `json:"mpesa_pochi,omitempty"`
	BankName          string `json:"bank_name,omitempty"`
	BankAccountNumber string `json:"bank_account_number,omitempty"`
	BankAccountName   string `json:"bank_account_name,omitempty"`
}

// receiptResponse is the full receipt payload.
type receiptResponse struct {
	ReceiptNumber      string                 `json:"receipt_number"`
	OrderNumber        string                 `json:"order_number"`
	OutletID           uuid.UUID              `json:"outlet_id"`
	OutletName         string                 `json:"outlet_name,omitempty"`
	OutletAddress      string                 `json:"outlet_address,omitempty"`
	// BillTo is the customer shown on the receipt: the guest/payer name for M-Pesa / card / online
	// payments (where a real customer is identified), or "Walk-in customer" for cash.
	BillTo             string                 `json:"bill_to,omitempty"`
	IssuedAt           time.Time              `json:"issued_at"`
	// Timezone is the outlet's IANA timezone (e.g. "Africa/Nairobi"). The frontend formats
	// issued_at in this zone so the printed time matches the local wall-clock regardless of
	// the device/browser timezone. Server-side HTML already renders issued_at in this zone.
	Timezone           string                 `json:"timezone,omitempty"`
	Lines              []receiptLine          `json:"lines"`
	Subtotal           float64                `json:"subtotal"`
	TaxAmount          float64                `json:"tax_amount"`
	DiscountAmount     float64                `json:"discount_amount"`
	ChargesTotal       float64                `json:"charges_total,omitempty"`
	RoundOff           float64                `json:"round_off,omitempty"`
	TotalAmount        float64                `json:"total_amount"`
	Currency           string                 `json:"currency"`
	AmountPaid         float64                `json:"amount_paid"`
	PaymentMethod      string                 `json:"payment_method"`
	AmountTendered     float64                `json:"amount_tendered"`
	ChangeDue          float64                `json:"change_due"`
	EtimsInvoiceNumber string                 `json:"etims_invoice_number,omitempty"`
	EtimsQRCodeURL     string                 `json:"etims_qr_code_url,omitempty"`
	PaymentMethods     *receiptPaymentMethods `json:"payment_methods,omitempty"`
	// Configurable receipt/printer settings sourced from the outlet's POS settings.
	ReceiptHeader string  `json:"receipt_header,omitempty"`
	ReceiptFooter string  `json:"receipt_footer,omitempty"`
	VatEnabled    bool    `json:"vat_enabled"`
	VatRate       float64 `json:"vat_rate,omitempty"`
	PaperWidth    string  `json:"paper_width,omitempty"`
	ServedBy      string  `json:"served_by,omitempty"`
}

// GetReceipt handles GET /{tenantID}/pos/orders/{orderID}/receipt
// Query param ?format=pdf returns an HTML receipt suitable for printing; default returns JSON receipt data.
func (h *ReceiptHandler) GetReceipt(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderIDStr := chi.URLParam(r, "orderID")
	orderID, err := uuid.Parse(orderIDStr)
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		WithLines().
		WithPayments(func(q *ent.POSPaymentQuery) {
			q.Where(pospayment.StatusEQ("completed")).Limit(1)
		}).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("get receipt: query order", zap.Error(err))
		jsonError(w, "failed to get order", http.StatusInternalServerError)
		return
	}

	// Optional split-by-item receipt: ?split_id=<billsplit> filters the receipt to only the
	// order lines assigned to that split, so each payer gets their own itemised bill.
	var splitLineSet map[string]bool
	var splitLabel string
	if splitParam := r.URL.Query().Get("split_id"); splitParam != "" {
		if splitID, perr := uuid.Parse(splitParam); perr == nil {
			if split, serr := h.client.BillSplit.Query().
				Where(entbillsplit.ID(splitID), entbillsplit.TenantID(tid), entbillsplit.OrderID(orderID)).
				Only(ctx); serr == nil && len(split.OrderLineIds) > 0 {
				splitLineSet = make(map[string]bool, len(split.OrderLineIds))
				for _, id := range split.OrderLineIds {
					splitLineSet[id] = true
				}
				splitLabel = split.SplitLabel
			}
		}
	}

	// Safety: if a split's stored line ids match NONE of the order's lines (e.g. a caller stored
	// catalog ids instead of order-line ids), fall back to the full order rather than a blank bill.
	if splitLineSet != nil {
		matched := false
		for _, l := range order.Edges.Lines {
			if splitLineSet[l.ID.String()] {
				matched = true
				break
			}
		}
		if !matched {
			splitLineSet = nil
			splitLabel = ""
		}
	}

	lines := make([]receiptLine, 0, len(order.Edges.Lines))
	var subtotal float64
	for _, l := range order.Edges.Lines {
		if splitLineSet != nil && !splitLineSet[l.ID.String()] {
			continue // only this split's items
		}
		lines = append(lines, receiptLine{
			SKU:        l.Sku,
			Name:       l.Name,
			Quantity:   l.Quantity,
			UnitPrice:  l.UnitPrice,
			TotalPrice: l.TotalPrice,
		})
		subtotal += l.TotalPrice
	}

	var amountPaid float64
	paymentMethod := "cash"
	if len(order.Edges.Payments) > 0 {
		p := order.Edges.Payments[0]
		amountPaid = p.Amount
		if t, terr := h.client.Tender.Get(ctx, p.TenderID); terr == nil {
			if t.Type != "" {
				paymentMethod = t.Type
			} else if t.Name != "" {
				paymentMethod = t.Name
			}
		}
	}
	changeDue := amountPaid - order.TotalAmount
	if changeDue < 0 {
		changeDue = 0
	}

	// Bill-to name: cash sales are anonymous walk-ins; M-Pesa / card / mobile-money / online
	// payments identify a real customer, so show the captured guest/payer name.
	billTo := "Walk-in customer"
	if !isCashMethod(paymentMethod) {
		if order.CustomerName != nil && *order.CustomerName != "" {
			billTo = *order.CustomerName
		} else if order.CustomerPhone != nil && *order.CustomerPhone != "" {
			billTo = *order.CustomerPhone
		}
	}

	receipt := receiptResponse{
		BillTo:         billTo,
		ReceiptNumber:  fmt.Sprintf("RCT-%s", order.OrderNumber),
		OrderNumber:    order.OrderNumber,
		OutletID:       order.OutletID,
		IssuedAt:       order.CreatedAt,
		Lines:          lines,
		Subtotal:       subtotal,
		TaxAmount:      order.TaxTotal,
		DiscountAmount: order.DiscountTotal,
		ChargesTotal:   order.ChargesTotal,
		RoundOff:       order.RoundOff,
		TotalAmount:    order.TotalAmount,
		Currency:       order.Currency,
		AmountPaid:     amountPaid,
		PaymentMethod:  paymentMethod,
		AmountTendered: amountPaid,
		ChangeDue:      changeDue,
	}

	// For a split-by-item receipt the totals reflect ONLY this split's items (prices are
	// VAT-inclusive, so the split total is the sum of its line totals). Order-level tax/discount/
	// paid don't apply to an individual split.
	if splitLineSet != nil {
		receipt.TaxAmount = 0
		receipt.DiscountAmount = 0
		receipt.ChargesTotal = 0
		receipt.RoundOff = 0
		receipt.TotalAmount = subtotal
		receipt.AmountPaid = 0
		receipt.AmountTendered = 0
		receipt.ChangeDue = 0
		if splitLabel != "" {
			receipt.BillTo = splitLabel // e.g. "Guest 1" — this guest's own bill
		}
	}

	// Populate eTIMS fields if present on order (set by treasury.etims.invoice_transmitted subscriber).
	if order.EtimsInvoiceNumber != nil {
		receipt.EtimsInvoiceNumber = *order.EtimsInvoiceNumber
	}
	if order.EtimsQrCodeURL != nil {
		receipt.EtimsQRCodeURL = *order.EtimsQrCodeURL
	}

	// Enrich with outlet info
	if outlet, err := h.client.Outlet.Query().
		Where(entoutlet.ID(order.OutletID)).
		Only(ctx); err == nil {
		receipt.OutletName = outlet.Name
		receipt.Timezone = outlet.Timezone
		if addr := outlet.AddressJSON; addr != nil {
			if street, ok := addr["street"].(string); ok && street != "" {
				receipt.OutletAddress = street
			} else if city, ok := addr["city"].(string); ok {
				receipt.OutletAddress = city
			}
		}
	}

	// Pull the outlet's POS settings: receipt header/footer text, VAT rate, paper width, and
	// (when enabled) the payment display info. These drive both the JSON receipt the pos-ui
	// renders/prints and the server-side printable HTML.
	if s, err := h.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(order.OutletID)).
		Only(ctx); err == nil {
		if s.ReceiptHeader != nil {
			receipt.ReceiptHeader = *s.ReceiptHeader
		}
		if s.ReceiptFooter != nil {
			receipt.ReceiptFooter = *s.ReceiptFooter
		}
		receipt.VatEnabled = s.VatEnabled
		receipt.VatRate = s.VatRate
		receipt.PaperWidth = s.PaperWidth

		if s.ShowPaymentInfoOnReceipt {
			pm := &receiptPaymentMethods{}
			filled := false
			if s.MpesaPaybill != nil && *s.MpesaPaybill != "" {
				pm.MpesaPaybill = *s.MpesaPaybill
				filled = true
			}
			if s.MpesaAccountReference != nil && *s.MpesaAccountReference != "" {
				pm.MpesaAccountRef = *s.MpesaAccountReference
				filled = true
			}
			if s.MpesaTill != nil && *s.MpesaTill != "" {
				pm.MpesaTill = *s.MpesaTill
				filled = true
			}
			if s.MpesaPochi != nil && *s.MpesaPochi != "" {
				pm.MpesaPochi = *s.MpesaPochi
				filled = true
			}
			if s.BankName != nil && *s.BankName != "" {
				pm.BankName = *s.BankName
				filled = true
			}
			if s.BankAccountNumber != nil && *s.BankAccountNumber != "" {
				pm.BankAccountNumber = *s.BankAccountNumber
				filled = true
			}
			if s.BankAccountName != nil && *s.BankAccountName != "" {
				pm.BankAccountName = *s.BankAccountName
				filled = true
			}
			if filled {
				receipt.PaymentMethods = pm
			}
		}
	}

	format := r.URL.Query().Get("format")
	// Served-by (waiter/cashier name) is passed by the client print action so the bill matches the receipt.
	receipt.ServedBy = r.URL.Query().Get("served_by")
	if format == "pdf" {
		brand := h.branding(r.Context(), tid)
		pdfBytes, err := generateReceiptPDF(receipt, brand)
		if err != nil {
			h.log.Error("generate receipt pdf", zap.Error(err))
			jsonError(w, "Failed to generate receipt PDF", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="receipt-%s.pdf"`, order.OrderNumber))
		_, _ = w.Write(pdfBytes)
		return
	}
	if format == "html" {
		// Receipts print on thermal/non-colour printers, so we take only the tenant LOGO from branding
		// and deliberately ignore the brand primary colour (coloured text prints faint). Black-on-white.
		brand := h.branding(r.Context(), tid)
		htmlOut := generateReceiptHTML(receipt, brand.LogoURL)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="receipt-%s.html"`, order.OrderNumber))
		_, _ = w.Write(htmlOut)
		return
	}

	jsonOK(w, receipt)
}

// isCashMethod reports whether a payment method is an anonymous cash tender (no identified
// customer). Everything else — M-Pesa, card, mobile money, bank, online — identifies a payer.
func isCashMethod(method string) bool {
	m := strings.ToLower(strings.TrimSpace(method))
	return m == "" || m == "cash" || m == "cash_on_delivery" || m == "cod"
}

// GetReceiptHTML handles GET /{tenantID}/pos/orders/{orderID}/receipt/html
func (h *ReceiptHandler) GetReceiptHTML(w http.ResponseWriter, r *http.Request) {
	// Delegate to GetReceipt with format=html
	q := r.URL.Query()
	q.Set("format", "html")
	r.URL.RawQuery = q.Encode()
	h.GetReceipt(w, r)
}

// GetReceiptPDF handles GET /{tenantID}/pos/orders/{orderID}/receipt/pdf
// Returns printable HTML (80mm thermal width). True PDF generation requires
// a headless browser; for now returns HTML with print-optimised CSS.
func (h *ReceiptHandler) GetReceiptPDF(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	q.Set("format", "pdf")
	r.URL.RawQuery = q.Encode()
	h.GetReceipt(w, r)
}

// formatReceiptTime renders the receipt timestamp in the outlet's local timezone.
// order.CreatedAt is stored in UTC; without this conversion the printed time is offset by
// the UTC delta (the "wrong time, correct date" bug). Falls back to Africa/Nairobi, then UTC.
func formatReceiptTime(t time.Time, tz string) string {
	if tz == "" {
		tz = "Africa/Nairobi"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("02 Jan 2006  15:04")
}

// generateReceiptHTML generates a printable thermal-width HTML receipt.
// Designed for window.print() and direct browser print-to-PDF. Honours the outlet's configured
// receipt header/footer text, VAT rate, and paper width (58mm | 80mm).
func generateReceiptHTML(rec receiptResponse, logoURL string) []byte {
	// Paper width drives the @page size and body width. Default 80mm.
	pageWidth, bodyWidth := "80mm", "72mm"
	if rec.PaperWidth == "58mm" {
		pageWidth, bodyWidth = "58mm", "50mm"
	}

	var buf bytes.Buffer
	buf.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8">`)
	buf.WriteString(`<title>Receipt ` + rec.ReceiptNumber + `</title>`)
	// White sheet, pure-black bold ink, colours kept exact (no brand colour) — thermal/non-colour
	// printers render gray/coloured text faint, so everything is black on white. The logo is the only
	// image and is grayscaled so a colour logo still prints crisply.
	buf.WriteString(fmt.Sprintf(`<style>
@page{size:%s auto;margin:4mm}
*{box-sizing:border-box}
body{font-family:'Courier New',Courier,'DejaVu Sans Mono',monospace;font-size:12px;font-weight:bold;color:#000;background:#fff;width:%s;margin:0 auto;padding:4px;-webkit-print-color-adjust:exact;print-color-adjust:exact}
body,body *{color:#000}
.logo{display:block;margin:0 auto 4px;max-width:48mm;max-height:20mm;object-fit:contain;filter:grayscale(1) contrast(1.2)}
h1{font-size:13px;text-align:center;margin:2px 0}
.sub{font-size:10px;text-align:center;margin:1px 0;color:#000}
.hdr{font-size:10px;text-align:center;margin:2px 0;white-space:pre-wrap}
.ftr{font-size:10px;text-align:center;margin:2px 0;white-space:pre-wrap}
.center{text-align:center}
.line{display:flex;justify-content:space-between;margin:1px 0}
.divider{border-top:1px dashed #000;margin:4px 0}
.bold{font-weight:bold}
.etims-qr{display:block;margin:4px auto;width:80px;height:80px}
.etims-num{font-size:9px;text-align:center;word-break:break-all}
@media print{body{width:100%%}}
</style></head><body>`, pageWidth, bodyWidth))
	if logoURL != "" {
		buf.WriteString(fmt.Sprintf(`<img class="logo" src="%s" alt="logo">`, htmlEscape(logoURL)))
	}
	if rec.OutletName != "" {
		buf.WriteString(fmt.Sprintf(`<h1>%s</h1>`, htmlEscape(rec.OutletName)))
	} else {
		buf.WriteString(`<h1>RECEIPT</h1>`)
	}
	if rec.OutletAddress != "" {
		buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, htmlEscape(rec.OutletAddress)))
	}
	// Custom header text configured in POS settings (business name, address, slogan…).
	if rec.ReceiptHeader != "" {
		buf.WriteString(fmt.Sprintf(`<p class="hdr">%s</p>`, htmlEscape(rec.ReceiptHeader)))
	}
	buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, formatReceiptTime(rec.IssuedAt, rec.Timezone)))
	buf.WriteString(fmt.Sprintf(`<p class="sub">Receipt: %s</p>`, rec.ReceiptNumber))
	if rec.BillTo != "" {
		buf.WriteString(fmt.Sprintf(`<p class="sub">Customer: %s</p>`, htmlEscape(rec.BillTo)))
	}
	if rec.ServedBy != "" {
		buf.WriteString(fmt.Sprintf(`<p class="sub">Served by: %s</p>`, htmlEscape(rec.ServedBy)))
	}
	buf.WriteString(`<div class="divider"></div>`)
	for _, l := range rec.Lines {
		// A zero-charge line (complimentary accompaniment / bundled side) prints "FREE" rather than
		// "0.00" so the bill clearly shows it was included at no charge.
		amount := fmt.Sprintf("%.2f", l.TotalPrice)
		if l.TotalPrice == 0 {
			amount = "FREE"
		}
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s x%.0f</span><span>%s</span></div>`, htmlEscape(l.Name), l.Quantity, amount))
	}
	buf.WriteString(`<div class="divider"></div>`)
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>Subtotal</span><span>%.2f</span></div>`, rec.Subtotal))
	taxLabel := "Tax"
	if rec.VatEnabled && rec.VatRate > 0 {
		taxLabel = fmt.Sprintf("VAT (%g%%)", rec.VatRate)
	}
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s</span><span>%.2f</span></div>`, taxLabel, rec.TaxAmount))
	if rec.DiscountAmount > 0 {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>Discount</span><span>-%.2f</span></div>`, rec.DiscountAmount))
	}
	buf.WriteString(fmt.Sprintf(`<div class="line bold"><span>TOTAL</span><span>%.2f %s</span></div>`, rec.TotalAmount, rec.Currency))
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>Paid</span><span>%.2f</span></div>`, rec.AmountPaid))
	if rec.EtimsInvoiceNumber != "" || rec.EtimsQRCodeURL != "" {
		buf.WriteString(`<div class="divider"></div>`)
		if rec.EtimsQRCodeURL != "" {
			buf.WriteString(fmt.Sprintf(`<img class="etims-qr" src="%s" alt="eTIMS QR">`, rec.EtimsQRCodeURL))
		}
		if rec.EtimsInvoiceNumber != "" {
			buf.WriteString(fmt.Sprintf(`<p class="etims-num">CU No: %s</p>`, rec.EtimsInvoiceNumber))
		}
	}
	if pm := rec.PaymentMethods; pm != nil {
		buf.WriteString(`<div class="divider"></div>`)
		buf.WriteString(`<p class="bold" style="font-size:10px;text-align:center">HOW TO PAY</p>`)
		if pm.MpesaPaybill != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>M-PESA Paybill</span><span>%s</span></div>`, pm.MpesaPaybill))
		}
		if pm.MpesaAccountRef != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>Account No.</span><span>%s</span></div>`, pm.MpesaAccountRef))
		}
		if pm.MpesaTill != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>M-PESA Till</span><span>%s</span></div>`, pm.MpesaTill))
		}
		if pm.MpesaPochi != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>M-PESA Pochi</span><span>%s</span></div>`, pm.MpesaPochi))
		}
		if pm.BankName != "" || pm.BankAccountNumber != "" {
			label := pm.BankName
			if label == "" {
				label = "Bank"
			}
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s</span><span>%s</span></div>`, label, pm.BankAccountNumber))
		}
		if pm.BankAccountName != "" {
			buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, pm.BankAccountName))
		}
	}
	buf.WriteString(`<div class="divider"></div>`)
	// Custom footer text configured in POS settings; fall back to a friendly default.
	footer := rec.ReceiptFooter
	if footer == "" {
		footer = "Thank you for your business!"
	}
	buf.WriteString(fmt.Sprintf(`<p class="ftr">%s</p>`, htmlEscape(footer)))
	buf.WriteString(`</body></html>`)
	return buf.Bytes()
}

// htmlEscape escapes user-configured text (header/footer/item names) before embedding it in the
// receipt HTML, preventing layout breakage or injection from settings values.
func htmlEscape(s string) string { return html.EscapeString(s) }
