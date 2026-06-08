// Package repairs provides the repair / job-card service layer for POS operations.
// It manages the lifecycle of a device repair job: intake, diagnosis, parts,
// status transitions (each recorded as a RepairJobEvent), and settlement via POS.
package repairs

import (
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Valid repair job statuses.
const (
	StatusIntake        = "intake"
	StatusDiagnosed     = "diagnosed"
	StatusAwaitingParts = "awaiting_parts"
	StatusInProgress    = "in_progress"
	StatusReady         = "ready"
	StatusCompleted     = "completed"
	StatusCancelled     = "cancelled"
)

// Event types appended to a job's timeline.
const (
	EventIntake       = "intake"
	EventDiagnosis    = "diagnosis"
	EventPartsAdded   = "parts_added"
	EventStatusChange = "status_change"
	EventNote         = "note"
	EventSettled      = "settled"
)

// CreateInput is the payload to open a new repair job (intake).
type CreateInput struct {
	OutletID          *uuid.UUID
	CustomerPhone     string
	CustomerName      string
	DeviceDescription string
	ReportedIssue     string
	EstimatedCost     decimal.Decimal
	AssignedStaffID   *uuid.UUID
	ActorID           *uuid.UUID
}

// UpdateInput is the payload to patch a repair job (status / diagnosis / assignment).
// Nil fields are left unchanged.
type UpdateInput struct {
	Status          *string
	Diagnosis       *string
	QuotedCost      *decimal.Decimal
	AssignedStaffID *uuid.UUID
	Note            string
	ActorID         *uuid.UUID
}

// AddPartInput is the payload to add a part line to a repair job.
type AddPartInput struct {
	InventorySKU    string
	InventoryItemID *uuid.UUID
	Description     string
	Quantity        float64
	UnitCost        decimal.Decimal
	ActorID         *uuid.UUID
}

// validStatuses is the set of statuses accepted on update.
var validStatuses = map[string]struct{}{
	StatusIntake:        {},
	StatusDiagnosed:     {},
	StatusAwaitingParts: {},
	StatusInProgress:    {},
	StatusReady:         {},
	StatusCompleted:     {},
	StatusCancelled:     {},
}

// IsValidStatus reports whether s is a recognised repair job status.
func IsValidStatus(s string) bool {
	_, ok := validStatuses[s]
	return ok
}
