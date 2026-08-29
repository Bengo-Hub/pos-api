// Package returns implements the POS sell-return/exchange lifecycle (initiate → approve →
// complete) as a callable service — extracted out of internal/http/handlers so other
// business modules (saleedit's fiscalized-reduction path) can reuse the exact same
// settlement logic (treasury refund, eTIMS credit note, inventory restock event) the
// customer-facing three-stage flow uses, instead of re-implementing it a second time.
// handlers.ReturnHandler is now a thin HTTP adapter over this package.
package returns

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/posreturnline"
	"github.com/bengobox/pos-service/internal/modules/documents"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/salecorrection"
	"github.com/bengobox/pos-service/internal/modules/treasury"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// Service orchestrates the return/exchange lifecycle.
type Service struct {
	log            *zap.Logger
	client         *ent.Client
	treasuryClient *treasury.Client
	publisher      *events.Publisher
	auditSvc       *audit.Service
	// orderSvc creates exchange replacement orders through the normal sale pipeline.
	orderSvc *orders.Service
	// seq, when wired, mints return numbers through the tenant-configurable document sequence
	// (numeric by default), falling back to the legacy RET-<epoch-ms> format.
	seq *documents.SequenceService
}

// NewService wires the returns orchestrator.
func NewService(log *zap.Logger, client *ent.Client, treasuryClient *treasury.Client, publisher *events.Publisher) *Service {
	return &Service{log: log.Named("returns"), client: client, treasuryClient: treasuryClient, publisher: publisher}
}

// SetAuditService wires the centralized audit trail for refunds/returns.
func (s *Service) SetAuditService(a *audit.Service) { s.auditSvc = a }

// WithSequence wires the document-sequence service so return numbers are minted through the
// tenant's pos_return sequence (numeric by default), falling back to the legacy format.
func (s *Service) WithSequence(seq *documents.SequenceService) *Service {
	s.seq = seq
	return s
}

// SetOrderService wires the orders service used to create exchange replacement orders.
func (s *Service) SetOrderService(svc *orders.Service) { s.orderSvc = svc }

// Client exposes the ent client for read-only list/detail queries in the HTTP layer.
func (s *Service) Client() *ent.Client { return s.client }

