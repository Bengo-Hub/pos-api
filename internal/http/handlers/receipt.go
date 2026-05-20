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

// receiptResponse is the full receipt payload.
type receiptResponse struct {
	ReceiptNumber      string        `json:"receipt_number"`
	OrderNumber        string        `json:"order_number"`
	OutletID           uuid.UUID     `json:"outlet_id"`
	IssuedAt           time.Time     `json:"issued_at"`
	Lines              []receiptLine `json:"lines"`
	Subtotal           float64       `json:"subtotal"`
	TaxAmount          float64       `json:"tax_amount"`
	DiscountAmount     float64       `json:"discount_amount"`
	TotalAmount        float64       `json:"total_amount"`
	Currency           string        `json:"currency"`
	AmountPaid         float64       `json:"amount_paid"`
	EtimsInvoiceNumber string        `json:"etims_invoice_number,omitempty"`
	EtimsQRCodeURL     string        `json:"etims_qr_code_url,omitempty"`
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

	format := r.URL.Query().Get("format")
	if format == "pdf" {
		html := generateReceiptHTML(receipt)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="receipt-%s.html"`, order.OrderNumber))
		_, _ = w.Write(html)
		return
	}

	jsonOK(w, receipt)
}

// generateReceiptHTML generates a printable HTML receipt.
// Returns an HTML document suitable for window.print() in the browser.
func generateReceiptHTML(rec receiptResponse) []byte {
	var buf bytes.Buffer
	buf.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8">`)
	buf.WriteString(`<title>Receipt ` + rec.ReceiptNumber + `</title>`)
	buf.WriteString(`<style>
body{font-family:monospace;font-size:12px;max-width:300px;margin:0 auto;padding:8px}
h1{font-size:14px;text-align:center;margin:0}
.center{text-align:center}
.line{display:flex;justify-content:space-between}
.divider{border-top:1px dashed #000;margin:4px 0}
.bold{font-weight:bold}
</style></head><body>`)
	buf.WriteString(`<h1>RECEIPT</h1>`)
	buf.WriteString(fmt.Sprintf(`<p class="center">%s</p>`, rec.IssuedAt.Format("2006-01-02 15:04:05")))
	buf.WriteString(fmt.Sprintf(`<p class="center">Order: %s</p>`, rec.OrderNumber))
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
	if rec.EtimsInvoiceNumber != "" {
		buf.WriteString(`<div class="divider"></div>`)
		buf.WriteString(fmt.Sprintf(`<p class="center">eTIMS: %s</p>`, rec.EtimsInvoiceNumber))
	}
	buf.WriteString(`</body></html>`)
	return buf.Bytes()
}
