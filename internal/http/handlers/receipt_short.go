package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/bengobox/pos-service/internal/shorttoken"
)

// RegisterShortReceiptRoute mounts the shortened public receipt-download link at /api/v1/r/{code}
// — always serves the PDF as an attachment (that IS the point of this link: "download your
// receipt"), reusing the exact same buildReceiptForOrder/writeReceiptResponse pipeline as the
// long-form /api/v1/public/receipts/{token}/pdf route, zero rendering logic duplicated. The code
// is just shorttoken.Encode(order.PublicToken) — same capability token, same security model, just
// a shorter, friendlier string for a link a customer is asked to tap (see internal/shorttoken).
func (h *ReceiptHandler) RegisterShortReceiptRoute(r chi.Router) {
	r.Get("/{code}", h.GetShortReceiptPDF)
}

func (h *ReceiptHandler) GetShortReceiptPDF(w http.ResponseWriter, r *http.Request) {
	token, err := shorttoken.Decode(chi.URLParam(r, "code"))
	if err != nil {
		jsonError(w, "invalid link", http.StatusBadRequest)
		return
	}
	order, err := h.loadReceiptOrderByPublicToken(r.Context(), token)
	if err != nil {
		jsonError(w, "receipt not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	receipt, brand := h.buildReceiptForOrder(ctx, order.TenantID, order, r)
	writeReceiptResponse(w, h.log, receipt, brand, order.OrderNumber, "pdf", true)
}
