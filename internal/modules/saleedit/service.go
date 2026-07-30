// Package saleedit implements the tenant-admin "Edit Sale" tool for a FINALIZED sale (full
// line/qty/price/customer/payment/date edit). Rather than mutating a finalized order's money
// fields in place — which would break the immutable-ledger assumption every other flow in this
// codebase relies on (GL, eTIMS, COGS) — edit = reverse the original + create a replacement, the
// same safe pattern accountants use for correcting a posted invoice.
//
// PrepareEdit reverses the original sale via the SAME reversals.Service engine the platform-owner
// Txn Reversal tool and saledelete's fiscalised branch use (unmodified) — regardless of eTIMS
// status, an edit always keeps full history on both sides (only Delete gets a hard-wipe branch).
// It never creates the replacement order itself: pos-ui's existing, already-proven order-creation
// pipeline (Add Sale) does that, tagged with metadata.edited_from_order_id — reusing that pipeline
// verbatim is safer than re-implementing checkout/settlement here.
package saleedit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposrefund "github.com/bengobox/pos-service/internal/ent/posrefund"
	entposreturn "github.com/bengobox/pos-service/internal/ent/posreturn"
	entposreversal "github.com/bengobox/pos-service/internal/ent/posreversal"
	"github.com/bengobox/pos-service/internal/modules/reversals"
)

// finalizedStatuses mirrors reversals.finalizedStatuses / saledelete.finalizedStatuses — only a
// settled sale can be edited.
var finalizedStatuses = map[string]bool{"completed": true, "paid": true, "closed": true}

// Request describes a sale to prepare for editing.
type Request struct {
	OrderID     uuid.UUID
	Reason      string
	RequestedBy uuid.UUID
	TenantSlug  string
}

// Result is what the handler returns — pos-ui uses OrderNumber/CustomerName etc. (already
// available from its own GET /orders/{id}) so this only needs to confirm success + carry the
// reversal reference for the audit trail / UI toast.
type Result struct {
	OrderID        uuid.UUID `json:"order_id"`
	ReversalID     uuid.UUID `json:"reversal_id"`
	ReversalNumber string    `json:"reversal_number"`
}

// Service orchestrates the Edit-Sale "prepare" step.
type Service struct {
	log         *zap.Logger
	client      *ent.Client
	reversalSvc *reversals.Service
	auditSvc    *audit.Service
}

// NewService wires the Edit-Sale orchestrator.
func NewService(log *zap.Logger, client *ent.Client, reversalSvc *reversals.Service) *Service {
	return &Service{log: log.Named("saleedit"), client: client, reversalSvc: reversalSvc}
}

// SetAuditService wires the centralized audit trail.
func (s *Service) SetAuditService(a *audit.Service) { s.auditSvc = a }

// PrepareEdit validates the order and reverses it (GL/inventory/eTIMS), marking it superseded.
// The caller (pos-ui) then creates the replacement order through the normal Add Sale pipeline.
func (s *Service) PrepareEdit(ctx context.Context, tenantID uuid.UUID, req Request) (*Result, error) {
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
	if hasHistory, err := s.hasCorrectionHistory(ctx, tenantID, order.ID); err != nil {
		return nil, fmt.Errorf("check correction history: %w", err)
	} else if hasHistory {
		return nil, fmt.Errorf("this sale already has a return, refund, or reversal on record — resolve that first")
	}
	if already, ok := order.Metadata["edit"].(map[string]any); ok && already != nil {
		return nil, fmt.Errorf("this sale is already being edited (see %v)", already["replacement_pending"])
	}

	rev, err := s.reversalSvc.Execute(ctx, tenantID, reversals.CreateRequest{
		OrderID:     order.ID,
		Scope:       "full",
		Reason:      fmt.Sprintf("Edit Sale: %s", req.Reason),
		RequestedBy: req.RequestedBy,
		TenantSlug:  req.TenantSlug,
	})
	if err != nil {
		return nil, fmt.Errorf("reverse original sale: %w", err)
	}

	md := map[string]any{}
	for k, v := range order.Metadata {
		md[k] = v
	}
	md["edit"] = map[string]any{
		"edited_by":           req.RequestedBy.String(),
		"edit_reason":         req.Reason,
		"edited_at":           time.Now().UTC().Format(time.RFC3339),
		"reversal_id":         rev.ID.String(),
		"reversal_number":     rev.ReversalNumber,
		"replacement_pending": true,
	}
	if _, err := order.Update().SetMetadata(md).Save(ctx); err != nil {
		return nil, fmt.Errorf("mark order as edited: %w", err)
	}

	s.audit(ctx, tenantID, req.RequestedBy, order.OrderNumber, order.ID, req.Reason, rev.ID)

	return &Result{OrderID: order.ID, ReversalID: rev.ID, ReversalNumber: rev.ReversalNumber}, nil
}