// CreateReturn initiates a return/exchange against a finalized order. Unless
// req.BypassGuards is set (the admin Edit-Sale caller only), it enforces the outlet's
// return-window age limit and the per-item is_returnable flag — the same guards the
// customer-facing lifecycle has always enforced.
func (s *Service) CreateReturn(ctx context.Context, tenantID uuid.UUID, req CreateReturnRequest) (*ent.POSReturn, error) {
	if len(req.Lines) == 0 {
		return nil, fmt.Errorf("at least one return line is required")
	}

	order, err := s.client.POSOrder.Query().
		Where(entposorder.ID(req.OrderID), entposorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("order not found")
		}
		return nil, fmt.Errorf("load order: %w", err)
	}

	if !req.BypassGuards {
		if outletSetting, settingErr := s.client.OutletSetting.Query().
			Where(entoutletsetting.OutletID(order.OutletID)).
			Only(ctx); settingErr == nil {
			windowDays := outletSetting.ReturnWindowDays
			if windowDays > 0 && time.Since(order.CreatedAt) > time.Duration(windowDays)*24*time.Hour {
				return nil, fmt.Errorf("return window has expired")
			}
		}

		skus := make([]string, 0, len(req.Lines))
		for _, l := range req.Lines {
			if l.SKU != "" {
				skus = append(skus, l.SKU)
			}
		}
		if len(skus) > 0 {
			nonReturnableOverrides, _ := s.client.POSCatalogOverride.Query().
				Where(
					entoverride.TenantID(tenantID),
					entoverride.InventorySkuIn(skus...),
					entoverride.IsReturnableEQ(false),
				).
				All(ctx)
			if len(nonReturnableOverrides) > 0 {
				names := make([]string, 0, len(nonReturnableOverrides))
				for _, it := range nonReturnableOverrides {
					names = append(names, it.InventorySku)
				}
				return nil, fmt.Errorf("return not allowed for: %s", strings.Join(names, ", "))
			}
		}
	}

	returnType := req.ReturnType
	if returnType == "" {
		returnType = "refund"
	}

	onAccount := orders.SettledOnAccount(ctx, s.client, tenantID, req.OrderID)
	restrictOnAccount := s.creditSaleRefundRestricted(ctx, tenantID, req.OrderID)
	if perr := validateRefundChannel(reasonCodePtr(req.ReasonCode), posreturn.ReturnType(returnType), req.RefundChannel, onAccount, restrictOnAccount); perr != nil {
		return nil, perr
	}

	returnNumber := fmt.Sprintf("RET-%d", time.Now().UnixMilli())
	if s.seq != nil {
		if n, err := s.seq.GenerateNumber(ctx, tenantID, documents.DocTypePosReturn); err == nil && n != "" {
			returnNumber = n
		}
	}

	var refundAmount float64
	for _, l := range req.Lines {
		refundAmount += l.TotalPrice
	}

	md := map[string]any{"on_account_sale": onAccount}
	if req.BypassGuards {
		// BypassGuards is the admin Edit-Sale signal — tag the return's SOURCE too (distinct
		// from the guard bypass itself) so pos-ui's Returns list can badge it "Edited" instead
		// of a customer-initiated return, even though the underlying settlement is identical.
		md["source"] = "edit_sale"
	}

	ret, err := s.client.POSReturn.Create().
		SetTenantID(tenantID).
		SetOutletID(req.OutletID).
		SetOrderID(req.OrderID).
		SetReturnNumber(returnNumber).
		SetReturnType(posreturn.ReturnType(returnType)).
		SetStatus(posreturn.StatusPending).
		SetReason(req.Reason).
		SetNillableReasonCode(reasonCodePtr(req.ReasonCode)).
		SetNillableRefundChannel(refundChannelPtr(req.RefundChannel)).
		SetRefundAmount(refundAmount).
		SetRequestedBy(req.RequestedBy).
		SetMetadata(md).
		SetNillableCreatedAt(req.ReturnDate).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create return: %w", err)
	}

	for _, l := range req.Lines {
		if _, lerr := s.client.POSReturnLine.Create().
			SetReturnID(ret.ID).
			SetOrderLineID(l.OrderLineID).
			SetSku(l.SKU).
			SetName(l.Name).
			SetQuantity(l.Quantity).
			SetUnitPrice(l.UnitPrice).
			SetTotalPrice(l.TotalPrice).
			SetReason(l.Reason).
			Save(ctx); lerr != nil {
			s.log.Error("create return line failed", zap.Error(lerr))
		}
	}

	if s.auditSvc != nil {
		amt := refundAmount
		oid := req.OutletID
		s.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tenantID,
			OutletID:    &oid,
			ActorUserID: req.RequestedBy,
			Action:      "return.refund",
			EntityType:  "pos_return",
			EntityID:    ret.ID.String(),
			Reason:      req.Reason,
			Amount:      &amt,
			After:       map[string]any{"return_type": returnType, "order_id": req.OrderID.String(), "return_number": returnNumber},
		})
	}

	if s.publisher != nil {
		linesSummary := make([]map[string]any, 0, len(req.Lines))
		for _, l := range req.Lines {
			linesSummary = append(linesSummary, map[string]any{
				"sku": l.SKU, "name": l.Name, "quantity": l.Quantity, "total_price": l.TotalPrice,
			})
		}
		_ = s.publisher.PublishReturnInitiated(ctx, tenantID, map[string]any{
			"return_id":     ret.ID,
			"order_id":      req.OrderID,
			"outlet_id":     req.OutletID,
			"return_type":   returnType,
			"refund_amount": refundAmount,
			"lines":         linesSummary,
		})
	}

	return ret, nil
}

// ApproveReturn is the decision-only approve/reject step — no money movement here (see
// CompleteReturn's doc comment for why that split exists).
func (s *Service) ApproveReturn(ctx context.Context, tenantID uuid.UUID, returnID uuid.UUID, req ApproveReturnRequest) (*ent.POSReturn, error) {
	ret, err := s.client.POSReturn.Get(ctx, returnID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("return not found")
		}
		return nil, fmt.Errorf("get return: %w", err)
	}
	if ret.TenantID != tenantID {
		return nil, fmt.Errorf("return not found")
	}
	if ret.Status != posreturn.StatusPending {
		return nil, fmt.Errorf("return is not pending")
	}

	newStatus := posreturn.StatusApproved
	if req.Action == "reject" {
		newStatus = posreturn.StatusRejected
	}

	update := s.client.POSReturn.UpdateOne(ret).SetStatus(newStatus)
	if req.ApproverID != uuid.Nil {
		update = update.SetApprovedBy(req.ApproverID)
	}

	if rc := refundChannelPtr(req.RefundChannel); rc != nil {
		onAccount := orders.SettledOnAccount(ctx, s.client, tenantID, ret.OrderID)
		restrictOnAccount := s.creditSaleRefundRestricted(ctx, tenantID, ret.OrderID)
		if perr := validateRefundChannel(ret.ReasonCode, ret.ReturnType, string(*rc), onAccount, restrictOnAccount); perr != nil {
			return nil, perr
		}
		update = update.SetNillableRefundChannel(rc)
	}

	if strings.TrimSpace(req.Notes) != "" {
		md := cloneReturnMetadata(ret.Metadata)
		if newStatus == posreturn.StatusRejected {
			md["rejection_notes"] = req.Notes
		} else {
			md["approval_notes"] = req.Notes
		}
		update = update.SetMetadata(md)
	}

	updated, err := update.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update return: %w", err)
	}

	if s.auditSvc != nil {
		action := "return.approved"
		if newStatus == posreturn.StatusRejected {
			action = "return.rejected"
		}
		oid := ret.OutletID
		s.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tenantID,
			OutletID:    &oid,
			ActorUserID: req.ApproverID,
			Action:      action,
			EntityType:  "pos_return",
			EntityID:    returnID.String(),
			Reason:      req.Notes,
			After:       map[string]any{"status": string(newStatus), "return_number": ret.ReturnNumber},
		})
	}

	return updated, nil
}

