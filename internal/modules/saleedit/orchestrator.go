package saleedit

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	entposreturn "github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/possaleedit"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/returns"
	"github.com/bengobox/pos-service/internal/modules/reversals"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// SetTreasuryClient wires the treasury S2S client — the per-order fiscalization check, the
// tenant eTIMS pre-check, and the in-place-increase GL/AR posting all need it.
func (s *Service) SetTreasuryClient(c *treasury.Client) { s.treasuryClient = c }

// SetInventoryClient wires the inventory S2S client for in-place-increase consumption.
func (s *Service) SetInventoryClient(c *inventory.Client) { s.inventoryClient = c }

// SetOrderService wires the orders service — RecomputeTotals for in-place increases, and
// CreateOrder for a fiscalized-increase addendum order.
func (s *Service) SetOrderService(svc *orders.Service) { s.orderSvc = svc }

// SetReturnsService wires the returns service — CreateAndAutoComplete for fiscalized
// reductions.
func (s *Service) SetReturnsService(svc *returns.Service) { s.returnsSvc = svc }

// Edit is the single centralized entry point for editing a finalized sale: the caller sends
// the full desired line set, and this orchestrator diffs it against the live order, resolves
// the fiscalization/AR policy ONCE, and routes each bucket of the diff to the correct
// sub-flow:
//   - non-fiscalized: every bucket (removed/reduced/increased/added) is a TRUE in-place edit
//     of the SAME order — no new order, no new receipt, no new transaction record.
//   - fiscalized: removed/reduced routes through returns.CreateAndAutoComplete (a real
//     auto-completed POSReturn + credit note); added/increased routes through the existing
//     linked-addendum-order pattern (a new order with its own docket).
//
// Both halves can fire in the same call for a mixed edit (e.g. remove one line, add another).
func (s *Service) Edit(ctx context.Context, tenantID uuid.UUID, req EditSaleRequest) (*EditSaleResult, error) {
	if req.Reason == "" {
		return nil, fmt.Errorf("reason is required")
	}

	order, err := s.client.POSOrder.Query().
		Where(entposorder.ID(req.OrderID), entposorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("order not found")
	}
	if order.DeletedAt != nil {
		return nil, fmt.Errorf("sale is deleted and cannot be edited")
	}
	if !finalizedStatuses[order.Status] {
		return nil, fmt.Errorf("only a finalized sale (completed/paid/closed) can be edited — this order is %q", order.Status)
	}
	if order.Status == "refunded" {
		return nil, fmt.Errorf("this sale has already been fully reversed — there is nothing left to edit")
	}
	// Unified correction-history policy: repeat edits are allowed (both reduce and increase)
	// unless a FULL reversal already ran against this order — that's terminal, matching
	// order.Status=="refunded" above for the case where the full-reversal history predates
	// this order's current status for some other reason. A prior partial reversal or a
	// completed POSReturn is NOT terminal — corrections over time are expected.
	if hasFull, err := orders.HasFullReversal(ctx, s.client, tenantID, order.ID); err == nil && hasFull {
		return nil, fmt.Errorf("this sale has already been fully reversed — there is nothing left to edit")
	}

	allLines, err := s.client.POSOrderLine.Query().Where(entposorderline.OrderID(order.ID)).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load lines: %w", err)
	}
	returnedQty, err := returnedQtyByLine(ctx, s.client, order.ID)
	if err != nil {
		return nil, fmt.Errorf("load prior fiscalized-return quantities: %w", err)
	}
	activeLines := make([]*ent.POSOrderLine, 0, len(allLines))
	for _, l := range allLines {
		if effectiveRemainingQty(l, returnedQty) > 0.009 {
			activeLines = append(activeLines, l)
		}
	}

	diff := diffLines(activeLines, req.Lines, returnedQty)
	hasDecrease := len(diff.removed) > 0 || len(diff.reduced) > 0
	hasIncrease := len(diff.added) > 0 || len(diff.increased) > 0
	if !hasDecrease && !hasIncrease {
		if len(diff.priceOnlySkipped) > 0 {
			return nil, fmt.Errorf("a price-only change on an unchanged quantity isn't supported yet — remove the line and re-add it at the new price instead")
		}
		return nil, fmt.Errorf("nothing to save")
	}

	pol := s.resolvePolicy(ctx, tenantID, req.TenantSlug, order.ID)
	kind := classifyKind(diff)
	editID := uuid.New()

	saleEdit, err := s.client.POSSaleEdit.Create().
		SetID(editID).
		SetTenantID(tenantID).
		SetOrderID(order.ID).
		SetOrderNumber(order.OrderNumber).
		SetKind(possaleedit.Kind(kind)).
		SetFiscalizedAtTime(pol.fiscalized).
		SetStatus("pending").
		SetLinesBefore(snapshotLines(activeLines, returnedQty)).
		SetReason(req.Reason).
		SetRequestedBy(req.RequestedBy).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create sale edit record: %w", err)
	}

	result := &EditSaleResult{
		OrderID: order.ID, SaleEditID: editID, Kind: kind, Fiscalized: pol.fiscalized,
		PriceOnlyLinesSkipped: diff.priceOnlySkipped,
	}

	var stepErr error
	if hasDecrease {
		if pol.fiscalized {
			ret, rerr := s.returnsSvc.CreateAndAutoComplete(ctx, tenantID, req.TenantSlug, returns.AutoCompleteRequest{
				OrderID: order.ID, OutletID: order.OutletID, Reason: req.Reason,
				Lines: buildReturnLines(diff.removed, diff.reduced, returnedQty), RequestedBy: req.RequestedBy, Source: "edit_sale",
			})
			if rerr != nil {
				stepErr = fmt.Errorf("reduce (return): %w", rerr)
			} else {
				result.LinkedReturnID = &ret.ID
			}
		} else {
			rev, rerr := s.reversalSvc.Execute(ctx, tenantID, reversals.CreateRequest{
				OrderID: order.ID, Scope: "partial", Lines: buildLineSelections(diff.removed, diff.reduced),
				Reason: fmt.Sprintf("Edit Sale: %s", req.Reason), RequestedBy: req.RequestedBy, TenantSlug: req.TenantSlug,
			})
			if rerr != nil {
				stepErr = fmt.Errorf("reduce: %w", rerr)
			} else {
				result.LinkedReversalID = &rev.ID
			}
		}
	}

	if stepErr == nil && hasIncrease {
		if pol.fiscalized {
			addendumOrderID, aerr := s.createAddendumOrder(ctx, tenantID, order, diff, req)
			if aerr != nil {
				stepErr = fmt.Errorf("increase (addendum): %w", aerr)
			} else {
				result.LinkedAddendumOrderID = &addendumOrderID
			}
		} else {
			if ierr := s.applyInPlaceIncrease(ctx, tenantID, order, editID, diff, req); ierr != nil {
				stepErr = fmt.Errorf("increase (in-place): %w", ierr)
			}
		}
	}

	afterLines, _ := s.client.POSOrderLine.Query().Where(entposorderline.OrderID(order.ID)).All(ctx)
	afterReturnedQty, aerr := returnedQtyByLine(ctx, s.client, order.ID)
	if aerr != nil {
		afterReturnedQty = returnedQty // best-effort fallback — stale but never worse than before
	}
	status := "completed"
	if stepErr != nil {
		status = "failed"
	}
	if _, uerr := saleEdit.Update().
		SetStatus(possaleedit.Status(status)).
		SetLinesAfter(snapshotLines(afterLines, afterReturnedQty)).
		SetNillableLinkedReversalID(result.LinkedReversalID).
		SetNillableLinkedReturnID(result.LinkedReturnID).
		SetNillableLinkedAddendumOrderID(result.LinkedAddendumOrderID).
		Save(ctx); uerr != nil {
		s.log.Warn("edit: failed to finalize sale-edit record", zap.Error(uerr))
	}

	s.auditEdit(ctx, tenantID, req.RequestedBy, order, kind, req.Reason, editID, activeLines, afterLines, returnedQty, afterReturnedQty)

	if stepErr != nil {
		return result, stepErr
	}
	return result, nil
}

