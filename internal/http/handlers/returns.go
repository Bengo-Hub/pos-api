package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/posreturnline"
	"github.com/bengobox/pos-service/internal/modules/treasury"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// ReturnHandler handles POS return/exchange endpoints.
type ReturnHandler struct {
	log            *zap.Logger
	client         *ent.Client
	treasuryClient *treasury.Client
	publisher      *events.Publisher
}

func NewReturnHandler(log *zap.Logger, client *ent.Client, treasuryClient *treasury.Client, publisher *events.Publisher) *ReturnHandler {
	return &ReturnHandler{log: log, client: client, treasuryClient: treasuryClient, publisher: publisher}
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
	OutletID   string            `json:"outlet_id"`
	ReturnType string            `json:"return_type"` // refund | exchange | store_credit
	Reason     string            `json:"reason"`
	ReasonCode string            `json:"reason_code,omitempty"` // changed_mind | defective | damaged | wrong_item | expired | other
	Lines      []returnLineInput `json:"lines"`
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

	// Block returns for non-returnable items (e.g. dispensed medications).
	// Collect all SKUs from return lines, load catalog items, check is_returnable.
	skus := make([]string, 0, len(input.Lines))
	for _, l := range input.Lines {
		if l.SKU != "" {
			skus = append(skus, l.SKU)
		}
	}
	if len(skus) > 0 {
		nonReturnableOverrides, _ := h.client.POSCatalogOverride.Query().
			Where(
				entoverride.TenantID(tid),
				entoverride.InventorySkuIn(skus...),
				entoverride.IsReturnableEQ(false),
			).
			All(r.Context())
		if len(nonReturnableOverrides) > 0 {
			names := make([]string, 0, len(nonReturnableOverrides))
			for _, it := range nonReturnableOverrides {
				names = append(names, it.InventorySku)
			}
			jsonError(w, "return not allowed for: "+strings.Join(names, ", "), http.StatusUnprocessableEntity)
			return
		}
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

	returnOutletID := parseOptionalUUID(input.OutletID, r)

	ctx := r.Context()
	ret, err := h.client.POSReturn.Create().
		SetTenantID(tid).
		SetOutletID(returnOutletID).
		SetOrderID(orderID).
		SetReturnNumber(returnNumber).
		SetReturnType(posreturn.ReturnType(returnType)).
		SetStatus(posreturn.StatusPending).
		SetReason(input.Reason).
		SetNillableReasonCode(reasonCodePtr(input.ReasonCode)).
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

	// Publish return.initiated event (audit trail, non-blocking).
	if h.publisher != nil {
		linesSummary := make([]map[string]any, 0, len(input.Lines))
		for _, l := range input.Lines {
			linesSummary = append(linesSummary, map[string]any{
				"sku": l.SKU, "name": l.Name, "quantity": l.Quantity, "total_price": l.TotalPrice,
			})
		}
		_ = h.publisher.PublishReturnInitiated(ctx, tid, map[string]any{
			"return_id":     ret.ID,
			"order_id":      orderID,
			"outlet_id":     input.OutletID,
			"return_type":   returnType,
			"refund_amount": refundAmount,
			"lines":         linesSummary,
		})
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

	p := pagination.Parse(r)
	baseQ := h.client.POSReturn.Query().Where(posreturn.TenantID(tid))

	urlq := r.URL.Query()
	if status := urlq.Get("status"); status != "" {
		baseQ = baseQ.Where(posreturn.StatusEQ(posreturn.Status(status)))
	}
	if staffIDStr := urlq.Get("staff_id"); staffIDStr != "" {
		if staffUID, err := uuid.Parse(staffIDStr); err == nil {
			baseQ = baseQ.Where(posreturn.RequestedBy(staffUID))
		}
	}

	total, _ := baseQ.Clone().Count(r.Context())
	returns, err := baseQ.WithLines().Order(ent.Desc(posreturn.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list returns failed", zap.Error(err))
		jsonError(w, "failed to list returns", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(returns, total, p))
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

	// Load return lines for event payload + refund.
	lines, _ := h.client.POSReturnLine.Query().
		Where(posreturnline.ReturnID(returnID)).
		All(ctx)

	newStatus := posreturn.StatusApproved
	if input.Action == "reject" {
		newStatus = posreturn.StatusRejected
	}

	update := h.client.POSReturn.UpdateOne(ret).SetStatus(newStatus)
	if approverID != nil {
		update = update.SetApprovedBy(*approverID)
	}

	// If approving a refund-type return, call treasury-api and store reference.
	var treasuryRefundRef string
	if newStatus == posreturn.StatusApproved && ret.ReturnType == posreturn.ReturnTypeRefund && h.treasuryClient != nil {
		tenantSlug := chi.URLParam(r, "tenantID")
		refundResp, refundErr := h.treasuryClient.CreateRefund(ctx, tenantSlug, treasury.RefundRequest{
			SourceService: "pos",
			ReferenceID:   returnID.String(),
			ReferenceType: "pos_return",
			Amount:        ret.RefundAmount,
			Currency:      "KES",
			Reason:        ret.Reason,
		})
		if refundErr != nil {
			h.log.Error("treasury refund call failed", zap.Error(refundErr), zap.String("return_id", returnID.String()))
			// Non-fatal: still approve the return record; refund can be retried
		} else {
			treasuryRefundRef = refundResp.ID
			update = update.SetTreasuryRefundRef(treasuryRefundRef)
		}
	}

	updated, err := update.Save(ctx)
	if err != nil {
		h.log.Error("approve return failed", zap.Error(err))
		jsonError(w, "failed to update return", http.StatusInternalServerError)
		return
	}

	// Publish event for inventory restock + treasury settlement.
	if h.publisher != nil && newStatus == posreturn.StatusApproved {
		linesSummary := make([]map[string]any, 0, len(lines))
		for _, l := range lines {
			linesSummary = append(linesSummary, map[string]any{
				"sku": l.Sku, "name": l.Name, "quantity": l.Quantity, "unit_price": l.UnitPrice,
			})
		}
		eventType := "return.completed"
		if ret.ReturnType == posreturn.ReturnTypeExchange {
			eventType = "exchange.completed"
		}
		eventData := map[string]any{
			"return_id":           returnID,
			"order_id":            ret.OrderID,
			"outlet_id":           ret.OutletID,
			"return_type":         string(ret.ReturnType),
			"refund_amount":       ret.RefundAmount,
			"treasury_refund_ref": treasuryRefundRef,
			"lines":               linesSummary,
		}
		if eventType == "exchange.completed" {
			_ = h.publisher.PublishExchangeCompleted(ctx, tid, eventData)
		} else {
			_ = h.publisher.PublishReturnCompleted(ctx, tid, eventData)
		}
	}

	jsonOK(w, updated)
}

// reasonCodePtr converts a reason_code string to a *posreturn.ReasonCode for SetNillableReasonCode.
// Returns nil if the string is empty or not a valid enum value.
func reasonCodePtr(s string) *posreturn.ReasonCode {
	switch posreturn.ReasonCode(s) {
	case posreturn.ReasonCodeChangedMind, posreturn.ReasonCodeDefective,
		posreturn.ReasonCodeDamaged, posreturn.ReasonCodeWrongItem,
		posreturn.ReasonCodeExpired, posreturn.ReasonCodeOther:
		rc := posreturn.ReasonCode(s)
		return &rc
	}
	return nil
}
