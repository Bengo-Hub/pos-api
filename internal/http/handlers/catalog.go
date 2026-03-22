package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/catalogitem"
)

// CatalogHandler handles catalog item CRUD endpoints.
type CatalogHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewCatalogHandler(log *zap.Logger, client *ent.Client) *CatalogHandler {
	return &CatalogHandler{log: log, client: client}
}

// ListCatalogItems handles GET /{tenantID}/pos/catalog/items
func (h *CatalogHandler) ListCatalogItems(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	query := h.client.CatalogItem.Query().Where(catalogitem.TenantID(tid))

	if cat := r.URL.Query().Get("category"); cat != "" {
		query = query.Where(catalogitem.Category(cat))
	}
	if status := r.URL.Query().Get("status"); status != "" {
		query = query.Where(catalogitem.Status(status))
	} else {
		query = query.Where(catalogitem.Status("active"))
	}
	if search := r.URL.Query().Get("search"); search != "" {
		query = query.Where(catalogitem.NameContainsFold(search))
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * limit

	total, _ := query.Clone().Count(r.Context())

	items, err := query.
		Offset(offset).
		Limit(limit).
		Order(ent.Asc(catalogitem.FieldName)).
		All(r.Context())
	if err != nil {
		h.log.Error("list catalog items failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": items, "total": total, "limit": limit, "page": page})
}

type createCatalogItemInput struct {
	SKU       string `json:"sku"`
	Name      string `json:"name"`
	Category  string `json:"category"`
	TaxStatus string `json:"taxStatus"`
	Status    string `json:"status"`
	Barcode   string `json:"barcode,omitempty"`
}

// CreateCatalogItem handles POST /{tenantID}/pos/catalog/items
func (h *CatalogHandler) CreateCatalogItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createCatalogItemInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Name == "" || input.SKU == "" {
		jsonError(w, "name and sku are required", http.StatusBadRequest)
		return
	}
	if input.TaxStatus == "" {
		input.TaxStatus = "taxable"
	}
	if input.Status == "" {
		input.Status = "active"
	}

	builder := h.client.CatalogItem.Create().
		SetTenantID(tid).
		SetSku(input.SKU).
		SetName(input.Name).
		SetCategory(input.Category).
		SetTaxStatus(input.TaxStatus).
		SetStatus(input.Status)

	item, err := builder.Save(r.Context())
	if err != nil {
		h.log.Error("create catalog item failed", zap.Error(err))
		jsonError(w, "failed to create item: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, item)
}

// GetCatalogItem handles GET /{tenantID}/pos/catalog/items/{id}
func (h *CatalogHandler) GetCatalogItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid item id", http.StatusBadRequest)
		return
	}

	item, err := h.client.CatalogItem.Query().
		Where(catalogitem.ID(itemID), catalogitem.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "item not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, item)
}

// UpdateCatalogItem handles PUT /{tenantID}/pos/catalog/items/{id}
func (h *CatalogHandler) UpdateCatalogItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid item id", http.StatusBadRequest)
		return
	}

	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	item, err := h.client.CatalogItem.Query().
		Where(catalogitem.ID(itemID), catalogitem.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "item not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	updater := item.Update()
	if v, ok := input["name"].(string); ok {
		updater.SetName(v)
	}
	if v, ok := input["category"].(string); ok {
		updater.SetCategory(v)
	}
	if v, ok := input["status"].(string); ok {
		updater.SetStatus(v)
	}
	if v, ok := input["taxStatus"].(string); ok {
		updater.SetTaxStatus(v)
	}

	updated, err := updater.Save(r.Context())
	if err != nil {
		jsonError(w, "update failed", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// DeleteCatalogItem handles DELETE /{tenantID}/pos/catalog/items/{id} (soft delete)
func (h *CatalogHandler) DeleteCatalogItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid item id", http.StatusBadRequest)
		return
	}

	_, err = h.client.CatalogItem.Update().
		Where(catalogitem.ID(itemID), catalogitem.TenantID(tid)).
		SetStatus("inactive").
		Save(r.Context())
	if err != nil {
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// parseTenantUUID extracts and parses tenant UUID from httpware context.
// Platform owners can override via ?tenantId= query param for cross-tenant access.
func parseTenantUUID(r *http.Request) (uuid.UUID, error) {
	ctx := r.Context()

	// Platform owner query-param override
	if httpware.IsPlatformOwner(ctx) {
		if q := r.URL.Query().Get("tenantId"); q != "" {
			return uuid.Parse(q)
		}
	}

	// Standard: httpware context (from TenantV2 middleware)
	tenantIDStr := httpware.GetTenantID(ctx)
	if tenantIDStr != "" {
		if httpware.IsPlatformOwner(ctx) {
			claims, ok := authclient.ClaimsFromContext(ctx)
			if ok && claims.TenantID == tenantIDStr {
				// Platform owner's own tenant — return Nil to indicate "all"
				return uuid.Nil, nil
			}
		}
		return uuid.Parse(tenantIDStr)
	}

	return uuid.Nil, fmt.Errorf("tenant context required")
}
