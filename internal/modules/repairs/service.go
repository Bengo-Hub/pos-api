package repairs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/repairjob"
	"github.com/bengobox/pos-service/internal/ent/repairjobpart"
)

// Sentinel errors returned by the service.
var (
	ErrNotFound        = errors.New("repair job not found")
	ErrPartNotFound    = errors.New("repair job part not found")
	ErrInvalidStatus   = errors.New("invalid repair job status")
	ErrAlreadySettled  = errors.New("repair job already settled")
	ErrTerminalStatus  = errors.New("repair job is in a terminal status")
)

// Service provides repair / job-card business logic.
type Service struct {
	client *ent.Client
	log    *zap.Logger
}

// NewService creates a new repairs service.
func NewService(client *ent.Client, log *zap.Logger) *Service {
	return &Service{
		client: client,
		log:    log.Named("repairs.service"),
	}
}

// List returns repair jobs matching the filter plus the total count.
func (s *Service) List(ctx context.Context, f ListFilter) ([]*ent.RepairJob, int, error) {
	return s.list(ctx, f)
}

// Get returns a tenant-scoped repair job.
func (s *Service) Get(ctx context.Context, tenantID, jobID uuid.UUID) (*ent.RepairJob, error) {
	return s.getOwned(ctx, tenantID, jobID)
}

// Parts returns the part lines for a tenant-scoped repair job.
func (s *Service) Parts(ctx context.Context, tenantID, jobID uuid.UUID) ([]*ent.RepairJobPart, error) {
	if _, err := s.getOwned(ctx, tenantID, jobID); err != nil {
		return nil, err
	}
	return s.listParts(ctx, jobID)
}

// Events returns the timeline events for a tenant-scoped repair job.
func (s *Service) Events(ctx context.Context, tenantID, jobID uuid.UUID) ([]*ent.RepairJobEvent, error) {
	if _, err := s.getOwned(ctx, tenantID, jobID); err != nil {
		return nil, err
	}
	return s.listEvents(ctx, jobID)
}

// generateJobNumber creates a unique, human-readable job-card number.
func (s *Service) generateJobNumber() string {
	return fmt.Sprintf("JOB-%d", time.Now().UnixMilli())
}

// Create opens a new repair job (intake) and records an intake event.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, in CreateInput) (*ent.RepairJob, error) {
	creator := s.client.RepairJob.Create().
		SetTenantID(tenantID).
		SetJobNumber(s.generateJobNumber()).
		SetCustomerPhone(in.CustomerPhone).
		SetCustomerName(in.CustomerName).
		SetDeviceDescription(in.DeviceDescription).
		SetReportedIssue(in.ReportedIssue).
		SetEstimatedCost(in.EstimatedCost).
		SetStatus(repairjob.StatusIntake)

	if in.OutletID != nil {
		creator = creator.SetOutletID(*in.OutletID)
	}
	if in.AssignedStaffID != nil {
		creator = creator.SetAssignedStaffID(*in.AssignedStaffID)
	}

	job, err := creator.Save(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.appendEvent(ctx, job.ID, EventIntake, "Job created at intake", in.ActorID); err != nil {
		s.log.Warn("failed to append intake event", zap.Error(err))
	}
	return job, nil
}

