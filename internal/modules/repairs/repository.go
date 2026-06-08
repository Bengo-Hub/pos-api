package repairs

import (
	"context"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/repairjob"
	"github.com/bengobox/pos-service/internal/ent/repairjobpart"
)

// ListFilter narrows a repair-job list query.
type ListFilter struct {
	TenantID uuid.UUID
	OutletID *uuid.UUID
	Status   string
	Limit    int
	Offset   int
}

// list returns repair jobs matching the filter (newest first) plus the total count.
func (s *Service) list(ctx context.Context, f ListFilter) ([]*ent.RepairJob, int, error) {
	q := s.client.RepairJob.Query().Where(repairjob.TenantID(f.TenantID))
	if f.OutletID != nil {
		q = q.Where(repairjob.OutletID(*f.OutletID))
	}
	if f.Status != "" {
		q = q.Where(repairjob.StatusEQ(repairjob.Status(f.Status)))
	}

	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}

	jobs, err := q.
		Order(ent.Desc(repairjob.FieldCreatedAt)).
		Limit(f.Limit).
		Offset(f.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}
	return jobs, total, nil
}

// getOwned fetches a repair job scoped to a tenant, returning ErrNotFound when missing.
func (s *Service) getOwned(ctx context.Context, tenantID, jobID uuid.UUID) (*ent.RepairJob, error) {
	job, err := s.client.RepairJob.Get(ctx, jobID)
	if err != nil || job.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return job, nil
}

// listParts returns the part lines for a repair job (insertion order).
func (s *Service) listParts(ctx context.Context, jobID uuid.UUID) ([]*ent.RepairJobPart, error) {
	return s.client.RepairJobPart.Query().
		Where(repairjobpart.RepairJobID(jobID)).
		All(ctx)
}

// listEvents returns the timeline events for a repair job (newest first).
func (s *Service) listEvents(ctx context.Context, jobID uuid.UUID) ([]*ent.RepairJobEvent, error) {
	return s.client.RepairJob.Query().
		Where(repairjob.ID(jobID)).
		QueryEvents().
		All(ctx)
}
