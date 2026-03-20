package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entsection "github.com/bengobox/pos-service/internal/ent/section"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tableassignment"
)

// TableHandler handles table and section management endpoints.
type TableHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewTableHandler(log *zap.Logger, client *ent.Client) *TableHandler {
	return &TableHandler{log: log, client: client}
}

// ListSections handles GET /{tenantID}/pos/sections
func (h *TableHandler) ListSections(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	sections, err := h.client.Section.Query().
		Where(entsection.TenantID(tid), entsection.IsActive(true)).
		WithTables().
		Order(ent.Asc(entsection.FieldSortOrder)).
		All(r.Context())
	if err != nil {
		h.log.Error("list sections failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": sections, "total": len(sections)})
}

type createSectionInput struct {
	OutletID    uuid.UUID `json:"outletId"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	SectionType string    `json:"sectionType"`
	FloorNumber int       `json:"floorNumber"`
	SortOrder   int       `json:"sortOrder"`
}

// CreateSection handles POST /{tenantID}/pos/sections
func (h *TableHandler) CreateSection(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createSectionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	sec, err := h.client.Section.Create().
		SetTenantID(tid).
		SetOutletID(input.OutletID).
		SetName(input.Name).
		SetSlug(input.Slug).
		SetSectionType(entsection.SectionType(input.SectionType)).
		SetFloorNumber(input.FloorNumber).
		SetSortOrder(input.SortOrder).
		SetIsActive(true).
		Save(r.Context())
	if err != nil {
		h.log.Error("create section failed", zap.Error(err))
		jsonError(w, "failed to create section", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, sec)
}

// UpdateSection handles PUT /{tenantID}/pos/sections/{id}
func (h *TableHandler) UpdateSection(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	secID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid section id", http.StatusBadRequest)
		return
	}

	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	sec, err := h.client.Section.Query().
		Where(entsection.ID(secID), entsection.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "section not found", http.StatusNotFound)
		return
	}

	updater := sec.Update()
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

// ListTables handles GET /{tenantID}/pos/tables
func (h *TableHandler) ListTables(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	query := h.client.Table.Query().Where(enttable.TenantID(tid)).WithSection()

	if status := r.URL.Query().Get("status"); status != "" {
		query = query.Where(enttable.Status(status))
	}
	if sectionID := r.URL.Query().Get("section_id"); sectionID != "" {
		sid, err := uuid.Parse(sectionID)
		if err == nil {
			query = query.Where(enttable.SectionID(sid))
		}
	}

	tables, err := query.
		Order(ent.Asc(enttable.FieldName)).
		All(r.Context())
	if err != nil {
		h.log.Error("list tables failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": tables, "total": len(tables)})
}

type createTableInput struct {
	OutletID  uuid.UUID `json:"outletId"`
	SectionID uuid.UUID `json:"sectionId"`
	Name      string    `json:"name"`
	Capacity  int       `json:"capacity"`
	TableType string    `json:"tableType"`
	XPosition float64   `json:"xPosition"`
	YPosition float64   `json:"yPosition"`
	Tags      []string  `json:"tags"`
}

// CreateTable handles POST /{tenantID}/pos/tables
func (h *TableHandler) CreateTable(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createTableInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	builder := h.client.Table.Create().
		SetTenantID(tid).
		SetOutletID(input.OutletID).
		SetName(input.Name).
		SetCapacity(input.Capacity).
		SetStatus("available")

	if input.SectionID != uuid.Nil {
		builder.SetSectionID(input.SectionID)
	}
	if input.TableType != "" {
		builder.SetTableType(enttable.TableType(input.TableType))
	}
	if input.XPosition > 0 {
		builder.SetXPosition(input.XPosition)
	}
	if input.YPosition > 0 {
		builder.SetYPosition(input.YPosition)
	}
	if input.Tags != nil {
		builder.SetTags(input.Tags)
	}

	tbl, err := builder.Save(r.Context())
	if err != nil {
		h.log.Error("create table failed", zap.Error(err))
		jsonError(w, "failed to create table", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, tbl)
}

// UpdateTable handles PUT /{tenantID}/pos/tables/{id}
func (h *TableHandler) UpdateTable(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	tableID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid table id", http.StatusBadRequest)
		return
	}

	tbl, err := h.client.Table.Query().
		Where(enttable.ID(tableID), enttable.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "table not found", http.StatusNotFound)
		return
	}

	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	updater := tbl.Update()
	if v, ok := input["name"].(string); ok {
		updater.SetName(v)
	}
	if v, ok := input["capacity"].(float64); ok {
		updater.SetCapacity(int(v))
	}
	if v, ok := input["status"].(string); ok {
		updater.SetStatus(v)
	}

	updated, err := updater.Save(r.Context())
	if err != nil {
		jsonError(w, "update failed", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// UpdateTableStatus handles PATCH /{tenantID}/pos/tables/{id}/status
func (h *TableHandler) UpdateTableStatus(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	tableID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid table id", http.StatusBadRequest)
		return
	}

	var input struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	updated, err := h.client.Table.Update().
		Where(enttable.ID(tableID), enttable.TenantID(tid)).
		SetStatus(input.Status).
		Save(r.Context())
	if err != nil {
		jsonError(w, "update status failed", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"updated": updated})
}

// AssignTable handles POST /{tenantID}/pos/tables/{id}/assign
func (h *TableHandler) AssignTable(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	tableID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid table id", http.StatusBadRequest)
		return
	}

	var input struct {
		OrderID uuid.UUID `json:"orderId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Update table status to occupied
	_, err = h.client.Table.Update().
		Where(enttable.ID(tableID), enttable.TenantID(tid)).
		SetStatus("occupied").
		Save(r.Context())
	if err != nil {
		jsonError(w, "failed to update table", http.StatusInternalServerError)
		return
	}

	// Create assignment
	assignment, err := h.client.TableAssignment.Create().
		SetTableID(tableID).
		SetOrderID(input.OrderID).
		SetAssignedAt(time.Now()).
		Save(r.Context())
	if err != nil {
		h.log.Error("create assignment failed", zap.Error(err))
		jsonError(w, "failed to assign table", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, assignment)
}

// ReleaseTable handles POST /{tenantID}/pos/tables/{id}/release
func (h *TableHandler) ReleaseTable(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	tableID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid table id", http.StatusBadRequest)
		return
	}

	// Set table back to available
	_, err = h.client.Table.Update().
		Where(enttable.ID(tableID), enttable.TenantID(tid)).
		SetStatus("available").
		Save(r.Context())
	if err != nil {
		jsonError(w, "failed to release table", http.StatusInternalServerError)
		return
	}

	// Close active assignments
	now := time.Now()
	_, _ = h.client.TableAssignment.Update().
		Where(tableassignment.TableID(tableID), tableassignment.ReleasedAtIsNil()).
		SetReleasedAt(now).
		Save(r.Context())

	jsonOK(w, map[string]string{"status": "released"})
}
