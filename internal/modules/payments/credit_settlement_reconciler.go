package payments

import (
	"context"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"go.uber.org/zap"

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
	// Floor of 2 minutes skips settlements still inside the normal request-time sync attempt so
	// the reconciler never races a legitimate in-flight call. Ceiling of 7 days bounds the scan —
	// anything still unsynced after a week needs the ARDriftAuditScheduler safety net (or a human)
	// rather than an ever-growing per-run candidate list.
	windowStart := now.Add(-7 * 24 * time.Hour)
	windowEnd := now.Add(-2 * time.Minute)

	// Both JSON-path filters pushed into SQL (never load-then-filter-in-Go here): credit_settlement
	// payments are already a small slice of all POSPayment rows, but there's no reason to load
	// the rest just to discard them, and a fleet-wide table only grows over time — see
	// ar_drift_audit.go's run() doc comment for the same principle applied to a bigger scan.
	candidates, err := r.svc.client.POSPayment.Query().
		Where(
			pospayment.Status(StatusCompleted),
			pospayment.OccurredAtGTE(windowStart),
			pospayment.OccurredAtLT(windowEnd),
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
