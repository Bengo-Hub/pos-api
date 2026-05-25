package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entsection "github.com/bengobox/pos-service/internal/ent/section"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tableassignment"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
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
