package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/tender"
)

// TenderHandler handles tender (payment method) management.
type TenderHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewTenderHandler(log *zap.Logger, client *ent.Client) *TenderHandler {
	return &TenderHandler{log: log, client: client}
}

// ListTenders handles GET /{tenantID}/pos/tenders
func (h *TenderHandler) ListTenders(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	tenders, err := h.client.Tender.Query().
		Where(tender.TenantID(tid)).
		Order(ent.Asc(tender.FieldName)).
		All(r.Context())
	if err != nil {
		h.log.Error("list tenders failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": tenders, "total": len(tenders)})
}

type createTenderInput struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // cash, card, mobile, manual
	IsActive bool   `json:"isActive"`
}

// CreateTender handles POST /{tenantID}/pos/tenders
func (h *TenderHandler) CreateTender(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createTenderInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Name == "" || input.Type == "" {
		jsonError(w, "name and type are required", http.StatusBadRequest)
		return
	}

	t, err := h.client.Tender.Create().
		SetTenantID(tid).
		SetName(input.Name).
		SetType(input.Type).
		SetIsActive(input.IsActive).
		Save(r.Context())
	if err != nil {
		h.log.Error("create tender failed", zap.Error(err))
		jsonError(w, "failed to create tender", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, t)
}

// UpdateTender handles PUT /{tenantID}/pos/tenders/{id}
func (h *TenderHandler) UpdateTender(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	tenderID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid tender id", http.StatusBadRequest)
		return
	}

	t, err := h.client.Tender.Query().
		Where(tender.ID(tenderID), tender.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "tender not found", http.StatusNotFound)
		return
	}

	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	updater := t.Update()
	if v, ok := input["name"].(string); ok {
		updater.SetName(v)
	}
	if v, ok := input["isActive"].(bool); ok {
		updater.SetIsActive(v)
	}

	updated, err := updater.Save(r.Context())
	if err != nil {
		jsonError(w, "update failed", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}
