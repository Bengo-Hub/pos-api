package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
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
	auditSvc       *audit.Service
}

func NewReturnHandler(log *zap.Logger, client *ent.Client, treasuryClient *treasury.Client, publisher *events.Publisher) *ReturnHandler {
	return &ReturnHandler{log: log, client: client, treasuryClient: treasuryClient, publisher: publisher}
}

// SetAuditService wires the centralized audit trail for refunds/returns.
func (h *ReturnHandler) SetAuditService(a *audit.Service) { h.auditSvc = a }

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
	OutletID      string            `json:"outlet_id"`
	ReturnType    string            `json:"return_type"` // refund | exchange | store_credit
	Reason        string            `json:"reason"`
	ReasonCode    string            `json:"reason_code,omitempty"`    // changed_mind | defective | damaged | wrong_item | expired | other
	RefundChannel string            `json:"refund_channel,omitempty"` // cash | mpesa | bank | cheque | store_credit | offset_invoice
	Lines         []returnLineInput `json:"lines"`
}

type approveReturnInput struct {
	Action string `json:"action"` // approve | reject
	Notes  string `json:"notes"`
	// RefundChannel lets the approver pick/override the settlement method at approval time
	// (cash | mpesa | bank | cheque | store_credit | offset_invoice). When empty, the channel
	// chosen at return-create time is used.
	RefundChannel string `json:"refund_channel,omitempty"`
}

// completeReturnInput is the body for the complete-approved-return step. Both fields are optional:
// notes are recorded for audit; refund_channel overrides the settlement method one last time.
type completeReturnInput struct {
	Notes         string `json:"notes,omitempty"`
	RefundChannel string `json:"refund_channel,omitempty"`
}

// returnResponse decorates a POSReturn with the original order's human-readable number so the UI
// never has to render the raw order UUID ("Original Order"). Embedding the *ent.POSReturn promotes
// all of its JSON fields (incl. the `edges.lines`), and adds `order_number` alongside them.
type returnResponse struct {
	*ent.POSReturn
	OrderNumber string `json:"order_number,omitempty"`
}

// orderNumberFor resolves the display order number for a single order id (best-effort; "" on miss).
func (h *ReturnHandler) orderNumberFor(ctx context.Context, tid, orderID uuid.UUID) string {
	if orderID == uuid.Nil {
		return ""
	}
	o, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tid)).
		Select(entposorder.FieldOrderNumber).
		Only(ctx)
	if err != nil || o == nil {
		return ""
	}
	return o.OrderNumber
}

// withOrderNumber wraps one return in a returnResponse carrying its original order number.
func (h *ReturnHandler) withOrderNumber(ctx context.Context, tid uuid.UUID, ret *ent.POSReturn) returnResponse {
	return returnResponse{POSReturn: ret, OrderNumber: h.orderNumberFor(ctx, tid, ret.OrderID)}
}

