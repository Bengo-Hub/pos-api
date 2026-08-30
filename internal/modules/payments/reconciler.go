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

// saleFinalizedPublishedAtKey is the POSOrder.Metadata key stamped by
// stampSaleFinalizedPublished the moment a pos.sale.finalized outbox write is successfully
// enqueued. It is the reconciler's "already handled" signal — NOT a lookup against the
// outbox_events table, because a successfully published row is DELETED within seconds
// (shared-events' prune-on-publish, see the outbox-retry-retention-model), which used to make
// every completed order look "never published" again and again, every 5-minute tick, for up
// to 24h. A row that persists in outbox_events (status FAILED) is still checked separately
// below — that class of row is NOT pruned, so it remains a reliable "this genuinely failed,
// retry it" signal even for an order this stamp already marked as enqueued.
const saleFinalizedPublishedAtKey = "sale_finalized_published_at"

// SaleFinalizedReconciler periodically finds completed orders that never got a
// pos.sale.finalized publish stamped, or whose stamped attempt is known to have failed at the
// gateway, and republishes them — closing the crash window between the completion UPDATE and
// the outbox insert (widened to ~30s once fan-out went async, see pos-sale-close-async-fanout.md)
// and picking up rows the outbox publisher already exhausted its retries on (shared-events
// marks a row FAILED after MaxOutboxAttempts and never retries it).
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

	// FAILED outbox rows are the one status that survives shared-events' prune-on-publish
	// (only a terminal FAILED row is kept, for troubleshooting, until the nightly 7-day
	// retention sweep) — so this query reliably finds genuine, still-unresolved failures.
	// It does NOT need to (and cannot) tell us about a row that already succeeded; that's
	// what the per-order metadata stamp below is for.
	failedEvents, err := r.svc.client.OutboxEvent.Query().
		Where(
			outboxevent.EventType("sale.finalized"),
			outboxevent.CreatedAtGTE(windowStart),
			outboxevent.Status("FAILED"),
		).
		All(ctx)
	if err != nil {
		return err
	}
	failedOrderIDs := make(map[string]bool, len(failedEvents))
	for _, e := range failedEvents {
		var p outboxPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.OrderID == "" {
			continue
		}
		failedOrderIDs[p.OrderID] = true
	}

	for _, order := range completedOrders {
		orderIDStr := order.ID.String()
		reason := ""
		switch {
		case failedOrderIDs[orderIDStr]:
			reason = "failed_exhausted"
		case order.Metadata[saleFinalizedPublishedAtKey] == nil:
			reason = "missing" // never even attempted (the request-time crash window)
		default:
			continue // already enqueued a publish and it isn't known to have failed — done
		}
		r.log.Info("republishing sale.finalized for completed order",
			zap.String("order_id", orderIDStr),
			zap.String("reason", reason),
		)
		r.svc.publishSaleFinalized(ctx, order)
	}
	return nil
}
