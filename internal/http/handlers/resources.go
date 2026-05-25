package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entresource "github.com/bengobox/pos-service/internal/ent/resource"
)

type ResourceHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewResourceHandler(log *zap.Logger, db *ent.Client) *ResourceHandler {
	return &ResourceHandler{log: log, db: db}
}

// List handles GET /{tenantID}/pos/resources
func (h *ResourceHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.Resource.Query().Where(entresource.TenantID(tid))
	if t := r.URL.Query().Get("type"); t != "" {
		q = q.Where(entresource.TypeEQ(t))
	}
	if s := r.URL.Query().Get("status"); s != "" {
		q = q.Where(entresource.StatusEQ(entresource.Status(s)))
	}

	resources, err := q.Order(ent.Asc(entresource.FieldName)).All(r.Context())
	if err != nil {
		h.log.Error("list resources failed", zap.Error(err))
		jsonError(w, "failed to list resources", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": resources})
}

// Create handles POST /{tenantID}/pos/resources
func (h *ResourceHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		OutletID string `json:"outlet_id"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		Notes    string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}
	resourceType := input.Type
	if resourceType == "" {
		resourceType = "general"
	}

	res, err := h.db.Resource.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetName(input.Name).
		SetType(resourceType).
		SetNotes(input.Notes).
		Save(r.Context())
	if err != nil {
		h.log.Error("create resource failed", zap.Error(err))
		jsonError(w, "failed to create resource", http.StatusInternalServerError)
		return
	}
	jsonOK(w, res)
}

// PatchStatus handles PATCH /{tenantID}/pos/resources/{id}
func (h *ResourceHandler) PatchStatus(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	resID, err := uuid.Parse(chi.URLParam(r, "resourceID"))
	if err != nil {
		jsonError(w, "invalid resource id", http.StatusBadRequest)
		return
	}

	var body struct {
		Status string `json:"status"`
		Notes  string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	existing, err := h.db.Resource.Get(r.Context(), resID)
	if err != nil || existing.TenantID != tid {
		jsonError(w, "resource not found", http.StatusNotFound)
		return
	}

	upd := h.db.Resource.UpdateOneID(resID)
	if body.Status != "" {
		upd = upd.SetStatus(entresource.Status(body.Status))
	}
	if body.Notes != "" {
		upd = upd.SetNotes(body.Notes)
	}

	res, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("patch resource status failed", zap.Error(err))
		jsonError(w, "failed to update resource", http.StatusInternalServerError)
		return
	}
	jsonOK(w, res)
}