// withOrderNumbers wraps a slice of returns, batch-loading the order numbers in one query.
func (h *ReturnHandler) withOrderNumbers(ctx context.Context, tid uuid.UUID, returns []*ent.POSReturn) []returnResponse {
	ids := make([]uuid.UUID, 0, len(returns))
	for _, ret := range returns {
		if ret.OrderID != uuid.Nil {
			ids = append(ids, ret.OrderID)
		}
	}
	numByID := make(map[uuid.UUID]string, len(ids))
	if len(ids) > 0 {
		orders, err := h.client.POSOrder.Query().
			Where(entposorder.TenantID(tid), entposorder.IDIn(ids...)).
			Select(entposorder.FieldID, entposorder.FieldOrderNumber).
			All(ctx)
		if err == nil {
			for _, o := range orders {
				numByID[o.ID] = o.OrderNumber
			}
		}
	}
	out := make([]returnResponse, 0, len(returns))
	for _, ret := range returns {
		out = append(out, returnResponse{POSReturn: ret, OrderNumber: numByID[ret.OrderID]})
	}
	return out
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

	ctx := r.Context()

	// Enforce return window: load the original order and check its age against outlet settings.
	order, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tid)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to load order", http.StatusInternalServerError)
		return
	}
	if outletSetting, settingErr := h.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(order.OutletID)).
		Only(ctx); settingErr == nil {
		windowDays := outletSetting.ReturnWindowDays
		if windowDays > 0 && time.Since(order.CreatedAt) > time.Duration(windowDays)*24*time.Hour {
			jsonError(w, "return window has expired", http.StatusUnprocessableEntity)
			return
		}
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

	// Generate return number. Human-readable, order-number style (RET-<epoch-ms>) — never
	// embed the tenant UUID prefix, which read as a raw id on the returns UI.
	returnNumber := fmt.Sprintf("RET-%d", time.Now().UnixMilli())

	// Compute refund amount.
	var refundAmount float64
	for _, l := range input.Lines {
		refundAmount += l.TotalPrice
	}

	returnOutletID := parseOptionalUUID(input.OutletID, r)

	ret, err := h.client.POSReturn.Create().
		SetTenantID(tid).
		SetOutletID(returnOutletID).
		SetOrderID(orderID).
		SetReturnNumber(returnNumber).
		SetReturnType(posreturn.ReturnType(returnType)).
		SetStatus(posreturn.StatusPending).
		SetReason(input.Reason).
		SetNillableReasonCode(reasonCodePtr(input.ReasonCode)).
		SetNillableRefundChannel(refundChannelPtr(input.RefundChannel)).
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

	// Record the refund in the central audit trail (a key return-fraud signal —
	// surfaces in the per-cashier exception report).
	if h.auditSvc != nil {
		amt := refundAmount
		oid := returnOutletID
		h.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: requestedBy,
			Action:      "return.refund",
			EntityType:  "pos_return",
			EntityID:    ret.ID.String(),
			Reason:      input.Reason,
			Amount:      &amt,
			After:       map[string]any{"return_type": returnType, "order_id": orderID.String(), "return_number": returnNumber},
		})
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
	_ = json.NewEncoder(w).Encode(returnResponse{POSReturn: ret, OrderNumber: h.orderNumberFor(ctx, tid, orderID)})
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

	jsonOK(w, pagination.NewResponse(h.withOrderNumbers(r.Context(), tid, returns), total, p))
}

// GetReturn handles GET /{tenantID}/pos/returns/{returnID}
func (h *ReturnHandler) GetReturn(w http.ResponseWriter, r *http.Request) {
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

	ret, err := h.client.POSReturn.Query().
		Where(posreturn.ID(returnID), posreturn.TenantID(tid)).
		WithLines().
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "return not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get return", http.StatusInternalServerError)
		return
	}

	jsonOK(w, h.withOrderNumber(r.Context(), tid, ret))
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

	// Approval is a DECISION step only (manager authorises or rejects). The money movement
	// (treasury refund, eTIMS credit note, inventory restock) happens later at CompleteReturn,
	// when the goods are physically taken back and the refund is handed over. This gives a clean
	// three-stage lifecycle — request (cashier) → approve (manager) → complete (till) — so the
	// Completed tab is populated by real fulfilment, not by the approval itself.
	newStatus := posreturn.StatusApproved
	if input.Action == "reject" {
		newStatus = posreturn.StatusRejected
	}

	update := h.client.POSReturn.UpdateOne(ret).SetStatus(newStatus)
	if approverID != nil {
		update = update.SetApprovedBy(*approverID)
	}

	// Persist the approver's settlement-channel choice (an override at approval time wins over the
	// channel chosen at create time) so the completion step and treasury both use the right method.
	if rc := refundChannelPtr(input.RefundChannel); rc != nil {
		update = update.SetNillableRefundChannel(rc)
	}

	// Record the approver's decision notes on the return metadata for the audit trail.
	if strings.TrimSpace(input.Notes) != "" {
		md := cloneReturnMetadata(ret.Metadata)
		if newStatus == posreturn.StatusRejected {
			md["rejection_notes"] = input.Notes
		} else {
			md["approval_notes"] = input.Notes
		}
		update = update.SetMetadata(md)
	}

	updated, err := update.Save(ctx)
	if err != nil {
		h.log.Error("approve return failed", zap.Error(err))
		jsonError(w, "failed to update return", http.StatusInternalServerError)
		return
	}

	// Audit the approve/reject decision (per-cashier exception report + return-fraud signals).
	if h.auditSvc != nil {
		action := "return.approved"
		if newStatus == posreturn.StatusRejected {
			action = "return.rejected"
		}
		oid := ret.OutletID
		actor := uuid.Nil
		if approverID != nil {
			actor = *approverID
		}
		h.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: actor,
			Action:      action,
			EntityType:  "pos_return",
			EntityID:    returnID.String(),
			Reason:      input.Notes,
			After:       map[string]any{"status": string(newStatus), "return_number": ret.ReturnNumber},
		})
	}

	jsonOK(w, h.withOrderNumber(ctx, tid, updated))
}

