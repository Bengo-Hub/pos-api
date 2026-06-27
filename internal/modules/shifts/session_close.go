package shifts

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/tender"
)

// ErrSessionAlreadyClosed is returned by CloseSession when the conditional
// (status == "open") update affected zero rows — i.e. another replica/worker
// already closed this session. Callers should treat it as a no-op.
var ErrSessionAlreadyClosed = errors.New("session already closed")

// ComputeExpectedCash calculates opening_float + total completed cash-tender
// payments for orders during this session window on this device. This is the
// single source of truth shared by the manual close handler and the auto-end
// worker — do NOT duplicate this logic elsewhere.
func ComputeExpectedCash(ctx context.Context, client *ent.Client, tid uuid.UUID, session *ent.POSDeviceSession) (float64, error) {
	// Get all completed orders for this device since session opened.
	orders, err := client.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.DeviceID(session.DeviceID),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(session.OpenedAt),
		).
		WithPayments().
		All(ctx)
	if err != nil {
		return 0, err
	}

	// Load cash tenders for the tenant.
	cashTenders, err := client.Tender.Query().
		Where(tender.TenantID(tid), tender.TypeEQ("cash")).
		All(ctx)
	if err != nil {
		return 0, err
	}
	cashTenderIDs := make(map[uuid.UUID]bool, len(cashTenders))
	for _, t := range cashTenders {
		cashTenderIDs[t.ID] = true
	}

	cashTotal := session.FloatAmount
	for _, o := range orders {
		for _, p := range o.Edges.Payments {
			if p.Status == "completed" && cashTenderIDs[p.TenderID] {
				cashTotal += p.Amount
			}
		}
	}
	return cashTotal, nil
}

// CloseOptions controls how a register session is closed. Shared by the manual
// CloseSession handler (Auto=false, ClosingFloat set) and the auto-end worker
// (Auto=true, ClosingFloat nil).
type CloseOptions struct {
	// ClosingFloat, when non-nil, records the counted cash and computes variance
	// (= *ClosingFloat - expectedCash). Nil for auto-closes where no count exists.
	ClosingFloat *float64
	// Notes is recorded on the session when non-empty.
	Notes string
	// Metadata keys are merged into the session's existing metadata (existing
	// keys preserved unless overwritten).
	Metadata map[string]any
	// Auto marks this as a worker-driven auto-close (vs a manual user close).
	Auto bool
}

// CloseSession reconciles and closes an open register session. It computes
// expected cash via ComputeExpectedCash, then performs a RACE-SAFE conditional
// update that only transitions rows still in status "open" — so two HA replicas
// (the worker runs on every pod) can never double-close the same session.
//
// Returns the updated session and the computed expected cash. If the session
// was already closed by another replica, returns ErrSessionAlreadyClosed and the
// caller should skip (no-op).
func CloseSession(ctx context.Context, client *ent.Client, session *ent.POSDeviceSession, opts CloseOptions) (*ent.POSDeviceSession, float64, error) {
	tid := session.TenantID

	expectedCash, err := ComputeExpectedCash(ctx, client, tid, session)
	if err != nil {
		// Reconciliation is best-effort; fall back to the opening float so the
		// close still proceeds (mirrors the prior handler behaviour).
		expectedCash = session.FloatAmount
	}

	now := time.Now()

	// Merge metadata: preserve existing keys, add/overwrite with the given ones.
	merged := map[string]any{}
	for k, v := range session.Metadata {
		merged[k] = v
	}
	for k, v := range opts.Metadata {
		merged[k] = v
	}

	// RACE-SAFE conditional close: only affect rows still "open". The Where on
	// SessionStatus("open") means a second replica's update affects 0 rows.
	upd := client.POSDeviceSession.Update().
		Where(
			posdevicesession.ID(session.ID),
			posdevicesession.SessionStatus("open"),
		).
		SetSessionStatus("closed").
		SetClosedAt(now).
		SetMetadata(merged)

	if opts.ClosingFloat != nil {
		variance := *opts.ClosingFloat - expectedCash
		upd = upd.SetClosingFloat(*opts.ClosingFloat).SetVariance(variance)
	}
	if opts.Notes != "" {
		upd = upd.SetNotes(opts.Notes)
	}

	affected, err := upd.Save(ctx)
	if err != nil {
		return nil, expectedCash, err
	}
	if affected == 0 {
		// Another replica/worker already closed it — no-op for this caller.
		return nil, expectedCash, ErrSessionAlreadyClosed
	}

	// Re-read the freshly-closed row to return the canonical state.
	updated, err := client.POSDeviceSession.Get(ctx, session.ID)
	if err != nil {
		return nil, expectedCash, err
	}
	return updated, expectedCash, nil
}