// returnedQtyByLine sums, per order line, the quantity already removed via a completed
// fiscalized POSReturn against this order — see diffLines' doc comment for why this is
// necessary on top of VoidedQty. Only COMPLETED returns count (a pending/rejected return
// never actually took the stock/money back).
func returnedQtyByLine(ctx context.Context, client *ent.Client, orderID uuid.UUID) (map[uuid.UUID]float64, error) {
	rets, err := client.POSReturn.Query().
		Where(entposreturn.OrderID(orderID), entposreturn.StatusEQ(entposreturn.StatusCompleted)).
		WithLines().
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]float64, len(rets))
	for _, ret := range rets {
		for _, l := range ret.Edges.Lines {
			out[l.OrderLineID] = round2(out[l.OrderLineID] + l.Quantity)
		}
	}
	return out, nil
}

// buildReturnLines converts removed/reduced diff entries into returns.LineInput, prorating
// each reduced line's value by the fraction being removed.
func buildReturnLines(removed []*ent.POSOrderLine, reduced []reducedLine, returnedQty map[uuid.UUID]float64) []returns.LineInput {
	out := make([]returns.LineInput, 0, len(removed)+len(reduced))
	for _, l := range removed {
		remaining := effectiveRemainingQty(l, returnedQty)
		ratio := 1.0
		if l.Quantity > 0 {
			ratio = remaining / l.Quantity
		}
		out = append(out, returns.LineInput{
			OrderLineID: l.ID, SKU: l.Sku, Name: l.Name, Quantity: remaining,
			UnitPrice: l.UnitPrice, TotalPrice: round2(l.TotalPrice * ratio),
		})
	}
	for _, r := range reduced {
		ratio := 0.0
		if r.Line.Quantity > 0 {
			ratio = r.ByQty / r.Line.Quantity
		}
		out = append(out, returns.LineInput{
			OrderLineID: r.Line.ID, SKU: r.Line.Sku, Name: r.Line.Name, Quantity: r.ByQty,
			UnitPrice: r.Line.UnitPrice, TotalPrice: round2(r.Line.TotalPrice * ratio),
		})
	}
	return out
}

