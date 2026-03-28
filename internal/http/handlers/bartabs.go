package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/bartab"
)

// BarTabHandler handles bar tab lifecycle endpoints.
type BarTabHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewBarTabHandler(log *zap.Logger, client *ent.Client) *BarTabHandler {
	return &BarTabHandler{log: log, client: client}
}

type openBarTabInput struct {
	OutletID     uuid.UUID `json:"outletId"`
	CustomerName string    `json:"customerName"`
	LimitAmount  float64   `json:"limitAmount"`
}

// OpenBarTab handles POST /{tenantID}/pos/bar-tabs
func (h *BarTabHandler) OpenBarTab(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input openBarTabInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	tab, err := h.client.BarTab.Create().
		SetTenantID(tid).
		SetOutletID(input.OutletID).
		SetTabName(input.CustomerName).
		SetStatus("open").
		SetTotalAmount(0).
		Save(r.Context())
	if err != nil {
		h.log.Error("open bar tab failed", zap.Error(err))
		jsonError(w, "failed to open tab", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, tab)
}

// ListBarTabs handles GET /{tenantID}/pos/bar-tabs
func (h *BarTabHandler) ListBarTabs(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	query := h.client.BarTab.Query().Where(bartab.TenantID(tid)).WithEvents()

	if status := r.URL.Query().Get("status"); status != "" {
		query = query.Where(bartab.Status(status))
	}

	tabs, err := query.Order(ent.Desc(bartab.FieldCreatedAt)).All(r.Context())
	if err != nil {
		h.log.Error("list bar tabs failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": tabs, "total": len(tabs)})
}

// GetBarTab handles GET /{tenantID}/pos/bar-tabs/{id}
func (h *BarTabHandler) GetBarTab(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	tabID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid tab id", http.StatusBadRequest)
		return
	}

	tab, err := h.client.BarTab.Query().
		Where(bartab.ID(tabID), bartab.TenantID(tid)).
		WithEvents().
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "tab not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, tab)
}

// CloseBarTab handles POST /{tenantID}/pos/bar-tabs/{id}/close
func (h *BarTabHandler) CloseBarTab(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	tabID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid tab id", http.StatusBadRequest)
		return
	}

	tab, err := h.client.BarTab.Query().
		Where(bartab.ID(tabID), bartab.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "tab not found", http.StatusNotFound)
		return
	}

	updated, err := tab.Update().
		SetStatus("closed").
		Save(r.Context())
	if err != nil {
		jsonError(w, "close failed", http.StatusInternalServerError)
		return
	}

	jsonOK(w, updated)
}