// CompleteReturn handles POST /{tenantID}/pos/returns/{returnID}/complete — the final fulfilment
// step. Only an APPROVED return can be completed; it settles the money (treasury refund + eTIMS
// credit note) and publishes return.completed/exchange.completed (inventory restock + treasury
// settlement), then marks the return completed so it lands in the Completed tab.
func (h *ReturnHandler) CompleteReturn(w http.ResponseWriter, r *http.Request) {
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

	var input completeReturnInput
	// Body is optional (notes / channel override); ignore a decode error on an empty body.
	_ = json.NewDecoder(r.Body).Decode(&input)

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
	if ret.Status != posreturn.StatusApproved {
		jsonError(w, "only an approved return can be completed", http.StatusConflict)
		return
	}

	// Who is completing the return (the till/cashier handing over the refund).
	var completedBy *uuid.UUID
	if userIDStr := r.Header.Get("X-User-ID"); userIDStr != "" {
		if uid, uidErr := uuid.Parse(userIDStr); uidErr == nil {
			completedBy = &uid
		}
	}

	// Load return lines for the refund tax/cost + restock event payload.
	lines, _ := h.client.POSReturnLine.Query().
		Where(posreturnline.ReturnID(returnID)).
		All(ctx)

	update := h.client.POSReturn.UpdateOne(ret).SetStatus(posreturn.StatusCompleted)

	// Resolve the settlement channel: an explicit override at completion wins, otherwise the channel
	// persisted on the return (from create/approve) is used.
	refundChannel := ""
	if ret.RefundChannel != nil {
		refundChannel = string(*ret.RefundChannel)
	}
	if rc := refundChannelPtr(input.RefundChannel); rc != nil {
		refundChannel = string(*rc)
		update = update.SetNillableRefundChannel(rc)
	}

	// Money movement: settle in treasury for BOTH a cash/mpesa/bank refund AND a store-credit return.
	// A store-credit return has no channel of its own, so force the store_credit channel — treasury
	// then reverses revenue+VAT AND credits the customer's AR account, so a KES 800 store-credit return
	// against a KES 2000 credit sale nets to KES 1200 owed (previously store-credit returns skipped
	// this call entirely, posting only a GL journal and never reducing the customer balance).
	var treasuryRefundRef string
	if (ret.ReturnType == posreturn.ReturnTypeRefund || ret.ReturnType == posreturn.ReturnTypeStoreCredit) && h.treasuryClient != nil {
		tenantSlug := chi.URLParam(r, "tenantID")
		if ret.ReturnType == posreturn.ReturnTypeStoreCredit {
			refundChannel = "store_credit"
		}

		// Sum the returned lines' VAT and COGS so treasury can reverse the exact tax + cost-of-goods.
		// tax_amount is prorated from the original order line's tax by returned quantity; cost is the
		// inventory-synced cost_price (the same POSCatalogOverride.metadata["cost_price"] source used
		// for the sale.finalized COGS posting) × returned quantity. Missing data => 0 (never blocks).
		taxAmount := h.resolveReturnTax(ctx, ret.OrderID, lines)
		costAmount := h.resolveReturnCost(ctx, tid, lines)

		// Original buyer's CRM contact + name (for the treasury refund's customer linkage). The order
		// stores the name; the CRM contact lives on the matched loyalty account (by customer_phone).
		crmContactID, customerName := h.resolveReturnCustomer(ctx, tid, ret.OrderID)
		// Also forward the buyer's phone as the identifier fallback so a store-credit return still nets
		// against a legacy phone-keyed credit-sale row when no CRM contact was linked.
		customerIdentifier := h.resolveReturnCustomerPhone(ctx, tid, ret.OrderID)

		refundResp, refundErr := h.treasuryClient.CreateRefund(ctx, tenantSlug, returnID.String(), treasury.RefundRequest{
			SourceService:      "pos",
			ReferenceID:        returnID.String(),
			ReferenceType:      "pos_return",
			Amount:             ret.RefundAmount,
			TaxAmount:          taxAmount,
			Cost:               costAmount,
			Currency:           "KES",
			Reason:             ret.Reason,
			RefundChannel:      refundChannel,
			CrmContactID:       crmContactID,
			CustomerIdentifier: customerIdentifier,
			CustomerName:       customerName,
		})
		if refundErr != nil {
			h.log.Error("treasury refund call failed (non-fatal; refund can be retried)",
				zap.Error(refundErr),
				zap.String("return_id", returnID.String()),
				zap.String("refund_channel", refundChannel),
				zap.Float64("amount", ret.RefundAmount),
				zap.Float64("tax_amount", taxAmount),
				zap.Float64("cost", costAmount))
			// Non-fatal: still complete the return record; refund can be retried
		} else {
			treasuryRefundRef = refundResp.ID
			update = update.SetTreasuryRefundRef(treasuryRefundRef)
		}
	}

	// eTIMS credit note: a returned, tax-invoiced sale needs a VAT-reversal credit note in treasury.
	// Best-effort + non-fatal: find the original sale's invoice by reference, then issue the credit
	// note. Treasury owns it; pos only logs the number for audit.
	if h.treasuryClient != nil &&
		(ret.ReturnType == posreturn.ReturnTypeRefund || ret.ReturnType == posreturn.ReturnTypeStoreCredit) {
		slug := chi.URLParam(r, "tenantID")
		if inv, invErr := h.treasuryClient.GetInvoiceByReference(ctx, slug, "pos_order", ret.OrderID.String()); invErr == nil && inv != nil && inv.ID != "" {
			if cn, cnErr := h.treasuryClient.CreateCreditNote(ctx, slug, inv.ID); cnErr != nil {
				h.log.Warn("eTIMS credit-note creation failed (non-fatal)", zap.Error(cnErr), zap.String("return_id", returnID.String()))
			} else {
				h.log.Info("eTIMS credit-note issued for return", zap.String("return_id", returnID.String()), zap.String("credit_note", cn.Number))
			}
		}
	}

	// Record completer + notes on metadata for the audit trail (who physically fulfilled the return).
	md := cloneReturnMetadata(ret.Metadata)
	md["completed_at"] = time.Now().UTC().Format(time.RFC3339)
	if completedBy != nil {
		md["completed_by"] = completedBy.String()
	}
	if strings.TrimSpace(input.Notes) != "" {
		md["completion_notes"] = input.Notes
	}
	update = update.SetMetadata(md)

	updated, err := update.Save(ctx)
	if err != nil {
		h.log.Error("complete return failed", zap.Error(err))
		jsonError(w, "failed to complete return", http.StatusInternalServerError)
		return
	}

	// Audit the completion (money-out event — a key return-fraud signal).
	if h.auditSvc != nil {
		amt := ret.RefundAmount
		oid := ret.OutletID
		actor := uuid.Nil
		if completedBy != nil {
			actor = *completedBy
		}
		h.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: actor,
			Action:      "return.completed",
			EntityType:  "pos_return",
			EntityID:    returnID.String(),
			Reason:      ret.Reason,
			Amount:      &amt,
			After:       map[string]any{"return_number": ret.ReturnNumber, "refund_channel": refundChannel, "treasury_refund_ref": treasuryRefundRef},
		})
	}

	// Publish the completion event for inventory restock + treasury settlement.
	if h.publisher != nil {
		linesSummary := make([]map[string]any, 0, len(lines))
		for _, l := range lines {
			linesSummary = append(linesSummary, map[string]any{
				"sku": l.Sku, "name": l.Name, "quantity": l.Quantity, "unit_price": l.UnitPrice,
			})
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
		if ret.ReturnType == posreturn.ReturnTypeExchange {
			_ = h.publisher.PublishExchangeCompleted(ctx, tid, eventData)
		} else {
			_ = h.publisher.PublishReturnCompleted(ctx, tid, eventData)
		}
	}

	jsonOK(w, h.withOrderNumber(ctx, tid, updated))
}

