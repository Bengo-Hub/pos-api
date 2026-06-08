package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	repairsmod "github.com/bengobox/pos-service/internal/modules/repairs"
)

// RepairHandler exposes the repair / job-card endpoints.
type RepairHandler struct {
	log *zap.Logger
	svc *repairsmod.Service
}

// NewRepairHandler constructs a RepairHandler.
func NewRepairHandler(log *zap.Logger, db *ent.Client) *RepairHandler {
	return &RepairHandler{
		log: log.Named("repairs"),
		svc: repairsmod.NewService(db, log),
	}
}

// actorID returns the authenticated caller's user ID, if present.
func (h *RepairHandler) actorID(r *http.Request) *uuid.UUID {
	if uid, ok := callerUserID(r); ok {
		return &uid
	}
	return nil
}

func (h *RepairHandler) outletFromContext(r *http.Request) *uuid.UUID {
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, err := uuid.Parse(oidStr); err == nil {
			return &oid
		}
	}
	return nil
}

// List handles GET /{tenantID}/pos/repairs?status=
func (h *RepairHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	p := pagination.Parse(r)
	jobs, total, err := h.svc.List(r.Context(), repairsmod.ListFilter{
		TenantID: tid,
		OutletID: h.outletFromContext(r),
		Status:   r.URL.Query().Get("status"),
		Limit:    p.Limit,
		Offset:   p.Offset,
	})
	if err != nil {
		h.log.Error("list repair jobs failed", zap.Error(err))
		jsonError(w, "failed to list repair jobs", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(jobs, total, p))
}

type createRepairInput struct {
	OutletID          *string `json:"outlet_id"`
	CustomerPhone     string  `json:"customer_phone"`
	CustomerName      string  `json:"customer_name"`
	DeviceDescription string  `json:"device_description"`
	ReportedIssue     string  `json:"reported_issue"`
	EstimatedCost     string  `json:"estimated_cost"`
	AssignedStaffID   *string `json:"assigned_staff_id"`
}

// Create handles POST /{tenantID}/pos/repairs
func (h *RepairHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var in createRepairInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	estimate := decimal.Zero
	if in.EstimatedCost != "" {
		estimate, err = decimal.NewFromString(in.EstimatedCost)
		if err != nil {
			jsonError(w, "invalid estimated_cost", http.StatusBadRequest)
			return
		}
	}

	payload := repairsmod.CreateInput{
		CustomerPhone:     in.CustomerPhone,
		CustomerName:      in.CustomerName,
		DeviceDescription: in.DeviceDescription,
		ReportedIssue:     in.ReportedIssue,
		EstimatedCost:     estimate,
		ActorID:           h.actorID(r),
	}
	// Outlet: prefer explicit body value, fall back to outlet context.
	if in.OutletID != nil && *in.OutletID != "" {
		oid, perr := uuid.Parse(*in.OutletID)
		if perr != nil {
			jsonError(w, "invalid outlet_id", http.StatusBadRequest)
			return
		}
		payload.OutletID = &oid
	} else {
		payload.OutletID = h.outletFromContext(r)
	}
	if in.AssignedStaffID != nil && *in.AssignedStaffID != "" {
		sid, perr := uuid.Parse(*in.AssignedStaffID)
		if perr != nil {
			jsonError(w, "invalid assigned_staff_id", http.StatusBadRequest)
			return
		}
		payload.AssignedStaffID = &sid
	}

	job, err := h.svc.Create(r.Context(), tid, payload)
	if err != nil {
		h.log.Error("create repair job failed", zap.Error(err))
		jsonError(w, "failed to create repair job", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusCreated, job)
}

// Get handles GET /{tenantID}/pos/repairs/{id}
func (h *RepairHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid, jobID, ok := h.parseIDs(w, r)
	if !ok {
		return
	}

	job, err := h.svc.Get(r.Context(), tid, jobID)
	if err != nil {
		jsonError(w, "repair job not found", http.StatusNotFound)
		return
	}
	parts, _ := h.svc.Parts(r.Context(), tid, jobID)
	events, _ := h.svc.Events(r.Context(), tid, jobID)
	jsonOK(w, map[string]any{
		"job":    job,
		"parts":  parts,
		"events": events,
	})
}

type updateRepairInput struct {
	Status          *string `json:"status"`
	Diagnosis       *string `json:"diagnosis"`
	QuotedCost      *string `json:"quoted_cost"`
	AssignedStaffID *string `json:"assigned_staff_id"`
	Note            string  `json:"note"`
}

// Update handles PATCH /{tenantID}/pos/repairs/{id}
func (h *RepairHandler) Update(w http.ResponseWriter, r *http.Request) {
	tid, jobID, ok := h.parseIDs(w, r)
	if !ok {
		return
	}

	var in updateRepairInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	payload := repairsmod.UpdateInput{
		Status:    in.Status,
		Diagnosis: in.Diagnosis,
		Note:      in.Note,
		ActorID:   h.actorID(r),
	}
	if in.QuotedCost != nil && *in.QuotedCost != "" {
		qc, err := decimal.NewFromString(*in.QuotedCost)
		if err != nil {
			jsonError(w, "invalid quoted_cost", http.StatusBadRequest)
			return
		}
		payload.QuotedCost = &qc
	}
	if in.AssignedStaffID != nil && *in.AssignedStaffID != "" {
		sid, err := uuid.Parse(*in.AssignedStaffID)
		if err != nil {
			jsonError(w, "invalid assigned_staff_id", http.StatusBadRequest)
			return
		}
		payload.AssignedStaffID = &sid
	}

	job, err := h.svc.Update(r.Context(), tid, jobID, payload)
	if err != nil {
		h.writeServiceError(w, err, "failed to update repair job")
		return
	}
	jsonOK(w, job)
}

type addPartInput struct {
	InventorySKU    string  `json:"inventory_sku"`
	InventoryItemID *string `json:"inventory_item_id"`
	Description     string  `json:"description"`
	Quantity        float64 `json:"quantity"`
	UnitCost        string  `json:"unit_cost"`
}

// AddPart handles POST /{tenantID}/pos/repairs/{id}/parts
func (h *RepairHandler) AddPart(w http.ResponseWriter, r *http.Request) {
	tid, jobID, ok := h.parseIDs(w, r)
	if !ok {
		return
	}

	var in addPartInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	unitCost := decimal.Zero
	if in.UnitCost != "" {
		var err error
		unitCost, err = decimal.NewFromString(in.UnitCost)
		if err != nil {
			jsonError(w, "invalid unit_cost", http.StatusBadRequest)
			return
		}
	}

	payload := repairsmod.AddPartInput{
		InventorySKU: in.InventorySKU,
		Description:  in.Description,
		Quantity:     in.Quantity,
		UnitCost:     unitCost,
		ActorID:      h.actorID(r),
	}
	if in.InventoryItemID != nil && *in.InventoryItemID != "" {
		iid, err := uuid.Parse(*in.InventoryItemID)
		if err != nil {
			jsonError(w, "invalid inventory_item_id", http.StatusBadRequest)
			return
		}
		payload.InventoryItemID = &iid
	}

	part, err := h.svc.AddPart(r.Context(), tid, jobID, payload)
	if err != nil {
		h.writeServiceError(w, err, "failed to add part")
		return
	}
	total, _ := h.svc.PartsTotal(r.Context(), jobID)
	respondJSON(w, http.StatusCreated, map[string]any{
		"part":        part,
		"parts_total": total,
	})
}

// RemovePart handles DELETE /{tenantID}/pos/repairs/{id}/parts/{partID}
func (h *RepairHandler) RemovePart(w http.ResponseWriter, r *http.Request) {
	tid, jobID, ok := h.parseIDs(w, r)
	if !ok {
		return
	}
	partID, err := uuid.Parse(chi.URLParam(r, "partID"))
	if err != nil {
		jsonError(w, "invalid part_id", http.StatusBadRequest)
		return
	}

	if err := h.svc.RemovePart(r.Context(), tid, jobID, partID, h.actorID(r)); err != nil {
		h.writeServiceError(w, err, "failed to remove part")
		return
	}
	total, _ := h.svc.PartsTotal(r.Context(), jobID)
	jsonOK(w, map[string]any{"parts_total": total})
}

type settleInput struct {
	PosOrderID string `json:"pos_order_id"`
}

// Settle handles POST /{tenantID}/pos/repairs/{id}/settle
func (h *RepairHandler) Settle(w http.ResponseWriter, r *http.Request) {
	tid, jobID, ok := h.parseIDs(w, r)
	if !ok {
		return
	}

	var in settleInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	posOrderID, err := uuid.Parse(in.PosOrderID)
	if err != nil {
		jsonError(w, "invalid pos_order_id", http.StatusBadRequest)
		return
	}

	job, err := h.svc.Settle(r.Context(), tid, jobID, posOrderID, h.actorID(r))
	if err != nil {
		h.writeServiceError(w, err, "failed to settle repair job")
		return
	}
	jsonOK(w, job)
}

// parseIDs extracts the tenant + repair job IDs from the request.
func (h *RepairHandler) parseIDs(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, false
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid repair_job_id", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, false
	}
	return tid, jobID, true
}

// writeServiceError maps sentinel service errors to HTTP responses.
func (h *RepairHandler) writeServiceError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, repairsmod.ErrNotFound):
		jsonError(w, "repair job not found", http.StatusNotFound)
	case errors.Is(err, repairsmod.ErrPartNotFound):
		jsonError(w, "repair job part not found", http.StatusNotFound)
	case errors.Is(err, repairsmod.ErrInvalidStatus):
		jsonError(w, "invalid status", http.StatusBadRequest)
	case errors.Is(err, repairsmod.ErrAlreadySettled):
		jsonError(w, "repair job already settled", http.StatusConflict)
	case errors.Is(err, repairsmod.ErrTerminalStatus):
		jsonError(w, "repair job is in a terminal status", http.StatusConflict)
	default:
		h.log.Error(fallback, zap.Error(err))
		jsonError(w, fallback, http.StatusInternalServerError)
	}
}