// Update applies status / diagnosis / assignment changes and records events.
func (s *Service) Update(ctx context.Context, tenantID, jobID uuid.UUID, in UpdateInput) (*ent.RepairJob, error) {
	job, err := s.getOwned(ctx, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	if job.PosOrderID != nil {
		return nil, ErrAlreadySettled
	}

	upd := s.client.RepairJob.UpdateOneID(jobID)
	statusChanged := false

	if in.Status != nil && *in.Status != "" {
		if !IsValidStatus(*in.Status) {
			return nil, ErrInvalidStatus
		}
		if *in.Status != job.Status.String() {
			upd = upd.SetStatus(repairjob.Status(*in.Status))
			statusChanged = true
		}
	}
	if in.Diagnosis != nil {
		upd = upd.SetDiagnosis(*in.Diagnosis)
	}
	if in.QuotedCost != nil {
		upd = upd.SetQuotedCost(*in.QuotedCost)
	}
	if in.AssignedStaffID != nil {
		upd = upd.SetAssignedStaffID(*in.AssignedStaffID)
	}

	updated, err := upd.Save(ctx)
	if err != nil {
		return nil, err
	}

	// Record a diagnosis event when a diagnosis is supplied, otherwise a status_change/note.
	switch {
	case in.Diagnosis != nil && *in.Diagnosis != "":
		_ = s.appendEvent(ctx, jobID, EventDiagnosis, *in.Diagnosis, in.ActorID)
	case statusChanged:
		_ = s.appendEvent(ctx, jobID, EventStatusChange,
			fmt.Sprintf("Status changed to %s", *in.Status), in.ActorID)
	case in.Note != "":
		_ = s.appendEvent(ctx, jobID, EventNote, in.Note, in.ActorID)
	}
	return updated, nil
}

// AddPart adds a part line, recomputes the line total, and records an event.
func (s *Service) AddPart(ctx context.Context, tenantID, jobID uuid.UUID, in AddPartInput) (*ent.RepairJobPart, error) {
	if _, err := s.getOwned(ctx, tenantID, jobID); err != nil {
		return nil, err
	}

	qty := in.Quantity
	if qty <= 0 {
		qty = 1
	}
	lineTotal := in.UnitCost.Mul(decimal.NewFromFloat(qty))

	creator := s.client.RepairJobPart.Create().
		SetRepairJobID(jobID).
		SetInventorySku(in.InventorySKU).
		SetDescription(in.Description).
		SetQuantity(qty).
		SetUnitCost(in.UnitCost).
		SetLineTotal(lineTotal)
	if in.InventoryItemID != nil {
		creator = creator.SetInventoryItemID(*in.InventoryItemID)
	}

	part, err := creator.Save(ctx)
	if err != nil {
		return nil, err
	}

	desc := in.Description
	if desc == "" {
		desc = in.InventorySKU
	}
	_ = s.appendEvent(ctx, jobID, EventPartsAdded,
		fmt.Sprintf("Added part %s x%g (%s)", desc, qty, lineTotal.String()), in.ActorID)

	// Touch the parent so updated_at reflects the change.
	_ = s.client.RepairJob.UpdateOneID(jobID).Exec(ctx)
	return part, nil
}

// RemovePart deletes a part line from a repair job and records an event.
func (s *Service) RemovePart(ctx context.Context, tenantID, jobID, partID uuid.UUID, actorID *uuid.UUID) error {
	if _, err := s.getOwned(ctx, tenantID, jobID); err != nil {
		return err
	}

	part, err := s.client.RepairJobPart.Query().
		Where(repairjobpart.ID(partID), repairjobpart.RepairJobID(jobID)).
		Only(ctx)
	if err != nil {
		return ErrPartNotFound
	}

	if err := s.client.RepairJobPart.DeleteOneID(part.ID).Exec(ctx); err != nil {
		return err
	}

	_ = s.appendEvent(ctx, jobID, EventPartsAdded,
		fmt.Sprintf("Removed part %s", part.Description), actorID)
	_ = s.client.RepairJob.UpdateOneID(jobID).Exec(ctx)
	return nil
}

// PartsTotal returns the sum of all part line totals for a job.
func (s *Service) PartsTotal(ctx context.Context, jobID uuid.UUID) (decimal.Decimal, error) {
	parts, err := s.listParts(ctx, jobID)
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, p := range parts {
		total = total.Add(p.LineTotal)
	}
	return total, nil
}

// Settle links a POS order to the job, marks it completed, and records a settled event.
func (s *Service) Settle(ctx context.Context, tenantID, jobID, posOrderID uuid.UUID, actorID *uuid.UUID) (*ent.RepairJob, error) {
	job, err := s.getOwned(ctx, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	if job.PosOrderID != nil {
		return nil, ErrAlreadySettled
	}
	if job.Status == repairjob.StatusCancelled {
		return nil, ErrTerminalStatus
	}

	updated, err := s.client.RepairJob.UpdateOneID(jobID).
		SetPosOrderID(posOrderID).
		SetStatus(repairjob.StatusCompleted).
		Save(ctx)
	if err != nil {
		return nil, err
	}

	_ = s.appendEvent(ctx, jobID, EventSettled,
		fmt.Sprintf("Settled via POS order %s", posOrderID), actorID)
	return updated, nil
}

// appendEvent records a timeline event for a repair job.
func (s *Service) appendEvent(ctx context.Context, jobID uuid.UUID, eventType, notes string, actorID *uuid.UUID) error {
	creator := s.client.RepairJobEvent.Create().
		SetRepairJobID(jobID).
		SetEventType(eventType).
		SetNotes(notes)
	if actorID != nil {
		creator = creator.SetActorID(*actorID)
	}
	_, err := creator.Save(ctx)
	return err
}
