package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/documents"
)

// ─── Document numbering settings ────────────────────────────────────────────
// Per-tenant, per-doc-type sequence configuration (prefix / date format / padding)
// that drives POS order, receipt, return, reversal and repair-job numbers. Mirrors
// inventory/treasury-ui's document-numbering settings. Platform default is numeric.

// DocumentSequenceHandler exposes the tenant-configurable POS document-numbering settings.
type DocumentSequenceHandler struct {
	log *zap.Logger
	seq *documents.SequenceService
}

// NewDocumentSequenceHandler creates a DocumentSequenceHandler.
func NewDocumentSequenceHandler(log *zap.Logger, seq *documents.SequenceService) *DocumentSequenceHandler {
	return &DocumentSequenceHandler{log: log, seq: seq}
}

// RegisterRoutes wires the document-sequence config endpoints. `manage`, when non-nil, gates the
// mutating PUT with the caller's permission middleware (the same config gate other POS settings use).
func (h *DocumentSequenceHandler) RegisterRoutes(r chi.Router, manage func(http.Handler) http.Handler) {
	r.Get("/pos/document-sequences", h.List)
	r.Get("/pos/document-sequences/{docType}/preview", h.Preview)
	if manage != nil {
		r.With(manage).Put("/pos/document-sequences/{docType}", h.Update)
	} else {
		r.Put("/pos/document-sequences/{docType}", h.Update)
	}
}

// List returns every POS doc type's sequence config (auto-seeded numeric defaults).
//
//	@Summary		List POS document-numbering configs
//	@Description	Per-doc-type sequence configuration (numeric by default) for order, receipt, return, reversal and repair-job numbers.
//	@Tags			document-numbering
//	@Produce		json
//	@Security		bearerAuth
//	@Param			tenantID	path		string	true	"Tenant ID"
//	@Success		200			{object}	map[string]interface{}
//	@Router			/{tenantID}/pos/document-sequences [get]
func (h *DocumentSequenceHandler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}
	if h.seq == nil {
		jsonOK(w, map[string]any{"data": []any{}})
		return
	}
	rows, err := h.seq.ListConfigs(r.Context(), tenantID)
	if err != nil {
		jsonError(w, "failed to load document sequences", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": rows})
}

// Preview returns the next number for a doc type without consuming it.
//
//	@Summary		Preview next POS document number
//	@Description	Returns the next number for a doc type without incrementing the counter.
//	@Tags			document-numbering
//	@Produce		json
//	@Security		bearerAuth
//	@Param			tenantID	path		string	true	"Tenant ID"
//	@Param			docType		path		string	true	"Document type (order, pos_receipt, pos_return, pos_reversal, repair_job)"
//	@Success		200			{object}	map[string]string
//	@Router			/{tenantID}/pos/document-sequences/{docType}/preview [get]
func (h *DocumentSequenceHandler) Preview(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}
	if h.seq == nil {
		jsonError(w, "document service unavailable", http.StatusServiceUnavailable)
		return
	}
	n, err := h.seq.PreviewNext(r.Context(), tenantID, chi.URLParam(r, "docType"))
	if err != nil {
		jsonError(w, "failed to preview number", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"next_number": n})
}

// Update changes the format config (prefix/date/padding) for a doc type; never the counter.
//
//	@Summary		Update a POS document-numbering config
//	@Description	Sets prefix/separator/date format/padding for a doc type. Empty prefix + empty date_format = pure numeric.
//	@Tags			document-numbering
//	@Accept			json
//	@Produce		json
//	@Security		bearerAuth
//	@Param			tenantID	path		string					true	"Tenant ID"
//	@Param			docType		path		string					true	"Document type"
//	@Param			body		body		documents.SeqConfigDTO	true	"Format settings"
//	@Success		200			{object}	documents.SeqConfigDTO
//	@Router			/{tenantID}/pos/document-sequences/{docType} [put]
func (h *DocumentSequenceHandler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}
	if h.seq == nil {
		jsonError(w, "document service unavailable", http.StatusServiceUnavailable)
		return
	}
	var cfg documents.SeqConfigDTO
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	out, err := h.seq.UpdateConfig(r.Context(), tenantID, chi.URLParam(r, "docType"), cfg)
	if err != nil {
		jsonError(w, "failed to update document sequence", http.StatusInternalServerError)
		return
	}
	jsonOK(w, out)
}