// CompleteReturn is the final fulfilment step. Only an APPROVED return can be completed; it
// settles the money (treasury refund + eTIMS credit note) and publishes
// return.completed/exchange.completed (inventory restock + treasury settlement), then marks
// the return completed.
func (s *Service) CompleteReturn(ctx context.Context, tenantID uuid.UUID, tenantSlug string, returnID uuid.UUID, req CompleteReturnRequest) (*ent.POSReturn, *ExchangeResult, error) {
	ret, err := s.client.POSReturn.Get(ctx, returnID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil, fmt.Errorf("return not found")
		}
		return nil, nil, fmt.Errorf("get return: %w", err)
	}
	if ret.TenantID != tenantID {
		return nil, nil, fmt.Errorf("return not found")
	}
	if ret.Status != posreturn.StatusApproved {
		return nil, nil, fmt.Errorf("only an approved return can be completed")
	}

	lines, _ := s.client.POSReturnLine.Query().
		Where(posreturnline.ReturnID(returnID)).
		All(ctx)

	update := s.client.POSReturn.UpdateOne(ret).SetStatus(posreturn.StatusCompleted)

	onAccount := orders.SettledOnAccount(ctx, s.client, tenantID, ret.OrderID)
	refundChannel := ""
	if ret.RefundChannel != nil {
		refundChannel = string(*ret.RefundChannel)
	}
	if rc := refundChannelPtr(req.RefundChannel); rc != nil {
		refundChannel = string(*rc)
		update = update.SetNillableRefundChannel(rc)
	}
	if refundChannel == "" {
		refundChannel = defaultRefundChannel(ret.ReturnType, onAccount)
		if rc := refundChannelPtr(refundChannel); rc != nil {
			update = update.SetNillableRefundChannel(rc)
		}
	}
	restrictOnAccount := s.creditSaleRefundRestricted(ctx, tenantID, ret.OrderID)
	if perr := validateRefundChannel(ret.ReasonCode, ret.ReturnType, refundChannel, onAccount, restrictOnAccount); perr != nil {
		return nil, nil, perr
	}

	// Exchange fulfilment: create the replacement order and net the price difference —
	// dearer replacement → the delta is collected as a normal payment on the new order;
	// cheaper → the leftover is refunded through the (policy-validated) channel below.
	exchange, exErr := s.fulfilExchange(ctx, tenantID, ret, req)
	if exErr != nil {
		return nil, nil, exErr
	}
	if exchange != nil {
		update = update.SetExchangeOrderID(exchange.OrderID)
	}

	// Money movement: settle in treasury for a cash/mpesa/bank refund, a store-credit return,
	// an offset_invoice (credit-sale) return — and the LEFTOVER of an exchange whose
	// replacement is cheaper than the returned goods. A store-credit return type with no
	// channel persisted yet defaults to the store_credit channel — but ONLY when the original
	// sale was NOT on account (see the on-account guard above). For an exchange the
	// replacement order's exchange-credit discount already nets the revenue, so only the
	// leftover moves money.
	settleAmount := ret.RefundAmount
	if ret.ReturnType == posreturn.ReturnTypeExchange {
		settleAmount = 0
		if exchange != nil {
			settleAmount = exchange.Leftover
		}
	}
	var treasuryRefundRef string
	if settleAmount > 0.009 && s.treasuryClient != nil {
		if ret.ReturnType == posreturn.ReturnTypeStoreCredit && !onAccount {
			refundChannel = "store_credit"
		}

		// Sum the returned lines' VAT and COGS so treasury can reverse the exact tax + cost-of-goods.
		taxAmount := s.resolveReturnTax(ctx, ret.OrderID, lines)
		costAmount := s.resolveReturnCost(ctx, tenantID, lines)
		if ret.ReturnType == posreturn.ReturnTypeExchange && ret.RefundAmount > 0 {
			taxAmount = taxAmount * (settleAmount / ret.RefundAmount)
			costAmount = 0
		}

		crmContactID, customerName, customerIdentifier := orders.ResolveOrderCustomer(ctx, s.client, tenantID, ret.OrderID)

		// A refund must settle in the ORIGINAL sale's currency, not a hardcoded assumption.
		currency := "KES"
		if origOrder, oerr := s.client.POSOrder.Get(ctx, ret.OrderID); oerr == nil && origOrder.Currency != "" {
			currency = origOrder.Currency
		}

		refundResp, refundErr := s.treasuryClient.CreateRefund(ctx, tenantSlug, returnID.String(), treasury.RefundRequest{
			SourceService:      "pos",
			ReferenceID:        returnID.String(),
			ReferenceType:      "pos_return",
			Reference:          ret.ReturnNumber,
			Amount:             settleAmount,
			TaxAmount:          taxAmount,
			Cost:               costAmount,
			Currency:           currency,
			Reason:             ret.Reason,
			RefundChannel:      refundChannel,
			CrmContactID:       crmContactID,
			CustomerIdentifier: customerIdentifier,
			CustomerName:       customerName,
		})
		if refundErr != nil {
			s.log.Error("treasury refund call failed (non-fatal; refund can be retried)",
				zap.Error(refundErr), zap.String("return_id", returnID.String()),
				zap.String("refund_channel", refundChannel), zap.Float64("amount", settleAmount),
				zap.Float64("tax_amount", taxAmount), zap.Float64("cost", costAmount))
		} else {
			treasuryRefundRef = refundResp.ID
			update = update.SetTreasuryRefundRef(treasuryRefundRef)
		}
	}

	// eTIMS credit note: a returned, tax-invoiced sale needs a VAT-reversal credit note in
	// treasury — including an EXCHANGE (the exchanged-away item's original sale is just as
	// fiscally reversed as a refund's). Best-effort + non-fatal.
	if s.treasuryClient != nil &&
		(ret.ReturnType == posreturn.ReturnTypeRefund || ret.ReturnType == posreturn.ReturnTypeStoreCredit || ret.ReturnType == posreturn.ReturnTypeExchange) {
		if fiscalized, invoiceID, _ := orders.IsFiscalized(ctx, s.treasuryClient, tenantSlug, ret.OrderID); fiscalized {
			if cn, cnErr := s.treasuryClient.CreateCreditNote(ctx, tenantSlug, invoiceID); cnErr != nil {
				s.log.Warn("eTIMS credit-note creation failed (non-fatal)", zap.Error(cnErr), zap.String("return_id", returnID.String()))
			} else {
				s.log.Info("eTIMS credit-note issued for return", zap.String("return_id", returnID.String()), zap.String("credit_note", cn.Number))
			}
		}
	}

	md := cloneReturnMetadata(ret.Metadata)
	md["completed_at"] = time.Now().UTC().Format(time.RFC3339)
	if req.CompletedBy != uuid.Nil {
		md["completed_by"] = req.CompletedBy.String()
	}
	if strings.TrimSpace(req.Notes) != "" {
		md["completion_notes"] = req.Notes
	}
	update = update.SetMetadata(md)

	updated, err := update.Save(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("complete return: %w", err)
	}

	if s.auditSvc != nil {
		amt := ret.RefundAmount
		oid := ret.OutletID
		s.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tenantID,
			OutletID:    &oid,
			ActorUserID: req.CompletedBy,
			Action:      "return.completed",
			EntityType:  "pos_return",
			EntityID:    returnID.String(),
			Reason:      ret.Reason,
			Amount:      &amt,
			After:       map[string]any{"return_number": ret.ReturnNumber, "refund_channel": refundChannel, "treasury_refund_ref": treasuryRefundRef},
		})
	}

	if s.publisher != nil {
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
			if exchange != nil {
				eventData["exchange_order_id"] = exchange.OrderID
				eventData["exchange_credit"] = exchange.ExchangeCredit
				eventData["amount_payable"] = exchange.AmountPayable
			}
			_ = s.publisher.PublishExchangeCompleted(ctx, tenantID, eventData)
		} else {
			_ = s.publisher.PublishReturnCompleted(ctx, tenantID, eventData)
		}
	}

	return updated, exchange, nil
}

