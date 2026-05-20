package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/catalogitem"
)

// BarcodeLookup handles GET /{tenantID}/pos/catalog/barcode/{barcode}
// Returns a single CatalogItem matching the barcode scoped to the tenant.
func (h *CatalogHandler) BarcodeLookup(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	barcode := chi.URLParam(r, "barcode")
	if barcode == "" {
		jsonError(w, "barcode is required", http.StatusBadRequest)
		return
	}

	item, err := h.client.CatalogItem.Query().
		Where(
			catalogitem.TenantID(tid),
			catalogitem.Barcode(barcode),
			catalogitem.Status("active"),
		).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "item not found", http.StatusNotFound)
			return
		}
		h.log.Error("barcode lookup failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, item)
}