// cloneReturnMetadata returns a shallow copy of a return's metadata map so callers can add keys
// without mutating the loaded entity's map in place.
func cloneReturnMetadata(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src)+2)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// refundChannelPtr converts a refund_channel string to a *posreturn.RefundChannel for the Nillable
// setter. Returns nil for empty/invalid input so an unset channel stays NULL.
func refundChannelPtr(s string) *posreturn.RefundChannel {
	switch posreturn.RefundChannel(s) {
	case posreturn.RefundChannelCash, posreturn.RefundChannelMpesa,
		posreturn.RefundChannelBank, posreturn.RefundChannelCheque,
		posreturn.RefundChannelStoreCredit, posreturn.RefundChannelOffsetInvoice:
		rc := posreturn.RefundChannel(s)
		return &rc
	}
	return nil
}

// resolveReturnTax sums the VAT to reverse for the returned lines. The return line stores no tax, so
// we prorate the original POSOrderLine's tax_amount by the returned quantity (tax × returnedQty/lineQty).
// Lines without a matching priced order line or with no tax contribute 0. Errors => 0 (never blocks).
func (h *ReturnHandler) resolveReturnTax(ctx context.Context, orderID uuid.UUID, lines []*ent.POSReturnLine) float64 {
	if len(lines) == 0 {
		return 0
	}
	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		if l.OrderLineID != uuid.Nil {
			ids = append(ids, l.OrderLineID)
		}
	}
	if len(ids) == 0 {
		return 0
	}
	orderLines, err := h.client.POSOrderLine.Query().
		Where(entposorderline.OrderID(orderID), entposorderline.IDIn(ids...)).
		All(ctx)
	if err != nil {
		h.log.Warn("return refund: failed to resolve line tax (defaulting to 0)", zap.Error(err))
		return 0
	}
	byID := make(map[uuid.UUID]*ent.POSOrderLine, len(orderLines))
	for _, ol := range orderLines {
		byID[ol.ID] = ol
	}
	var total float64
	for _, l := range lines {
		ol, ok := byID[l.OrderLineID]
		if !ok || ol.TaxAmount == nil || *ol.TaxAmount == 0 || ol.Quantity <= 0 {
			continue
		}
		ratio := l.Quantity / ol.Quantity
		if ratio > 1 {
			ratio = 1
		}
		total += *ol.TaxAmount * ratio
	}
	return total
}

