package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	entsection "github.com/bengobox/pos-service/internal/ent/section"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tableassignment"
	entreservation "github.com/bengobox/pos-service/internal/ent/tablereservation"
	"github.com/bengobox/pos-service/internal/platform/events"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// slugify turns a display name into a URL-safe slug.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prev := '-'
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prev = r
		} else if prev != '-' {
			b.WriteRune('-')
			prev = '-'
		}
	}
	return strings.Trim(b.String(), "-")
}

// TableHandler handles table and section management endpoints.
type TableHandler struct {
	log    *zap.Logger
	client *ent.Client
	pub    *events.Publisher
}

func NewTableHandler(log *zap.Logger, client *ent.Client) *TableHandler {
	return &TableHandler{log: log, client: client}
}

// SetPublisher injects the event publisher for usage tracking events.
func (h *TableHandler) SetPublisher(pub *events.Publisher) {
	h.pub = pub
}

// ListSections handles GET /{tenantID}/pos/sections
func (h *TableHandler) ListSections(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	sectionQ := h.client.Section.Query().Where(entsection.TenantID(tid), entsection.IsActive(true))
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			sectionQ = sectionQ.Where(entsection.OutletID(oid))
		}
	}
	sections, err := sectionQ.
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
	OutletID    string `json:"outletId"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	SectionType string `json:"sectionType"`
	FloorNumber int    `json:"floorNumber"`
	SortOrder   int    `json:"sortOrder"`
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

	outletID := parseOptionalUUID(input.OutletID, r)

	slug := input.Slug
	if slug == "" {
		slug = slugify(input.Name)
	}
	sectionType := entsection.SectionTypeOther
	if input.SectionType != "" {
		sectionType = entsection.SectionType(input.SectionType)
	}

	sec, err := h.client.Section.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetName(input.Name).
		SetSlug(slug).
		SetSectionType(sectionType).
		SetFloorNumber(input.FloorNumber).
		SetSortOrder(input.SortOrder).
		SetIsActive(true).
		Save(r.Context())
	if err != nil {
		h.log.Error("create section failed", zap.Error(err))
		jsonError(w, "failed to create section: "+err.Error(), http.StatusInternalServerError)
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
// tableRow is the API response shape for a single table, extending the Ent model with occupied_since
// and live order info for occupied tables (order_id, order_number, order_total, covers).
type tableRow struct {
	*ent.Table
	OccupiedSince *string  `json:"occupied_since,omitempty"`
	OrderID       *string  `json:"order_id,omitempty"`
	OrderNumber   *string  `json:"order_number,omitempty"`
	OrderTotal    *float64 `json:"order_total,omitempty"`
	Covers        *int     `json:"covers,omitempty"`
}

func (h *TableHandler) ListTables(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	query := h.client.Table.Query().Where(enttable.TenantID(tid)).
		WithSection().
		WithAssignments(func(q *ent.TableAssignmentQuery) {
			q.Where(tableassignment.ReleasedAtIsNil()).Limit(1)
		})

	if status := r.URL.Query().Get("status"); status != "" {
		query = query.Where(enttable.Status(status))
	}
	if sectionID := r.URL.Query().Get("section_id"); sectionID != "" {
		sid, err := uuid.Parse(sectionID)
		if err == nil {
			query = query.Where(enttable.SectionID(sid))
		}
	}

	p := pagination.Parse(r)
	total, _ := query.Clone().Count(r.Context())
	tables, err := query.Order(ent.Asc(enttable.FieldName)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list tables failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Collect order IDs from active assignments so we can fetch order info in one query.
	orderIDSet := make(map[uuid.UUID]struct{})
	for _, t := range tables {
		if t.Status == "occupied" && len(t.Edges.Assignments) > 0 {
			asgn := t.Edges.Assignments[0]
			if asgn.OrderID != nil {
				orderIDSet[*asgn.OrderID] = struct{}{}
			}
		}
	}
	orderIDs := make([]uuid.UUID, 0, len(orderIDSet))
	for id := range orderIDSet {
		orderIDs = append(orderIDs, id)
	}

	type orderSummary struct {
		OrderNumber string
		TotalAmount float64
		Covers      int
	}
	orderSummaries := make(map[uuid.UUID]orderSummary)
	if len(orderIDs) > 0 {
		orders, qErr := h.client.POSOrder.Query().
			Where(entposorder.IDIn(orderIDs...)).
			Select("id", "order_number", "total_amount", "covers_count").
			All(r.Context())
		if qErr == nil {
			for _, o := range orders {
				orderSummaries[o.ID] = orderSummary{
					OrderNumber: o.OrderNumber,
					TotalAmount: o.TotalAmount,
					Covers:      o.CoversCount,
				}
			}
		}
	}

	rows := make([]tableRow, len(tables))
	for i, t := range tables {
		row := tableRow{Table: t}
		if t.Status == "occupied" && len(t.Edges.Assignments) > 0 {
			asgn := t.Edges.Assignments[0]
			ts := asgn.AssignedAt.UTC().Format(time.RFC3339)
			row.OccupiedSince = &ts
			if asgn.OrderID != nil {
				if s, ok := orderSummaries[*asgn.OrderID]; ok {
					oidStr := asgn.OrderID.String()
					row.OrderID = &oidStr
					row.OrderNumber = &s.OrderNumber
					row.OrderTotal = &s.TotalAmount
					row.Covers = &s.Covers
				}
			}
		}
		rows[i] = row
	}
	jsonOK(w, pagination.NewResponse(rows, total, p))
}

type createTableInput struct {
	OutletID  string   `json:"outletId"`
	SectionID string   `json:"sectionId"`
	Name      string   `json:"name"`
	Capacity  int      `json:"capacity"`
	TableType string   `json:"tableType"`
	XPosition float64  `json:"xPosition"`
	YPosition float64  `json:"yPosition"`
	Tags      []string `json:"tags"`
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

	// Enforce the plan's max_tables structural cap (hard-block, no overage).
	if count, cerr := h.client.Table.Query().Where(enttable.TenantID(tid)).Count(r.Context()); cerr == nil {
		if !subscriptions.CheckStructuralLimit(w, r, "tables", subscriptions.LimitTables, count) {
			return
		}
	}

	tableOutletID := parseOptionalUUID(input.OutletID, r)
	tableSectionID, _ := uuid.Parse(input.SectionID)

	builder := h.client.Table.Create().
		SetTenantID(tid).
		SetOutletID(tableOutletID).
		SetName(input.Name).
		SetCapacity(input.Capacity).
		SetStatus("available")

	if tableSectionID != uuid.Nil {
		builder.SetSectionID(tableSectionID)
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

	if h.pub != nil {
		_ = h.pub.PublishTableCreated(r.Context(), tid, map[string]any{
			"table_id":   tbl.ID.String(),
			"outlet_id":  tbl.OutletID.String(),
			"table_name": tbl.Name,
		})
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
	if v, ok := input["xPosition"].(float64); ok {
		updater.SetXPosition(v)
	}
	if v, ok := input["yPosition"].(float64); ok {
		updater.SetYPosition(v)
	}
	if v, ok := input["tableType"].(string); ok && v != "" {
		updater.SetTableType(enttable.TableType(v))
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

// TransferTable handles POST /{tenantID}/pos/tables/{id}/transfer
// Moves the active order on {id} to the destination table.
func (h *TableHandler) TransferTable(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	fromTableID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid table id", http.StatusBadRequest)
		return
	}

	var input struct {
		ToTableID uuid.UUID `json:"to_table_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.ToTableID == uuid.Nil {
		jsonError(w, "to_table_id is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Find the active assignment on the source table
	asgn, err := h.client.TableAssignment.Query().
		Where(tableassignment.TableID(fromTableID), tableassignment.ReleasedAtIsNil()).
		First(ctx)
	if err != nil || asgn.OrderID == nil {
		jsonError(w, "no active order on source table", http.StatusNotFound)
		return
	}

	// Verify destination table exists and is not occupied
	toTable, err := h.client.Table.Query().
		Where(enttable.ID(input.ToTableID), enttable.TenantID(tid)).
		Only(ctx)
	if err != nil {
		jsonError(w, "destination table not found", http.StatusNotFound)
		return
	}
	if toTable.Status == "occupied" {
		jsonError(w, "destination table is already occupied", http.StatusConflict)
		return
	}

	// Move the assignment: release source, create on destination
	now := time.Now()
	if _, err := asgn.Update().SetReleasedAt(now).Save(ctx); err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	_, err = h.client.TableAssignment.Create().
		SetTableID(input.ToTableID).
		SetNillableOrderID(asgn.OrderID).
		Save(ctx)
	if err != nil {
		jsonError(w, "failed to create assignment on destination", http.StatusInternalServerError)
		return
	}

	// Update table statuses
	h.client.Table.Update().Where(enttable.ID(fromTableID)).SetStatus("available").Exec(ctx)  //nolint
	h.client.Table.Update().Where(enttable.ID(input.ToTableID)).SetStatus("occupied").Exec(ctx) //nolint

	// Update order metadata with new table name
	h.client.POSOrder.Update().Where(entposorder.ID(*asgn.OrderID)).
		SetMetadata(map[string]any{"table_id": input.ToTableID.String(), "transferred": true}).
		Exec(ctx) //nolint

	jsonOK(w, map[string]any{"status": "transferred", "order_id": asgn.OrderID, "to_table_id": input.ToTableID})
}

