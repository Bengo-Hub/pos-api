package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// RegisterPublicRoutes mounts the unauthenticated, capability-token-based receipt-share routes
// (the WhatsApp/Email/SMS "download your receipt" links) — mirrors treasury-api's
// PublicInvoicing shape. The order's public_token IS the auth here: no tenant_id in the URL and
// no JWT is required or accepted. Mount under /api/v1/public (see router.go), registered before
// the authenticated /api/v1/{tenantID} block.
func (h *ReceiptHandler) RegisterPublicRoutes(r chi.Router) {
	r.Get("/receipts/{token}", h.GetPublicReceipt)
	r.Get("/receipts/{token}/pdf", h.GetPublicReceiptPDF)
}

// resolvePublicReceiptOrder parses the {token} URL param and resolves the order it points to.
// Shared by GetPublicReceipt and GetPublicReceiptPDF.
func (h *ReceiptHandler) resolvePublicReceiptOrder(w http.ResponseWriter, r *http.Request) (*ent.POSOrder, bool) {
	token, err := uuid.Parse(chi.URLParam(r, "token"))
	if err != nil {
		jsonError(w, "invalid token", http.StatusBadRequest)
		return nil, false
	}
	order, err := h.loadReceiptOrderByPublicToken(r.Context(), token)
	if err != nil {
		jsonError(w, "receipt not found", http.StatusNotFound)
		return nil, false
	}
	return order, true
}

// GetPublicReceipt handles GET /api/v1/public/receipts/{token} — the public-safe JSON view of a
// sale receipt, resolved purely by its public_token capability link (WhatsApp/Email/SMS share).
// Reuses the exact same receiptResponse the authenticated endpoint returns — already
// customer-safe (a receipt view never carries internal cost/margin fields) — via
// buildReceiptForOrder, so zero rendering logic is duplicated between the two surfaces.
func (h *ReceiptHandler) GetPublicReceipt(w http.ResponseWriter, r *http.Request) {
	order, ok := h.resolvePublicReceiptOrder(w, r)
	if !ok {
		return
	}
	receipt, _ := h.buildReceiptForOrder(r.Context(), order.TenantID, order, r)
	jsonOK(w, receipt)
}

// GetPublicReceiptPDF handles GET /api/v1/public/receipts/{token}/pdf — streams the identical PDF
// the authenticated download produces (same buildReceiptForOrder + layouts.RenderPDF pipeline).
// Defaults to an inline preview so a tapped WhatsApp/email link opens in the browser first;
// ?download=true forces a Save-As attachment, matching treasury-api's public document convention.
func (h *ReceiptHandler) GetPublicReceiptPDF(w http.ResponseWriter, r *http.Request) {
	order, ok := h.resolvePublicReceiptOrder(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	receipt, brand := h.buildReceiptForOrder(ctx, order.TenantID, order, r)
	download := r.URL.Query().Get("download") == "true"
	writeReceiptResponse(w, h.log, receipt, brand, order.OrderNumber, "pdf", download)
}
