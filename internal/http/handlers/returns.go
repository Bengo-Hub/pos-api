package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// ReturnHandler handles POS return/exchange endpoints.
type ReturnHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewReturnHandler(log *zap.Logger, client *ent.Client) *ReturnHandler {
	return &ReturnHandler{log: log, client: client}
}

type returnLineInput struct {
	OrderLineID uuid.UUID `json:"order_line_id"`
	SKU         string    `json:"sku"`
	Name        string    `json:"name"`
	Quantity    float64   `json:"quantity"`
	UnitPrice   float64   `json:"unit_price"`
	TotalPrice  float64   `json:"total_price"`
	Reason      string    `json:"reason"`
}

type createReturnInput struct {
	OutletID    uuid.UUID         `json:"outlet_id"`
	ReturnType  string            `json:"return_type"` // refund | exchange | store_credit
	Reason      string            `json:"reason"`
	Lines       []returnLineInput `json:"lines"`
}

type approveReturnInput struct {
	Action string `json:"action"` // approve | reject
	Notes  string `json:"notes"`
}

// CreateReturn handles POST /{tenantID}/pos/orders/{orderID}/returns
func (h *ReturnHandler) CreateReturn(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderIDStr := chi.URLParam(r, "orderID")
	orderID, err := uuid.Parse(orderIDStr)
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	var input createReturnInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(input.Lines) == 0 {
		jsonError(w, "at least one return line is required", http.StatusBadRequest)
		return
	}

	returnType := input.ReturnType
	if returnType == "" {
		returnType = "refund"
	}

	// Get requesting user.
	requestedBy := uuid.Nil
	if userIDStr := r.Header.Get("X-User-ID"); userIDStr != "" {
		if uid, err := uuid.Parse(userIDStr); err == nil {
			requestedBy = uid
		}
	}

	// Generate return number.
	returnNumber := fmt.Sprintf("RET-%s-%d", tid.String()[:8], time.Now().UnixMilli())

	// Compute refund amount.
	var refundAmount float64
	for _, l := range input.Lines {
		refundAmount += l.TotalPrice
	}

	ctx := r.Context()
	ret, err := h.client.POSReturn.Create().
		SetTenantID(tid).
		SetOutletID(input.OutletID).
		SetOrderID(orderID).
		SetReturnNumber(returnNumber).
		SetReturnType(posreturn.ReturnType(returnType)).
		SetStatus(posreturn.StatusPending).
		SetReason(input.Reason).
		SetRefundAmount(refundAmount).
		SetRequestedBy(requestedBy).
		Save(ctx)
	if err != nil {
		h.log.Error("create return failed", zap.Error(err))
		jsonError(w, "failed to create return", http.StatusInternalServerError)
		return
	}

	// Create return lines.
	for _, l := range input.Lines {
		_, err := h.client.POSReturnLine.Create().
			SetReturnID(ret.ID).
			SetOrderLineID(l.OrderLineID).
			SetSku(l.SKU).
			SetName(l.Name).
			SetQuantity(l.Quantity).
			SetUnitPrice(l.UnitPrice).
			SetTotalPrice(l.TotalPrice).
			SetReason(l.Reason).
			Save(ctx)
		if err != nil {
			h.log.Error("create return line failed", zap.Error(err))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(ret)
}

// ListReturns handles GET /{tenantID}/pos/returns
func (h *ReturnHandler) ListReturns(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.POSReturn.Query().
		Where(posreturn.TenantID(tid)).
		WithLines().
		Order(ent.Desc(posreturn.FieldCreatedAt)).
		Limit(50)

	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(posreturn.StatusEQ(posreturn.Status(status)))
	}

	returns, err := q.All(r.Context())
	if err != nil {
		h.log.Error("list returns failed", zap.Error(err))
		jsonError(w, "failed to list returns", http.StatusInternalServerError)
		return
	}

	jsonOK(w, returns)
}

// ApproveReturn handles PATCH /{tenantID}/pos/returns/{returnID}/approve
func (h *ReturnHandler) ApproveReturn(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	returnIDStr := chi.URLParam(r, "returnID")
	returnID, err := uuid.Parse(returnIDStr)
	if err != nil {
		jsonError(w, "invalid return_id", http.StatusBadRequest)
		return
	}

	var input approveReturnInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	ret, err := h.client.POSReturn.Get(ctx, returnID)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "return not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get return", http.StatusInternalServerError)
		return
	}

	if ret.TenantID != tid {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	if ret.Status != posreturn.StatusPending {
		jsonError(w, "return is not pending", http.StatusConflict)
		return
	}

	// Get approver from claims.
	var approverID *uuid.UUID
	if userIDStr := r.Header.Get("X-User-ID"); userIDStr != "" {
		if uid, err := uuid.Parse(userIDStr); err == nil {
			approverID = &uid
		}
	}

	newStatus := posreturn.StatusApproved
	if input.Action == "reject" {
		newStatus = posreturn.StatusRejected
	}

	update := h.client.POSReturn.UpdateOne(ret).
		SetStatus(newStatus)
	if approverID != nil {
		update = update.SetApprovedBy(*approverID)
	}

	updated, err := update.Save(ctx)
	if err != nil {
		h.log.Error("approve return failed", zap.Error(err))
		jsonError(w, "failed to update return", http.StatusInternalServerError)
		return
	}

	jsonOK(w, updated)
}
