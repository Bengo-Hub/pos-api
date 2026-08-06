package payments

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/outboxevent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// SaleFinalizedReconciler periodically finds completed orders with no successfully-published
// pos.sale.finalized outbox row and republishes them — closing the crash window between the
// completion UPDATE and the outbox insert (widened to ~30s once fan-out went async, see
// pos-sale-close-async-fanout.md) and picking up rows the outbox publisher already exhausted
// its retries on (shared-events marks a row FAILED after MaxOutboxAttempts and never retries it).
type SaleFinalizedReconciler struct {
	svc *Service
	log *zap.Logger
}

// NewSaleFinalizedReconciler creates a reconciler bound to the payments service, so it can
// reuse the exact same publishSaleFinalized path the request-time completion flow calls.
func NewSaleFinalizedReconciler(svc *Service, log *zap.Logger) *SaleFinalizedReconciler {
	return &SaleFinalizedReconciler{svc: svc, log: log.Named("payments.sale_finalized_reconciler")}
}

// Start runs the reconciler on a 5-minute ticker.
func (r *SaleFinalizedReconciler) Start(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	r.log.Info("sale.finalized reconciler started")
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

// outboxPayload is the minimal shape needed to correlate an OutboxEvent back to an order —
// AggregateID is a random UUID minted per-publish (see platform/events/publisher.go), NOT the
// order ID, so correlation must go through the JSON payload's order_id field instead.
type outboxPayload struct {
	OrderID string `json:"order_id"`
}

func (r *SaleFinalizedReconciler) runOnce(ctx context.Context) error {
	now := time.Now()
	// Floor of 2 minutes skips orders still inside the normal async fan-out window so the
	// reconciler never races a legitimate in-flight publish. Ceiling of 24h bounds the scan.
	windowStart := now.Add(-24 * time.Hour)
	windowEnd := now.Add(-2 * time.Minute)

	completedOrders, err := r.svc.client.POSOrder.Query().
		Where(
			posorder.Status(orders.StatusCompleted),
			posorder.UpdatedAtGTE(windowStart),
			posorder.UpdatedAtLT(windowEnd),
		).
		All(ctx)
	if err != nil {
		return err
	}
	if len(completedOrders) == 0 {
		return nil
	}

	events, err := r.svc.client.OutboxEvent.Query().
		Where(
			outboxevent.EventType("sale.finalized"),
			outboxevent.CreatedAtGTE(windowStart),
		).
		All(ctx)
	if err != nil {
		return err
	}

	// order_id -> latest known outbox status. PUBLISHED wins if both a FAILED and a
	// PUBLISHED row exist for the same order (a prior reconciler run already fixed it).
	statusByOrder := make(map[string]string, len(events))
	for _, e := range events {
		var p outboxPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.OrderID == "" {
			continue
		}
		if existing, ok := statusByOrder[p.OrderID]; ok && existing == "PUBLISHED" {
			continue
		}
		statusByOrder[p.OrderID] = e.Status
	}

	for _, order := range completedOrders {
		status, found := statusByOrder[order.ID.String()]
		if found && status != "FAILED" {
			continue // PENDING (outbox publisher will get to it) or PUBLISHED (already fine)
		}
		reason := "missing"
		if found {
			reason = "failed_exhausted"
		}
		r.log.Info("republishing sale.finalized for completed order with no live outbox row",
			zap.String("order_id", order.ID.String()),
			zap.String("reason", reason),
		)
		r.svc.publishSaleFinalized(ctx, order)
	}
	return nil
}
