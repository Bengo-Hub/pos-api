package shifts

import (
	"context"
	"errors"
	"time"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
	"github.com/bengobox/pos-service/internal/platform/clientinfo"
)

// ErrSessionAlreadyClosed is returned by CloseSession when the conditional
// (status == "open") update affected zero rows — i.e. another replica/worker
// already closed this session. Callers should treat it as a no-op.
var ErrSessionAlreadyClosed = errors.New("session already closed")

// ComputeExpectedCash is opening float + cash the shift user took - cash they refunded, over the
// shift window (see Summarize for the scope). It is the single source of truth shared by the
// manual close handler and the auto-end worker.
func ComputeExpectedCash(ctx context.Context, client *ent.Client, session *ent.POSDeviceSession) (float64, error) {
	sum, err := Summarize(ctx, client, session)
	if err != nil {
		return session.FloatAmount, err
	}
	return sum.ExpectedCash, nil
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
	// Reconciliation is best-effort: on a failure Summarize leaves ExpectedCash at zero, so fall
	// back to the opening float and let the close still proceed (mirrors the prior behaviour).
	sum, err := Summarize(ctx, client, session)
	expectedCash := sum.ExpectedCash
	if err != nil {
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
	// Freeze the cash-up breakdown at close so later edits to the shift's sales can't silently
	// change what the variance was computed from.
	merged["expected_cash"] = expectedCash
	if err == nil {
		merged["cash_taken"] = sum.CashTaken
		merged["cash_refunds"] = sum.CashRefunds
	}
	if info, ok := clientinfo.FromContext(ctx); ok {
		merged[audit.KeyClosedFrom] = info.Map()
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
