package payments

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/modules/treasury"
	"github.com/bengobox/pos-service/internal/payref"
)

// TreasuryIntentReconciler periodically finds completed cash/manual POSPayment rows with no
// treasury intent recorded yet (external_reference still NULL) and retries the create — closing
// the window where dispatchTreasuryIntent's background goroutine is lost to a process
// restart/panic before it completes. A 2-minute ticker (tighter than SaleFinalizedReconciler's
// 5-minute one, since this guards money reconciliation and each retry is cheap and idempotent —
// treasury's CreateIntent dedups on the deterministic reference_id from payref.Build).
type TreasuryIntentReconciler struct {
	svc *Service
	log *zap.Logger
}

// NewTreasuryIntentReconciler creates a reconciler bound to the payments service, so it can reuse
// the exact same runTreasuryIntentCreate path the request-time dispatch calls.
func NewTreasuryIntentReconciler(svc *Service, log *zap.Logger) *TreasuryIntentReconciler {
	return &TreasuryIntentReconciler{svc: svc, log: log.Named("payments.treasury_intent_reconciler")}
}

// Start runs the reconciler on a 2-minute ticker.
func (r *TreasuryIntentReconciler) Start(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	r.log.Info("treasury intent reconciler started")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.runOnce(ctx); err != nil {
				r.log.Error("reconciler run failed", zap.Error(err))
			}
		}
	}
}

func (r *TreasuryIntentReconciler) runOnce(ctx context.Context) error {
	now := time.Now()
	// Floor of 2 minutes skips payments still inside the normal async dispatch window so the
	// reconciler never races a legitimate in-flight create. Ceiling of 24h bounds the scan.
	windowStart := now.Add(-24 * time.Hour)
	windowEnd := now.Add(-2 * time.Minute)

	pending, err := r.svc.client.POSPayment.Query().
		Where(
			pospayment.Status(StatusCompleted),
			pospayment.ExternalReferenceIsNil(),
			pospayment.OccurredAtGTE(windowStart),
			pospayment.OccurredAtLT(windowEnd),
		).
		All(ctx)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	for _, payment := range pending {
		method, _ := payment.PaymentData["method"].(string)
		if !isCashMethod(method) {
			// on_account/complimentary/customer_credit payments never get a treasury intent —
			// they always stamp external_reference at creation, so this shouldn't match, but skip
			// defensively rather than misfiring a CreateIntent call for a tender that never wants one.
			continue
		}

		order, oerr := r.svc.client.POSOrder.Get(ctx, payment.OrderID)
		if oerr != nil {
			r.log.Warn("reconciler: order not found for pending intent payment",
				zap.String("payment_id", payment.ID.String()), zap.Error(oerr))
			continue
		}
		outlet, outErr := r.svc.client.Outlet.Get(ctx, order.OutletID)
		if outErr != nil || outlet.TenantSlug == "" {
			r.log.Warn("reconciler: outlet/tenant slug not resolved for pending intent payment",
				zap.String("payment_id", payment.ID.String()), zap.Error(outErr))
			continue
		}

		intentReq := treasury.CreateIntentRequest{
			SourceService: "pos",
			ReferenceID:   payref.Build("POS", outlet.TenantSlug, order.TenantID, order.ID),
			ReferenceType: "pos_order",
			Amount:        payment.Amount,
			Currency:      payment.Currency,
			PaymentMethod: treasuryMethodForImmediate(method),
			Description:   fmt.Sprintf("POS order %s", order.OrderNumber),
			OutletID:      order.OutletID.String(),
			Metadata:      map[string]any{"service": "pos", "entity_id": order.ID.String()},
		}
		r.log.Info("retrying deferred treasury intent create",
			zap.String("order_id", order.ID.String()), zap.String("payment_id", payment.ID.String()))
		r.svc.runTreasuryIntentCreate(ctx, payment.ID, outlet.TenantSlug, order.ID, intentReq)
	}
	return nil
}