// resolveReturnCost sums the COGS of the returned goods so treasury can reverse Cost-of-Goods-Sold and
// trigger the restock reversal. It uses the same authoritative cost source as the sale.finalized COGS
// posting: POSCatalogOverride.metadata["cost_price"] keyed by (tenant, inventory_sku). Missing cost => 0.
func (h *ReturnHandler) resolveReturnCost(ctx context.Context, tenantID uuid.UUID, lines []*ent.POSReturnLine) float64 {
	if len(lines) == 0 {
		return 0
	}
	skus := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		if l.Sku == "" {
			continue
		}
		if _, ok := seen[l.Sku]; ok {
			continue
		}
		seen[l.Sku] = struct{}{}
		skus = append(skus, l.Sku)
	}
	if len(skus) == 0 {
		return 0
	}
	overrides, err := h.client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tenantID), entoverride.InventorySkuIn(skus...)).
		All(ctx)
	if err != nil {
		h.log.Warn("return refund: failed to resolve item costs (defaulting to 0)", zap.Error(err))
		return 0
	}
	costBySKU := make(map[string]float64, len(overrides))
	for _, ov := range overrides {
		if ov.Metadata == nil {
			continue
		}
		switch v := ov.Metadata["cost_price"].(type) {
		case float64:
			costBySKU[ov.InventorySku] = v
		case int:
			costBySKU[ov.InventorySku] = float64(v)
		}
	}
	var total float64
	for _, l := range lines {
		total += costBySKU[l.Sku] * l.Quantity
	}
	return total
}

// resolveReturnCustomer returns the original buyer's CRM contact id (from the matched loyalty account)
// and name (from the order) for the treasury refund. Both are best-effort; empty when unavailable.
func (h *ReturnHandler) resolveReturnCustomer(ctx context.Context, tenantID, orderID uuid.UUID) (crmContactID, customerName string) {
	order, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return "", ""
	}
	if order.CustomerName != nil {
		customerName = *order.CustomerName
	}
	if order.CustomerPhone != nil && *order.CustomerPhone != "" {
		if acc, accErr := h.client.LoyaltyAccount.Query().
			Where(entla.TenantID(tenantID), entla.CustomerPhone(*order.CustomerPhone)).
			First(ctx); accErr == nil && acc != nil && acc.CrmContactID != nil {
			crmContactID = acc.CrmContactID.String()
		}
	}
	return crmContactID, customerName
}

// resolveReturnCustomerPhone returns the original buyer's phone (the treasury AR identifier fallback),
// or "" when the order carried no customer phone. Best-effort.
func (h *ReturnHandler) resolveReturnCustomerPhone(ctx context.Context, tenantID, orderID uuid.UUID) string {
	order, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil || order.CustomerPhone == nil {
		return ""
	}
	return *order.CustomerPhone
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
