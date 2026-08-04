// Package saledelete implements the tenant-admin "Delete Sale" tool. It is built ENTIRELY on top
// of the existing internal/modules/reversals engine (the platform-owner Txn Reversal tool) rather
// than re-implementing cross-service orchestration:
//
//   - Fiscalised sale (has a KRA eTIMS-signed treasury tax invoice): "sales transmitted to KRA
//     cannot be deleted" — run the SAME reversal engine reversals.Service uses (unmodified) to
//     reverse GL/inventory/eTIMS, then soft-mark the order deleted (deleted_at). The order, its
//     lines and payments are KEPT — only excluded from lists/reports by default.
//   - Non-fiscalised sale (the common cash-sale case): snapshot everything into a permanent
//     POSSaleShred audit record, restore inventory stock (same idempotent reversal call), HARD
//     DELETE the treasury ledger entries (genuine deletion, not a contra-entry — see
//     treasury.Client.ShredLedger), then hard-delete pos-api's own rows.
//
// A sale that has ANY prior return/refund/reversal history is refused outright — Delete is for a
// plain, untouched finalized sale; an already-corrected sale should go through the tool that
// already handled it.
package saledelete

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposrefund "github.com/bengobox/pos-service/internal/ent/posrefund"
	entposreturn "github.com/bengobox/pos-service/internal/ent/posreturn"
	entposreversal "github.com/bengobox/pos-service/internal/ent/posreversal"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/reversals"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// Step names, mirroring the reversals package's tracked-step convention.
const (
	StepFiscalCheck   = "fiscal_check"
	StepReversal      = "reversal"       // fiscalised branch: delegates to reversals.Service
	StepInventory     = "inventory_restore"
	StepTreasuryLedger = "treasury_ledger_shred"
	// StepARWriteoff reduces the customer's outstanding AR when the deleted sale was settled on
	// account — a real gap fixed 2026: previously deleteNonFiscalized only shredded GL journal
	// entries and left CustomerBalance.balance_due exactly as it was, stranding debt for a sale
	// that no longer existed anywhere. See treasury.Client.WriteOffDebt's doc comment for why
	// this is a distinct, GL-free call rather than RecordARPayment.
	StepARWriteoff    = "ar_writeoff"
	StepPOSHardDelete = "pos_hard_delete"
)

const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusSkipped   = "skipped"
)

// Request describes a sale to delete.
type Request struct {
	OrderID     uuid.UUID
	Reason      string
	RequestedBy uuid.UUID
	TenantSlug  string // forwarded to treasury S2S calls
}

// Service orchestrates the Delete-Sale tool.
type Service struct {
	log             *zap.Logger
	client          *ent.Client
	reversalSvc     *reversals.Service
	treasuryClient  *treasury.Client
	inventoryClient *inventory.Client
	auditSvc        *audit.Service
}

// NewService wires the Delete-Sale orchestrator.
func NewService(log *zap.Logger, client *ent.Client, reversalSvc *reversals.Service, treasuryClient *treasury.Client, inventoryClient *inventory.Client) *Service {
	return &Service{
		log:             log.Named("saledelete"),
		client:          client,
		reversalSvc:     reversalSvc,
		treasuryClient:  treasuryClient,
		inventoryClient: inventoryClient,
	}
}

// SetAuditService wires the centralized audit trail.
func (s *Service) SetAuditService(a *audit.Service) { s.auditSvc = a }

// finalizedStatuses mirrors reversals.finalizedStatuses — only a settled sale can be deleted.
var finalizedStatuses = map[string]bool{"completed": true, "paid": true, "closed": true}

// Delete validates and executes a sale deletion, branching on fiscal status.
func (s *Service) Delete(ctx context.Context, tenantID uuid.UUID, req Request) (*Result, error) {
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
		return nil, fmt.Errorf("sale already deleted")
	}
	if !finalizedStatuses[order.Status] {
		return nil, fmt.Errorf("only a finalized sale (completed/paid/closed) can be deleted — this order is %q", order.Status)
	}

	// Refuse a sale that already has return/refund/reversal history — Delete is for a plain,
	// untouched sale; an already-corrected sale should be handled by the tool that touched it,
	// not layered under a second, unrelated correction.
	if hasHistory, err := s.hasCorrectionHistory(ctx, tenantID, order.ID); err != nil {
		return nil, fmt.Errorf("check correction history: %w", err)
	} else if hasHistory {
		return nil, fmt.Errorf("this sale already has a return, refund, or reversal on record — use that tool instead of Delete")
	}

	fiscalized, invoiceID, err := s.isFiscalized(ctx, req.TenantSlug, order.ID)
	if err != nil {
		return nil, fmt.Errorf("check fiscal status: %w", err)
	}

	if fiscalized {
		return s.deleteFiscalized(ctx, tenantID, order, req, invoiceID)
	}
	return s.deleteNonFiscalized(ctx, tenantID, order, req)
}

// Result is what the handler returns to the caller.
type Result struct {
	Branch  string                        `json:"branch"` // "fiscalized" | "non_fiscalized"
	OrderID uuid.UUID                     `json:"order_id"`
	Steps   []entschema.ReversalStepJSON  `json:"steps"`
	ShredID *uuid.UUID                    `json:"shred_id,omitempty"`
}

// isFiscalized matches the exact criterion the reversals engine already uses for its eTIMS
// credit-note step: a treasury invoice exists for this order under reference_type "pos_order".
func (s *Service) isFiscalized(ctx context.Context, tenantSlug string, orderID uuid.UUID) (bool, string, error) {
	if s.treasuryClient == nil {
		return false, "", nil
	}
	inv, err := s.treasuryClient.GetInvoiceByReference(ctx, tenantSlug, "pos_order", orderID.String())
	if err != nil || inv == nil || inv.ID == "" {
		return false, "", nil
	}
	return true, inv.ID, nil
}

func (s *Service) hasCorrectionHistory(ctx context.Context, tenantID, orderID uuid.UUID) (bool, error) {
	if ok, err := s.client.POSReturn.Query().Where(entposreturn.TenantID(tenantID), entposreturn.OrderID(orderID)).Exist(ctx); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	// POSRefund has no tenant_id column of its own; order_id (a globally unique UUID already
	// scoped to tenantID by the caller's earlier order lookup) is sufficient here.
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

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func nowStep(step, service, status, detail, ref string) entschema.ReversalStepJSON {
	return entschema.ReversalStepJSON{
		Step: step, Service: service, Status: status, Detail: detail, Ref: ref,
		At: time.Now().UTC().Format(time.RFC3339),
	}
}

// snapshotJSON marshals the order + lines + payments into a generic map for the shred record —
// reused by the non-fiscalized branch (POSSaleShred.Snapshot) so the audit trail survives the
// order's physical deletion.
func snapshotOrder(order *ent.POSOrder, lines []*ent.POSOrderLine, payments []*ent.POSPayment) map[string]any {
	toMap := func(v any) map[string]any {
		b, _ := json.Marshal(v)
		m := map[string]any{}
		_ = json.Unmarshal(b, &m)
		return m
	}
	linesArr := make([]map[string]any, len(lines))
	for i, l := range lines {
		linesArr[i] = toMap(l)
	}
	paymentsArr := make([]map[string]any, len(payments))
	for i, p := range payments {
		paymentsArr[i] = toMap(p)
	}
	return map[string]any{
		"order":    toMap(order),
		"lines":    linesArr,
		"payments": paymentsArr,
	}
}