// buildLineSelections converts removed/reduced diff entries into reversals.LineSelection —
// Quantity 0 means "whole line" per that package's own convention.
func buildLineSelections(removed []*ent.POSOrderLine, reduced []reducedLine) []reversals.LineSelection {
	out := make([]reversals.LineSelection, 0, len(removed)+len(reduced))
	for _, l := range removed {
		out = append(out, reversals.LineSelection{LineID: l.ID})
	}
	for _, r := range reduced {
		out = append(out, reversals.LineSelection{LineID: r.Line.ID, Quantity: r.ByQty})
	}
	return out
}

// snapshotLines converts live order lines into the audit-trail JSON shape. returnedQty
// (see returnedQtyByLine) makes the snapshotted Quantity reflect prior fiscalized returns
// too, not just VoidedQty — otherwise a fiscalized line's audit snapshot would understate
// how much of it is actually still active.
func snapshotLines(lines []*ent.POSOrderLine, returnedQty map[uuid.UUID]float64) []entschema.SaleEditLineSnapshotJSON {
	out := make([]entschema.SaleEditLineSnapshotJSON, 0, len(lines))
	for _, l := range lines {
		out = append(out, entschema.SaleEditLineSnapshotJSON{
			LineID: l.ID, SKU: l.Sku, Name: l.Name,
			Quantity: effectiveRemainingQty(l, returnedQty), UnitPrice: l.UnitPrice, TotalPrice: l.TotalPrice,
		})
	}
	return out
}

func (s *Service) auditEdit(ctx context.Context, tenantID, actorID uuid.UUID, order *ent.POSOrder, kind, reason string, editID uuid.UUID, before, after []*ent.POSOrderLine, beforeReturnedQty, afterReturnedQty map[uuid.UUID]float64) {
	if s.auditSvc == nil {
		return
	}
	action := map[string]string{
		"reduction": "order.edit_reduce", "increase": "order.edit_increase",
		"mixed": "order.edit_mixed", "price_only": "order.edit_price",
	}[kind]
	if action == "" {
		action = "order.edit"
	}
	s.auditSvc.Record(ctx, audit.Entry{
		TenantID:    tenantID,
		ActorUserID: actorID,
		Action:      action,
		EntityType:  "pos_order",
		EntityID:    order.ID.String(),
		Reason:      reason,
		Before:      map[string]any{"lines": snapshotLines(before, beforeReturnedQty)},
		After: map[string]any{
			"order_number": order.OrderNumber,
			"sale_edit_id": editID.String(),
			"lines":        snapshotLines(after, afterReturnedQty),
		},
	})
}
