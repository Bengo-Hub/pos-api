package handlers

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
)

// ReceiptHandler handles receipt generation endpoints.
type ReceiptHandler struct {
	log    *zap.Logger
	client *ent.Client
}

// NewReceiptHandler creates a new ReceiptHandler.
func NewReceiptHandler(log *zap.Logger, client *ent.Client) *ReceiptHandler {
	return &ReceiptHandler{log: log, client: client}
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
	AirtelMoneyNumber string `json:"airtel_money_number,omitempty"`
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
	IssuedAt           time.Time              `json:"issued_at"`
	Lines              []receiptLine          `json:"lines"`
	Subtotal           float64                `json:"subtotal"`
	TaxAmount          float64                `json:"tax_amount"`
	DiscountAmount     float64                `json:"discount_amount"`
	TotalAmount        float64                `json:"total_amount"`
	Currency           string                 `json:"currency"`
	AmountPaid         float64                `json:"amount_paid"`
	EtimsInvoiceNumber string                 `json:"etims_invoice_number,omitempty"`
	EtimsQRCodeURL     string                 `json:"etims_qr_code_url,omitempty"`
	PaymentMethods     *receiptPaymentMethods `json:"payment_methods,omitempty"`
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

	lines := make([]receiptLine, 0, len(order.Edges.Lines))
	var subtotal float64
	for _, l := range order.Edges.Lines {
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
	if len(order.Edges.Payments) > 0 {
		amountPaid = order.Edges.Payments[0].Amount
	}

	receipt := receiptResponse{
		ReceiptNumber:  fmt.Sprintf("RCT-%s", order.OrderNumber),
		OrderNumber:    order.OrderNumber,
		OutletID:       order.OutletID,
		IssuedAt:       order.CreatedAt,
		Lines:          lines,
		Subtotal:       subtotal,
		TaxAmount:      order.TaxTotal,
		DiscountAmount: order.DiscountTotal,
		TotalAmount:    order.TotalAmount,
		Currency:       order.Currency,
		AmountPaid:     amountPaid,
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
		if addr := outlet.AddressJSON; addr != nil {
			if street, ok := addr["street"].(string); ok && street != "" {
				receipt.OutletAddress = street
			} else if city, ok := addr["city"].(string); ok {
				receipt.OutletAddress = city
			}
		}
	}

	// Populate payment display info when outlet has it configured.
	if s, err := h.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(order.OutletID)).
		Only(ctx); err == nil && s.ShowPaymentInfoOnReceipt {
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
		if s.AirtelMoneyNumber != nil && *s.AirtelMoneyNumber != "" {
			pm.AirtelMoneyNumber = *s.AirtelMoneyNumber
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

	format := r.URL.Query().Get("format")
	if format == "pdf" || format == "html" {
		html := generateReceiptHTML(receipt)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="receipt-%s.html"`, order.OrderNumber))
		_, _ = w.Write(html)
		return
	}

	jsonOK(w, receipt)
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

// generateReceiptHTML generates a printable 80mm thermal-width HTML receipt.
// Designed for window.print() and direct browser print-to-PDF.
func generateReceiptHTML(rec receiptResponse) []byte {
	var buf bytes.Buffer
	buf.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8">`)
	buf.WriteString(`<title>Receipt ` + rec.ReceiptNumber + `</title>`)
	buf.WriteString(`<style>
@page{size:80mm auto;margin:4mm}
*{box-sizing:border-box}
body{font-family:monospace;font-size:11px;width:72mm;margin:0 auto;padding:4px}
h1{font-size:13px;text-align:center;margin:2px 0}
.sub{font-size:10px;text-align:center;margin:1px 0;color:#444}
.center{text-align:center}
.line{display:flex;justify-content:space-between;margin:1px 0}
.divider{border-top:1px dashed #000;margin:4px 0}
.bold{font-weight:bold}
.etims-qr{display:block;margin:4px auto;width:80px;height:80px}
.etims-num{font-size:9px;text-align:center;word-break:break-all}
@media print{body{width:100%}}
</style></head><body>`)
	if rec.OutletName != "" {
		buf.WriteString(fmt.Sprintf(`<h1>%s</h1>`, rec.OutletName))
	} else {
		buf.WriteString(`<h1>RECEIPT</h1>`)
	}
	if rec.OutletAddress != "" {
		buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, rec.OutletAddress))
	}
	buf.WriteString(fmt.Sprintf(`<p class="sub">%s</p>`, rec.IssuedAt.Format("02 Jan 2006  15:04")))
	buf.WriteString(fmt.Sprintf(`<p class="sub">Receipt: %s</p>`, rec.ReceiptNumber))
	buf.WriteString(`<div class="divider"></div>`)
	for _, l := range rec.Lines {
		buf.WriteString(fmt.Sprintf(`<div class="line"><span>%s x%.0f</span><span>%.2f</span></div>`, l.Name, l.Quantity, l.TotalPrice))
	}
	buf.WriteString(`<div class="divider"></div>`)
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>Subtotal</span><span>%.2f</span></div>`, rec.Subtotal))
	buf.WriteString(fmt.Sprintf(`<div class="line"><span>Tax</span><span>%.2f</span></div>`, rec.TaxAmount))
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
		if pm.AirtelMoneyNumber != "" {
			buf.WriteString(fmt.Sprintf(`<div class="line"><span>Airtel Money</span><span>%s</span></div>`, pm.AirtelMoneyNumber))
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
	buf.WriteString(`<p class="center" style="font-size:10px">Thank you for your business</p>`)
	buf.WriteString(`</body></html>`)
	return buf.Bytes()
}