// MergeTables handles POST /{tenantID}/pos/tables/merge
// Merges two active orders (one per table) into a single order on the primary table.
func (h *TableHandler) MergeTables(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		PrimaryTableID   uuid.UUID `json:"primary_table_id"`
		SecondaryTableID uuid.UUID `json:"secondary_table_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Load both active assignments
	primaryAsgn, err := h.client.TableAssignment.Query().
		Where(tableassignment.TableID(input.PrimaryTableID), tableassignment.ReleasedAtIsNil()).
		First(ctx)
	if err != nil || primaryAsgn.OrderID == nil {
		jsonError(w, "no active order on primary table", http.StatusNotFound)
		return
	}
	secondaryAsgn, err := h.client.TableAssignment.Query().
		Where(tableassignment.TableID(input.SecondaryTableID), tableassignment.ReleasedAtIsNil()).
		First(ctx)
	if err != nil || secondaryAsgn.OrderID == nil {
		jsonError(w, "no active order on secondary table", http.StatusNotFound)
		return
	}

	// Load the secondary order's lines
	secondaryLines, err := h.client.POSOrderLine.Query().
		Where(entposorderline.OrderID(*secondaryAsgn.OrderID)).
		All(ctx)
	if err != nil {
		jsonError(w, "failed to load secondary order lines", http.StatusInternalServerError)
		return
	}

	// Move all secondary lines to the primary order
	for _, l := range secondaryLines {
		if _, err := l.Update().SetOrderID(*primaryAsgn.OrderID).Save(ctx); err != nil {
			h.log.Warn("merge tables: failed to move line", zap.Error(err))
		}
	}

	// Load updated primary order to recalculate totals
	primaryOrder, err := h.client.POSOrder.Query().
		Where(entposorder.ID(*primaryAsgn.OrderID)).
		WithLines().
		Only(ctx)
	if err == nil {
		var newSubtotal, newTaxTotal float64
		for _, l := range primaryOrder.Edges.Lines {
			newSubtotal += l.TotalPrice
			if l.TaxAmount != nil {
				newTaxTotal += *l.TaxAmount
			}
		}
		primaryOrder.Update().
			SetSubtotal(newSubtotal).
			SetTaxTotal(newTaxTotal).
			SetTotalAmount(newSubtotal).
			Exec(ctx) //nolint
	}

	// Cancel the secondary order and release its table
	now := time.Now()
	h.client.POSOrder.Update().Where(entposorder.ID(*secondaryAsgn.OrderID), entposorder.TenantID(tid)).SetStatus("merged").Exec(ctx) //nolint
	secondaryAsgn.Update().SetReleasedAt(now).Exec(ctx) //nolint
	h.client.Table.Update().Where(enttable.ID(input.SecondaryTableID), enttable.TenantID(tid)).SetStatus("available").Exec(ctx) //nolint

	jsonOK(w, map[string]any{
		"status":            "merged",
		"primary_order_id":  primaryAsgn.OrderID,
		"merged_order_id":   secondaryAsgn.OrderID,
		"lines_transferred": len(secondaryLines),
	})
}

// UnmergeTables handles POST /{tenantID}/pos/tables/unmerge
// Reverses a previous table merge by splitting the specified lines out of the primary
// order into a new order and re-assigning the secondary table to that new order.
func (h *TableHandler) UnmergeTables(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		PrimaryTableID   uuid.UUID   `json:"primary_table_id"`
		SecondaryTableID uuid.UUID   `json:"secondary_table_id"`
		LineIDs          []uuid.UUID `json:"line_ids"` // lines to move back to the secondary table's new order
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.PrimaryTableID == uuid.Nil || input.SecondaryTableID == uuid.Nil || len(input.LineIDs) == 0 {
		jsonError(w, "primary_table_id, secondary_table_id, and line_ids are required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Primary table must be occupied with an active order.
	primaryAsgn, err := h.client.TableAssignment.Query().
		Where(tableassignment.TableID(input.PrimaryTableID), tableassignment.ReleasedAtIsNil()).
		First(ctx)
	if err != nil || primaryAsgn.OrderID == nil {
		jsonError(w, "no active order on primary table", http.StatusNotFound)
		return
	}

	// Secondary table must be available (released at merge time).
	secTable, err := h.client.Table.Query().
		Where(enttable.ID(input.SecondaryTableID), enttable.TenantID(tid)).
		Only(ctx)
	if err != nil {
		jsonError(w, "secondary table not found", http.StatusNotFound)
		return
	}
	if secTable.Status != "available" {
		jsonError(w, "secondary table is not available — cannot unmerge", http.StatusConflict)
		return
	}

	// Load primary order and validate the requested lines belong to it.
	primaryOrder, err := h.client.POSOrder.Query().
		Where(entposorder.ID(*primaryAsgn.OrderID), entposorder.TenantID(tid)).
		Only(ctx)
	if err != nil {
		jsonError(w, "primary order not found", http.StatusNotFound)
		return
	}

	lines, err := h.client.POSOrderLine.Query().
		Where(entposorderline.OrderID(*primaryAsgn.OrderID), entposorderline.IDIn(input.LineIDs...)).
		All(ctx)
	if err != nil || len(lines) != len(input.LineIDs) {
		jsonError(w, "one or more line_ids not found on the primary order", http.StatusBadRequest)
		return
	}

	// Calculate totals for the new (secondary) order.
	var newSubtotal, newTaxTotal float64
	for _, l := range lines {
		newSubtotal += l.TotalPrice
		if l.TaxAmount != nil {
			newTaxTotal += *l.TaxAmount
		}
	}

	// Create the new order for the secondary table.
	newOrder, err := h.client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(primaryOrder.OutletID).
		SetDeviceID(primaryOrder.DeviceID).
		SetUserID(primaryOrder.UserID).
		SetOrderNumber(primaryOrder.OrderNumber + "-U").
		SetStatus("open").
		SetSubtotal(newSubtotal).
		SetTaxTotal(newTaxTotal).
		SetDiscountTotal(0).
		SetTotalAmount(newSubtotal).
		SetOrderSubtype(primaryOrder.OrderSubtype).
		SetMetadata(map[string]any{
			"unmerged_from": primaryAsgn.OrderID.String(),
			"table_id":      input.SecondaryTableID.String(),
			"table_name":    secTable.Name,
		}).
		Save(ctx)
	if err != nil {
		h.log.Error("unmerge tables: create order failed", zap.Error(err))
		jsonError(w, "failed to create order for secondary table", http.StatusInternalServerError)
		return
	}

	// Move lines from primary to new order.
	for _, l := range lines {
		l.Update().SetOrderID(newOrder.ID).Exec(ctx) //nolint
	}

	// Recalculate primary order totals.
	remainingLines, _ := h.client.POSOrderLine.Query().Where(entposorderline.OrderID(*primaryAsgn.OrderID)).All(ctx)
	var remSubtotal, remTaxTotal float64
	for _, l := range remainingLines {
		remSubtotal += l.TotalPrice
		if l.TaxAmount != nil {
			remTaxTotal += *l.TaxAmount
		}
	}
	primaryOrder.Update().SetSubtotal(remSubtotal).SetTaxTotal(remTaxTotal).SetTotalAmount(remSubtotal).Exec(ctx) //nolint

	// Assign the secondary table to the new order and mark it occupied.
	now := time.Now()
	h.client.TableAssignment.Create().
		SetTableID(input.SecondaryTableID).
		SetOrderID(newOrder.ID).
		SetAssignedAt(now).
		Exec(ctx) //nolint
	h.client.Table.UpdateOneID(input.SecondaryTableID).SetStatus("occupied").Exec(ctx) //nolint

	jsonOK(w, map[string]any{
		"status":             "unmerged",
		"new_order_id":       newOrder.ID,
		"new_order_number":   newOrder.OrderNumber,
		"secondary_table_id": input.SecondaryTableID,
		"lines_moved":        len(lines),
	})
}

// SplitOrder handles POST /{tenantID}/pos/orders/{orderID}/split
// Creates a new order with the specified line IDs removed from the original.
func (h *TableHandler) SplitOrder(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	var input struct {
		LineIDs []uuid.UUID `json:"line_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.LineIDs) == 0 {
		jsonError(w, "line_ids is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	order, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tid)).
		Only(ctx)
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	// Verify all line IDs belong to this order
	lines, err := h.client.POSOrderLine.Query().
		Where(entposorderline.OrderID(orderID), entposorderline.IDIn(input.LineIDs...)).
		All(ctx)
	if err != nil || len(lines) != len(input.LineIDs) {
		jsonError(w, "one or more line_ids not found on this order", http.StatusBadRequest)
		return
	}

	// Create the new split order
	var splitSubtotal, splitTaxTotal float64
	for _, l := range lines {
		splitSubtotal += l.TotalPrice
		if l.TaxAmount != nil {
			splitTaxTotal += *l.TaxAmount
		}
	}

	splitOrder, err := h.client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(order.OutletID).
		SetDeviceID(order.DeviceID).
		SetUserID(order.UserID).
		SetOrderNumber(order.OrderNumber + "-S").
		SetStatus("open").
		SetSubtotal(splitSubtotal).
		SetTaxTotal(splitTaxTotal).
		SetDiscountTotal(0).
		SetTotalAmount(splitSubtotal).
		SetOrderSubtype(order.OrderSubtype).
		SetMetadata(map[string]any{"split_from": orderID.String()}).
		Save(ctx)
	if err != nil {
		h.log.Error("split order: create failed", zap.Error(err))
		jsonError(w, "failed to create split order", http.StatusInternalServerError)
		return
	}

	// Move selected lines to the new order and recalculate original
	for _, l := range lines {
		l.Update().SetOrderID(splitOrder.ID).Exec(ctx) //nolint
	}

	// Recalculate original order totals
	remainingLines, _ := h.client.POSOrderLine.Query().Where(entposorderline.OrderID(orderID)).All(ctx)
	var remSubtotal, remTaxTotal float64
	for _, l := range remainingLines {
		remSubtotal += l.TotalPrice
		if l.TaxAmount != nil {
			remTaxTotal += *l.TaxAmount
		}
	}
	order.Update().SetSubtotal(remSubtotal).SetTaxTotal(remTaxTotal).SetTotalAmount(remSubtotal).Exec(ctx) //nolint

	jsonOK(w, map[string]any{
		"split_order_id":    splitOrder.ID,
		"split_order_number": splitOrder.OrderNumber,
		"lines_moved":       len(lines),
	})
}

// SetServiceCharge handles PATCH /{tenantID}/pos/orders/{orderID}/service-charge
// Sets or removes the service charge on a dine-in order.
// Requires pos.orders.manage permission (manager override).
func (h *TableHandler) SetServiceCharge(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	var input struct {
		Percent float64 `json:"percent"` // 0 to remove
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Percent < 0 || input.Percent > 100 {
		jsonError(w, "percent must be 0–100", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	order, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tid)).
		Only(ctx)
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	chargeAmount := order.TotalAmount * input.Percent / 100
	updated, err := order.Update().
		SetServiceChargePercent(input.Percent).
		SetServiceChargeAmount(chargeAmount).
		Save(ctx)
	if err != nil {
		h.log.Error("set service charge failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{
		"order_id":               orderID,
		"service_charge_percent": updated.ServiceChargePercent,
		"service_charge_amount":  updated.ServiceChargeAmount,
		"total_with_charge":      updated.TotalAmount + updated.ServiceChargeAmount,
	})
}

// DeleteTable handles DELETE /{tenantID}/pos/tables/{id}
// Hard-delete with cleanup. Rejects if the table is currently occupied. Any table that
// was ever seated has table_assignment rows; that FK is ON DELETE NO ACTION, so a bare
// delete fails. We therefore clear dependent assignment + reservation rows first and then
// delete the table, all within one transaction so a failure leaves nothing half-removed.
func (h *TableHandler) DeleteTable(w http.ResponseWriter, r *http.Request) {
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

	if tbl.Status == "occupied" {
		jsonError(w, "cannot delete an occupied table — release it first", http.StatusConflict)
		return
	}

	tx, err := h.client.Tx(r.Context())
	if err != nil {
		h.log.Error("delete table: begin tx failed", zap.Error(err))
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	// Clear historical assignments (the blocking FK) and any reservations for this table.
	if _, err := tx.TableAssignment.Delete().
		Where(tableassignment.TableID(tableID)).
		Exec(r.Context()); err != nil {
		_ = tx.Rollback()
		h.log.Error("delete table: clear assignments failed", zap.Error(err))
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	if _, err := tx.TableReservation.Delete().
		Where(entreservation.TableID(tableID)).
		Exec(r.Context()); err != nil {
		_ = tx.Rollback()
		h.log.Error("delete table: clear reservations failed", zap.Error(err))
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	if err := tx.Table.DeleteOneID(tableID).Exec(r.Context()); err != nil {
		_ = tx.Rollback()
		h.log.Error("delete table failed", zap.Error(err))
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("delete table: commit failed", zap.Error(err))
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// DeleteSection handles DELETE /{tenantID}/pos/sections/{id}
// Rejects if the section still contains tables.
func (h *TableHandler) DeleteSection(w http.ResponseWriter, r *http.Request) {
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

	sec, err := h.client.Section.Query().
		Where(entsection.ID(secID), entsection.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "section not found", http.StatusNotFound)
		return
	}

	count, err := h.client.Table.Query().
		Where(enttable.SectionID(secID), enttable.TenantID(tid)).
		Count(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if count > 0 {
		jsonError(w, "section still has tables — move or delete them first", http.StatusConflict)
		return
	}

	if err := h.client.Section.DeleteOne(sec).Exec(r.Context()); err != nil {
		h.log.Error("delete section failed", zap.Error(err))
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ─── Table Reservation handlers ──────────────────────────────────────────────

type reservationInput struct {
	TableID        *string `json:"table_id"`
	GuestName      string  `json:"guest_name"`
	GuestPhone     *string `json:"guest_phone"`
	GuestEmail     *string `json:"guest_email"`
	PartySize      int     `json:"party_size"`
	ScheduledAt    string  `json:"scheduled_at"` // RFC3339
	DurationMins   int     `json:"duration_minutes"`
	Notes          *string `json:"notes"`
	SpecialRequest *string `json:"special_requests"`
	Source         string  `json:"source"`
	OutletID       string  `json:"outlet_id"`
}

// CreateReservation handles POST /{tenantID}/pos/reservations
func (h *TableHandler) CreateReservation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var inp reservationInput
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(inp.GuestName) == "" {
		jsonError(w, "guest_name is required", http.StatusBadRequest)
		return
	}
	if inp.PartySize < 1 {
		inp.PartySize = 1
	}
	scheduledAt, err := time.Parse(time.RFC3339, inp.ScheduledAt)
	if err != nil {
		jsonError(w, "scheduled_at must be RFC3339", http.StatusBadRequest)
		return
	}
	if scheduledAt.Before(time.Now()) {
		jsonError(w, "scheduled_at must be in the future", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(inp.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	dur := inp.DurationMins
	if dur <= 0 {
		dur = 90
	}
	source := inp.Source
	if source == "" {
		source = "online_widget"
	}

	q := h.client.TableReservation.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetGuestName(inp.GuestName).
		SetPartySize(inp.PartySize).
		SetScheduledAt(scheduledAt).
		SetDurationMinutes(dur).
		SetSource(source)

	if inp.TableID != nil {
		if tbid, err2 := uuid.Parse(*inp.TableID); err2 == nil {
			q.SetTableID(tbid)
		}
	}
	if inp.GuestPhone != nil {
		q.SetGuestPhone(*inp.GuestPhone)
	}
	if inp.GuestEmail != nil {
		q.SetGuestEmail(*inp.GuestEmail)
	}
	if inp.Notes != nil {
		q.SetNotes(*inp.Notes)
	}
	if inp.SpecialRequest != nil {
		q.SetSpecialRequests(*inp.SpecialRequest)
	}

	res, err := q.Save(r.Context())
	if err != nil {
		h.log.Error("create reservation", zap.Error(err))
		jsonError(w, "failed to create reservation", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, res)
}

// ListReservations handles GET /{tenantID}/pos/reservations
func (h *TableHandler) ListReservations(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.TableReservation.Query().
		Where(entreservation.TenantID(tid)).
		Order(ent.Asc(entreservation.FieldScheduledAt))

	// Outlet: context takes precedence over query param
	reservationOutlet := httpware.GetOutletID(r.Context())
	if reservationOutlet == "" {
		reservationOutlet = r.URL.Query().Get("outlet_id")
	}
	if reservationOutlet != "" {
		if oid, parseErr := uuid.Parse(reservationOutlet); parseErr == nil {
			q = q.Where(entreservation.OutletID(oid))
		}
	}

	if dateStr := r.URL.Query().Get("date"); dateStr != "" {
		day, err := time.Parse("2006-01-02", dateStr)
		if err == nil {
			start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
			end := start.Add(24 * time.Hour)
			q = q.Where(
				entreservation.ScheduledAtGTE(start),
				entreservation.ScheduledAtLT(end),
			)
		}
	}
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entreservation.StatusEQ(entreservation.Status(status)))
	}
	if tableStr := r.URL.Query().Get("table_id"); tableStr != "" {
		if tbid, err := uuid.Parse(tableStr); err == nil {
			q = q.Where(entreservation.TableIDEQ(tbid))
		}
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	reservations, err := q.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(reservations, total, p))
}

// GetReservation handles GET /{tenantID}/pos/reservations/{id}
func (h *TableHandler) GetReservation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	resID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	res, err := h.client.TableReservation.Query().
		Where(entreservation.ID(resID), entreservation.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "reservation not found", http.StatusNotFound)
		return
	}
	jsonOK(w, res)
}

// UpdateReservation handles PATCH /{tenantID}/pos/reservations/{id}
func (h *TableHandler) UpdateReservation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	resID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	var inp struct {
		GuestName      *string `json:"guest_name"`
		GuestPhone     *string `json:"guest_phone"`
		GuestEmail     *string `json:"guest_email"`
		PartySize      *int    `json:"party_size"`
		ScheduledAt    *string `json:"scheduled_at"`
		DurationMins   *int    `json:"duration_minutes"`
		Notes          *string `json:"notes"`
		SpecialRequest *string `json:"special_requests"`
		TableID        *string `json:"table_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	upd := h.client.TableReservation.UpdateOneID(resID).
		Where(entreservation.TenantID(tid))

	if inp.GuestName != nil {
		upd.SetGuestName(*inp.GuestName)
	}
	if inp.GuestPhone != nil {
		upd.SetGuestPhone(*inp.GuestPhone)
	}
	if inp.GuestEmail != nil {
		upd.SetGuestEmail(*inp.GuestEmail)
	}
	if inp.PartySize != nil {
		upd.SetPartySize(*inp.PartySize)
	}
	if inp.ScheduledAt != nil {
		if t, err := time.Parse(time.RFC3339, *inp.ScheduledAt); err == nil {
			upd.SetScheduledAt(t)
		}
	}
	if inp.DurationMins != nil {
		upd.SetDurationMinutes(*inp.DurationMins)
	}
	if inp.Notes != nil {
		upd.SetNotes(*inp.Notes)
	}
	if inp.SpecialRequest != nil {
		upd.SetSpecialRequests(*inp.SpecialRequest)
	}
	if inp.TableID != nil {
		if tbid, err := uuid.Parse(*inp.TableID); err == nil {
			upd.SetTableID(tbid)
		}
	}

	res, err := upd.Save(r.Context())
	if err != nil {
		jsonError(w, "failed to update reservation", http.StatusInternalServerError)
		return
	}
	jsonOK(w, res)
}

// ConfirmReservation handles POST /{tenantID}/pos/reservations/{id}/confirm
func (h *TableHandler) ConfirmReservation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	resID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	var inp struct {
		TableID *string `json:"table_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&inp)

	upd := h.client.TableReservation.UpdateOneID(resID).
		Where(entreservation.TenantID(tid)).
		SetStatus("confirmed").
		SetConfirmedAt(time.Now())

	if inp.TableID != nil {
		if tbid, err := uuid.Parse(*inp.TableID); err == nil {
			upd.SetTableID(tbid)
			h.client.Table.UpdateOneID(tbid).
				Where(enttable.TenantID(tid)).
				SetStatus("reserved").
				SaveX(r.Context())
		}
	}

	res, err := upd.Save(r.Context())
	if err != nil {
		jsonError(w, "failed to confirm reservation", http.StatusInternalServerError)
		return
	}
	jsonOK(w, res)
}

// CheckInReservation handles POST /{tenantID}/pos/reservations/{id}/check-in
func (h *TableHandler) CheckInReservation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	resID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	res, err := h.client.TableReservation.Query().
		Where(entreservation.ID(resID), entreservation.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "reservation not found", http.StatusNotFound)
		return
	}

	upd := h.client.TableReservation.UpdateOneID(resID).
		SetStatus("checked_in").
		SetCheckedInAt(time.Now())

	if res.TableID != nil {
		h.client.Table.UpdateOneID(*res.TableID).
			Where(enttable.TenantID(tid)).
			SetStatus("occupied").
			SaveX(r.Context())
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		jsonError(w, "failed to check in", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// CancelReservation handles POST /{tenantID}/pos/reservations/{id}/cancel
func (h *TableHandler) CancelReservation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	resID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	var inp struct {
		Reason *string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&inp)

	res, err := h.client.TableReservation.Query().
		Where(entreservation.ID(resID), entreservation.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "reservation not found", http.StatusNotFound)
		return
	}

	if res.TableID != nil {
		h.client.Table.UpdateOneID(*res.TableID).
			Where(enttable.TenantID(tid)).
			SetStatus("available").
			SaveX(r.Context())
	}

	upd := h.client.TableReservation.UpdateOneID(resID).
		Where(entreservation.TenantID(tid)).
		SetStatus("cancelled").
		SetCancelledAt(time.Now())
	if inp.Reason != nil {
		upd.SetCancellationReason(*inp.Reason)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		jsonError(w, "failed to cancel reservation", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// GetAvailableSlots handles GET /{tenantID}/pos/reservations/available
// Query: date=YYYY-MM-DD, party_size=N, outlet_id=UUID
// Returns tables and their booked time slots for the requested date.
func (h *TableHandler) GetAvailableSlots(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}
	day, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		jsonError(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)

	partySize := 1
	if ps := r.URL.Query().Get("party_size"); ps != "" {
		fmt.Sscanf(ps, "%d", &partySize)
	}

	tq := h.client.Table.Query().
		Where(enttable.TenantID(tid), enttable.CapacityGTE(partySize))
	if outletStr := r.URL.Query().Get("outlet_id"); outletStr != "" {
		if oid, err := uuid.Parse(outletStr); err == nil {
			tq = tq.Where(enttable.OutletID(oid))
		}
	}
	tables, err := tq.All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	reservations, err := h.client.TableReservation.Query().
		Where(
			entreservation.TenantID(tid),
			entreservation.ScheduledAtGTE(dayStart),
			entreservation.ScheduledAtLT(dayEnd),
			entreservation.StatusNEQ("cancelled"),
		).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type slot struct {
		Start    time.Time `json:"start"`
		End      time.Time `json:"end"`
		Duration int       `json:"duration_minutes"`
	}
	bookedSlots := map[uuid.UUID][]slot{}
	for _, res := range reservations {
		if res.TableID == nil {
			continue
		}
		bookedSlots[*res.TableID] = append(bookedSlots[*res.TableID], slot{
			Start:    res.ScheduledAt,
			End:      res.ScheduledAt.Add(time.Duration(res.DurationMinutes) * time.Minute),
			Duration: res.DurationMinutes,
		})
	}

	type tableAvail struct {
		ID          uuid.UUID `json:"id"`
		Name        string    `json:"name"`
		Capacity    int       `json:"capacity"`
		TableType   string    `json:"table_type"`
		Status      string    `json:"status"`
		Tags        []string  `json:"tags"`
		BookedSlots []slot    `json:"booked_slots"`
	}
	result := make([]tableAvail, 0, len(tables))
	for _, t := range tables {
		tags := t.Tags
		if tags == nil {
			tags = []string{}
		}
		bs := bookedSlots[t.ID]
		if bs == nil {
			bs = []slot{}
		}
		result = append(result, tableAvail{
			ID:          t.ID,
			Name:        t.Name,
			Capacity:    t.Capacity,
			TableType:   string(t.TableType),
			Status:      t.Status,
			Tags:        tags,
			BookedSlots: bs,
		})
	}

	jsonOK(w, map[string]any{"date": dateStr, "tables": result})
}
