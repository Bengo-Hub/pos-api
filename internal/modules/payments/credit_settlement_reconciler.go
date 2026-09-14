package payments

import (
	"context"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// CreditSettlementSyncReconciler periodically retries a credit-settlement's treasury AR receipt
// when the original attempt (SettleCreditPayment, credit_settlement.go) failed — the PRIMARY
// defense against "payment on a sale doesn't reflect on treasury" (see
// [[boi-mixed-edit-reconcile-race-2026-09-10]] / [[boi-treasury-payment-reflection-gap-audit-2026-09-09]]
// for two live incidents this exact gap caused): a real, collected payment recorded correctly in
// POS but never synced to treasury, with the only trace being a log line and an easy-to-miss
// toast (TreasurySynced:false). Confirmed live 2026-09-11 (boi-enterprises, MRS MERCY BUSIA): two
// real payments (38,600 cash + 20,000 bank) failed to sync within the same minute — almost
// certainly a brief treasury outage — and sat unreflected for two days until found by hand.
//
// This closes the gap the RIGHT way — retry the specific failed operation shortly after, the same
// way TreasuryIntentReconciler (intent_reconciler.go) and SaleFinalizedReconciler (reconciler.go)
// already retry their own async follow-ups — rather than relying solely on ARDriftAuditScheduler
// (ar_drift_audit.go), which is a periodic, comparatively expensive, LAST-RESORT fleet-wide safety
// net for whatever this and every other targeted retry mechanism still misses. A failure here
// self-heals within minutes instead of surfacing only on the next daily audit pass.
//
// Safe to retry blindly: RecordARPayment gained an idempotency guard for exactly this S2S path
// (treasury-api, arpa/balances.go) alongside this reconciler shipping, so a retry against a call
// that actually succeeded server-side (response merely lost in transit) is a no-op, not a
// double-credit.
type CreditSettlementSyncReconciler struct {
	svc *Service
	log *zap.Logger
}

// NewCreditSettlementSyncReconciler creates a reconciler bound to the payments service, so it can
// reuse the exact same syncCreditSettlementReceipt path SettleCreditPayment calls.
func NewCreditSettlementSyncReconciler(svc *Service, log *zap.Logger) *CreditSettlementSyncReconciler {
	return &CreditSettlementSyncReconciler{svc: svc, log: log.Named("payments.credit_settlement_reconciler")}
}

// Start runs the reconciler on a 2-minute ticker — matches TreasuryIntentReconciler's own cadence
// (this guards money reconciliation too, and each retry is cheap and now idempotent).
func (r *CreditSettlementSyncReconciler) Start(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	r.log.Info("credit settlement sync reconciler started")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

func (r *CreditSettlementSyncReconciler) runOnce(ctx context.Context) {
	if r.svc == nil || r.svc.treasuryClient == nil {
		return
	}
	now := time.Now()

	// Both JSON-path filters pushed into SQL (never load-then-filter-in-Go here): credit_settlement
	// payments missing a treasury_receipt_id are already a small, SELF-BOUNDING slice of all
	// POSPayment rows — every row that succeeds gets its receipt id stashed and drops out of this
	// set for good, so it never grows the way an unfiltered fleet-wide scan would (see
	// ar_drift_audit.go's run() doc comment for the same principle applied to a bigger scan).
	//
	// Deliberately NOT filtered by occurred_at in SQL — see the in-Go in-flight check below for
	// why: occurred_at is the payment's BUSINESS date, not when the row was actually written, and
	// backdating (recording a payment today for money received days/weeks ago) is a first-class,
	// heavily-used feature throughout this codebase (payments/service.go, credit_settlement.go,
	// etc.). A payment recorded THIS MINUTE with occurred_at backdated a week+ earlier used to be
	// silently invisible to this reconciler forever (it never fell inside any occurred_at window
	// that also satisfied "at least 2 minutes old") — confirmed live 2026-09-14, boi-enterprises
	// order 000803 (MR ALBERT INLAW MALABA): a 10,610 payment recorded today, backdated to
	// occurred_at=2026-09-02, sat unsynced with zero retries because 2026-09-02 was already
	// outside the reconciler's then-existing 7-day occurred_at lookback the very first time it ran.
	candidates, err := r.svc.client.POSPayment.Query().
		Where(
			pospayment.Status(StatusCompleted),
			predicate.POSPayment(func(sel *sql.Selector) {
				sel.Where(sqljson.ValueEQ(pospayment.FieldPaymentData, true, sqljson.Path("credit_settlement")))
			}),
			predicate.POSPayment(func(sel *sql.Selector) {
				sel.Where(sql.Not(sqljson.HasKey(pospayment.FieldPaymentData, sqljson.Path("treasury_receipt_id"))))
			}),
		).
		All(ctx)
	if err != nil {
		r.log.Error("credit settlement reconciler: load candidates failed", zap.Error(err))
		return
	}
	if len(candidates) == 0 {
		return
	}

	var retried, healed int
	for _, payment := range candidates {
		// In-flight guard: skip a row whose real WRITE time (not its possibly-backdated
		// occurred_at) is under 2 minutes old, so this never races the normal request-time sync
		// attempt. recordedAt() falls back to occurred_at for a non-backdated payment, where the
		// two are the same thing anyway.
		if now.Sub(recordedAt(payment)) < 2*time.Minute {
			continue
		}
		order, oerr := r.svc.client.POSOrder.Get(ctx, payment.OrderID)
		if oerr != nil {
			r.log.Warn("credit settlement reconciler: order not found for pending settlement",
				zap.String("payment_id", payment.ID.String()), zap.Error(oerr))
			continue
		}
		outlet, outErr := r.svc.client.Outlet.Get(ctx, order.OutletID)
		if outErr != nil || outlet.TenantSlug == "" {
			r.log.Warn("credit settlement reconciler: outlet/tenant slug not resolved",
				zap.String("payment_id", payment.ID.String()), zap.Error(outErr))
			continue
		}
		method, _ := payment.PaymentData["method"].(string)
		retried++
		synced, _ := r.svc.syncCreditSettlementReceipt(ctx, order.TenantID, outlet.TenantSlug, order,
			payment.ID, payment.PaymentData, method, payment.Amount, payment.OccurredAt, "")
		if synced {
			healed++
			r.log.Info("credit settlement reconciler: retry succeeded",
				zap.String("order", order.OrderNumber), zap.String("payment_id", payment.ID.String()))
		}
	}
	if retried > 0 {
		r.log.Info("credit settlement reconciler pass complete",
			zap.Int("retried", retried), zap.Int("healed", healed))
	}
}

// recordedAt returns the moment a payment row was actually WRITTEN — payment_data["recorded_at"]
// (stamped by credit_settlement.go whenever the caller backdates OccurredAt, i.e.
// payment_data["backdated"]==true) when present, else OccurredAt itself (a non-backdated payment
// has no separate recorded_at because the two are identical). POSPayment has no dedicated
// created_at column to read this from directly.
func recordedAt(p *ent.POSPayment) time.Time {
	if s, ok := p.PaymentData["recorded_at"].(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return p.OccurredAt
}