// CreateAndAutoComplete runs create → approve → complete back-to-back in one call, for an
// already-authorized admin caller (saleedit's fiscalized-reduction path) rather than the
// customer-facing three-stage UI flow. Always bypasses the return-window/returnable guards
// (see CreateRequest.BypassGuards) — an admin correcting a data-entry error is not a
// customer physically returning goods. Never an exchange (Edit-Sale reductions have no
// replacement items).
func (s *Service) CreateAndAutoComplete(ctx context.Context, tenantID uuid.UUID, tenantSlug string, req AutoCompleteRequest) (*ent.POSReturn, error) {
	ret, err := s.CreateReturn(ctx, tenantID, CreateReturnRequest{
		OrderID:      req.OrderID,
		OutletID:     req.OutletID,
		ReturnType:   "refund",
		Reason:       req.Reason,
		ReasonCode:   req.ReasonCode,
		Lines:        req.Lines,
		RequestedBy:  req.RequestedBy,
		BypassGuards: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	if _, err := s.ApproveReturn(ctx, tenantID, ret.ID, ApproveReturnRequest{
		Action:     "approve",
		ApproverID: req.RequestedBy,
	}); err != nil {
		return nil, fmt.Errorf("approve: %w", err)
	}
	completed, _, err := s.CompleteReturn(ctx, tenantID, tenantSlug, ret.ID, CompleteReturnRequest{
		CompletedBy: req.RequestedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}

	// Loyalty/commission clawback: CompleteReturn itself never does this (a plain customer
	// return doesn't automatically claw back points/commission today — a separate, existing
	// product behavior this method deliberately does not change). But routing a fiscalized
	// Edit-Sale reduction through this auto-complete path must NOT silently regress what the
	// non-fiscalized reversals-based path already did (reversals.stepLoyaltyCommission) — so
	// this admin-driven path claws back explicitly, using the same shared logic.
	s.clawbackLoyaltyAndCommission(ctx, tenantID, req.OrderID, completed, req.Lines)

	return completed, nil
}

// clawbackLoyaltyAndCommission prorates the clawback by the completed return's refund amount
// against the original order's total (1.0 if the order has no total to prorate against —
// matches reversals' own "no total, skip proration" guard by simply capping at 1). Best-effort:
// a lookup/write error here never fails the already-completed return.
func (s *Service) clawbackLoyaltyAndCommission(ctx context.Context, tenantID, orderID uuid.UUID, ret *ent.POSReturn, lines []LineInput) {
	order, err := s.client.POSOrder.Query().Where(entposorder.ID(orderID), entposorder.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return
	}
	ratio := 1.0
	if order.TotalAmount > 0.009 {
		ratio = ret.RefundAmount / order.TotalAmount
		if ratio > 1 {
			ratio = 1
		}
	}
	skus := map[string]bool{}
	for _, l := range lines {
		if l.SKU != "" {
			skus[l.SKU] = true
		}
	}
	reasonRef := ret.ReturnNumber
	if d := salecorrection.ReverseLoyalty(ctx, s.log, s.client, tenantID, orderID, ratio, reasonRef); d != "" {
		s.log.Info("edit-sale return: loyalty clawed back", zap.String("return_number", reasonRef), zap.String("detail", d))
	}
	if d := salecorrection.ReverseCommission(ctx, s.log, s.client, tenantID, orderID, skus, reasonRef); d != "" {
		s.log.Info("edit-sale return: commission voided", zap.String("return_number", reasonRef), zap.String("detail", d))
	}
}

// resolveReturnTax sums the VAT to reverse for the returned lines. The return line stores no tax, so
// we prorate the original POSOrderLine's tax_amount by the returned quantity (tax × returnedQty/lineQty).
// Lines without a matching priced order line or with no tax contribute 0. Errors => 0 (never blocks).
func (s *Service) resolveReturnTax(ctx context.Context, orderID uuid.UUID, lines []*ent.POSReturnLine) float64 {
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
	orderLines, err := s.client.POSOrderLine.Query().
		Where(entposorderline.OrderID(orderID), entposorderline.IDIn(ids...)).
		All(ctx)
	if err != nil {
		s.log.Warn("return refund: failed to resolve line tax (defaulting to 0)", zap.Error(err))
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
func (s *Service) resolveReturnCost(ctx context.Context, tenantID uuid.UUID, lines []*ent.POSReturnLine) float64 {
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
	costBySKU := orders.CatalogCostBySKU(ctx, s.client, tenantID, skus)
	var total float64
	for _, l := range lines {
		total += costBySKU[l.Sku] * l.Quantity
	}
	return total
}