func (s *Service) hasCorrectionHistory(ctx context.Context, tenantID, orderID uuid.UUID) (bool, error) {
	if ok, err := s.client.POSReturn.Query().Where(entposreturn.TenantID(tenantID), entposreturn.OrderID(orderID)).Exist(ctx); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	if ok, err := s.client.POSRefund.Query().Where(entposrefund.OrderID(orderID)).Exist(ctx); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	if ok, err := s.client.POSReversal.Query().Where(entposreversal.TenantID(tenantID), entposreversal.OrderID(orderID)).Exist(ctx); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	return false, nil
}

// EditRequest describes an in-place edit: zero or more existing lines to remove/reduce.
// Added value (new items, or raising an existing line's qty/price) is NOT expressed here —
// pos-ui posts that as a separate, linked addendum sale through the normal create-order
// pipeline (see ApplyEdit's doc comment).
type EditRequest struct {
	OrderID     uuid.UUID
	Reason      string
	RequestedBy uuid.UUID
	TenantSlug  string
	Reductions  []reversals.LineSelection
}

// EditResult is what ApplyEdit returns to the caller.
type EditResult struct {
	OrderID        uuid.UUID  `json:"order_id"`
	ReversalID     *uuid.UUID `json:"reversal_id,omitempty"`
	ReversalNumber string     `json:"reversal_number,omitempty"`
}

// ApplyEdit performs a TRUE in-place edit of a finalized sale: reducing/removing the given
// lines runs a PARTIAL reversal (reversals.Service, scope "partial") — inventory add-back,
// treasury GL refund, an eTIMS credit note only if the tenant is actually fiscalized, and
// loyalty/commission clawback are all prorated to just the reduced value, and the order's
// status is left exactly as it was (never flipped to "refunded" — that partial-scope
// behavior already exists in reversals.stepPOSTotals). Unlike PrepareEdit (the full-reversal
// tool this replaces for everyday edits), a sale with prior edit/return/reversal history is
// NOT refused — repeat corrections over time are expected. Increases (a new item, or raising
// an existing line's qty/price) are intentionally out of scope here: they can't be posted onto
// an already-signed eTIMS invoice / already-posted GL entry in place, so pos-ui creates a small
// linked addendum sale for them (metadata.related_order_id) through the ordinary, already-correct
// create -> finalize pipeline instead — no new treasury/inventory endpoints needed for that side.
func (s *Service) ApplyEdit(ctx context.Context, tenantID uuid.UUID, req EditRequest) (*EditResult, error) {
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

	result := &EditResult{OrderID: order.ID}
	if len(req.Reductions) == 0 {
		// A pure-increase edit needs no backend call here — pos-ui goes straight to creating
		// the addendum sale.
		return result, nil
	}

	rev, err := s.reversalSvc.Execute(ctx, tenantID, reversals.CreateRequest{
		OrderID:     order.ID,
		Scope:       "partial",
		Lines:       req.Reductions,
		Reason:      fmt.Sprintf("Edit Sale: %s", req.Reason),
		RequestedBy: req.RequestedBy,
		TenantSlug:  req.TenantSlug,
	})
	if err != nil {
		// resolveLines refuses a line already carrying ANY voided_qty ("line already voided")
		// — the reversal engine's per-line idempotency guard means a line can only be
		// reduced once, ever, through this path. Surface that plainly rather than the raw
		// engine error, since "already voided" reads like a bug report otherwise.
		if strings.Contains(err.Error(), "already voided") {
			return nil, fmt.Errorf("one of these lines was already adjusted by a previous edit and can't be reduced again — remove it and add the corrected quantity as a new line instead")
		}
		return nil, fmt.Errorf("reduce line(s): %w", err)
	}
	result.ReversalID = &rev.ID
	result.ReversalNumber = rev.ReversalNumber

	md := map[string]any{}
	for k, v := range order.Metadata {
		md[k] = v
	}
	edits, _ := md["edits"].([]any)
	edits = append(edits, map[string]any{
		"edited_by":       req.RequestedBy.String(),
		"edit_reason":     req.Reason,
		"edited_at":       time.Now().UTC().Format(time.RFC3339),
		"reversal_id":     rev.ID.String(),
		"reversal_number": rev.ReversalNumber,
	})
	md["edits"] = edits
	if _, err := order.Update().SetMetadata(md).Save(ctx); err != nil {
		s.log.Warn("apply-edit: failed to stamp edit history", zap.Error(err))
	}

	s.audit(ctx, tenantID, req.RequestedBy, order.OrderNumber, order.ID, req.Reason, rev.ID)

	return result, nil
}

func (s *Service) audit(ctx context.Context, tenantID, actorID uuid.UUID, orderNumber string, orderID uuid.UUID, reason string, reversalID uuid.UUID) {
	if s.auditSvc == nil {
		return
	}
	s.auditSvc.Record(ctx, audit.Entry{
		TenantID:    tenantID,
		ActorUserID: actorID,
		Action:      "order.edit_prepare",
		EntityType:  "pos_order",
		EntityID:    orderID.String(),
		Reason:      reason,
		After: map[string]any{
			"order_number": orderNumber,
			"reversal_id":  reversalID.String(),
		},
	})
}
