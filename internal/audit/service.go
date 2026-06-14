// Package audit provides a centralized, append-only trail of sensitive /
// fraud-relevant POS actions (voids, line removals, discount/price overrides,
// refunds, cash-drawer movements, role changes).
//
// Record is best-effort but durable: it writes synchronously and logs (rather
// than returns) failures so a failed audit write never breaks the primary
// business operation.
package audit

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
)

// Entry is a single audit record to persist.
type Entry struct {
	TenantID     uuid.UUID
	OutletID     *uuid.UUID
	ActorUserID  uuid.UUID
	ActorStaffID *uuid.UUID
	ApproverID   *uuid.UUID
	Action       string
	EntityType   string
	EntityID     string
	Reason       string
	Before       map[string]any
	After        map[string]any
	Amount       *float64
}

// Service writes audit entries.
type Service struct {
	client *ent.Client
	log    *zap.Logger
}

// NewService constructs an audit service.
func NewService(client *ent.Client, log *zap.Logger) *Service {
	return &Service{client: client, log: log.Named("audit")}
}

// Record persists an audit entry. Failures are logged, never returned.
func (s *Service) Record(ctx context.Context, e Entry) {
	if s == nil || s.client == nil {
		return
	}
	b := s.client.AuditLog.Create().
		SetTenantID(e.TenantID).
		SetActorUserID(e.ActorUserID).
		SetAction(e.Action)
	if e.OutletID != nil {
		b = b.SetOutletID(*e.OutletID)
	}
	if e.ActorStaffID != nil {
		b = b.SetActorStaffID(*e.ActorStaffID)
	}
	if e.ApproverID != nil {
		b = b.SetApproverUserID(*e.ApproverID)
	}
	if e.EntityType != "" {
		b = b.SetEntityType(e.EntityType)
	}
	if e.EntityID != "" {
		b = b.SetEntityID(e.EntityID)
	}
	if e.Reason != "" {
		b = b.SetReason(e.Reason)
	}
	if e.Before != nil {
		b = b.SetBeforeJSON(e.Before)
	}
	if e.After != nil {
		b = b.SetAfterJSON(e.After)
	}
	if e.Amount != nil {
		b = b.SetAmount(*e.Amount)
	}
	if _, err := b.Save(ctx); err != nil {
		s.log.Warn("failed to record audit entry",
			zap.String("action", e.Action),
			zap.String("entity_type", e.EntityType),
			zap.Error(err))
	}
}
