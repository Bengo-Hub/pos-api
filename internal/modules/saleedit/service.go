// Package saleedit implements the tenant-admin "Edit Sale" tool for a FINALIZED sale — a
// centralized orchestrator (see orchestrator.go, Service.Edit) that diffs the caller's full
// desired line set against the live order and branches per-line by the order's actual
// fiscalization status:
//   - non-fiscalized: every bucket (add/reduce/increase/remove) is a TRUE in-place edit of
//     the SAME order — no new order, no new receipt, no new transaction record.
//   - fiscalized: reductions become a real auto-completed POSReturn + credit note
//     (returns.Service.CreateAndAutoComplete); increases keep the existing linked-addendum-
//     order pattern (a new order with its own docket, via orders.Service.CreateOrder).
package saleedit

import (
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/returns"
	"github.com/bengobox/pos-service/internal/modules/reversals"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// finalizedStatuses mirrors reversals.finalizedStatuses / saledelete.finalizedStatuses — only a
// settled sale can be edited.
var finalizedStatuses = map[string]bool{"completed": true, "paid": true, "closed": true}

// Service orchestrates the Edit-Sale tool.
type Service struct {
	log         *zap.Logger
	client      *ent.Client
	reversalSvc *reversals.Service
	auditSvc    *audit.Service
	// treasuryClient, inventoryClient, orderSvc and returnsSvc are wired via their Set*
	// methods (orchestrator.go) — the centralized Edit orchestrator needs all of them.
	treasuryClient  *treasury.Client
	inventoryClient *inventory.Client
	orderSvc        *orders.Service
	returnsSvc      *returns.Service
}

// NewService wires the Edit-Sale orchestrator.
func NewService(log *zap.Logger, client *ent.Client, reversalSvc *reversals.Service) *Service {
	return &Service{log: log.Named("saleedit"), client: client, reversalSvc: reversalSvc}
}

// SetAuditService wires the centralized audit trail.
func (s *Service) SetAuditService(a *audit.Service) { s.auditSvc = a }
