package events

import (
	"context"
	"database/sql"
	"fmt"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Publisher handles publishing POS events via the transactional outbox pattern.
type Publisher struct {
	repo   eventslib.OutboxRepository
	logger *zap.Logger
}

// NewPublisher creates a new POS event publisher backed by the shared-events outbox.
func NewPublisher(sqlDB *sql.DB, logger *zap.Logger) *Publisher {
	return &Publisher{
		repo:   eventslib.NewSQLOutboxRepository(sqlDB),
		logger: logger.Named("pos.events"),
	}
}

// OutboxRepo returns the outbox repository for use by the background publisher.
func (p *Publisher) OutboxRepo() eventslib.OutboxRepository {
	return p.repo
}

// publish writes an event to the outbox for background publishing to NATS.
func (p *Publisher) publish(ctx context.Context, tenantID uuid.UUID, eventType string, data map[string]any) error {
	if p == nil {
		return nil
	}

	event := eventslib.NewEvent(eventType, "pos", uuid.New(), tenantID, data)

	tx, err := p.repo.BeginTx(ctx)
	if err != nil {
		p.logger.Error("failed to begin tx for event", zap.String("event_type", eventType), zap.Error(err))
		return fmt.Errorf("begin tx: %w", err)
	}

	if err := eventslib.CreateOutboxRecordInTx(ctx, tx, p.repo, event); err != nil {
		_ = tx.Rollback()
		p.logger.Error("failed to write event to outbox", zap.String("event_type", eventType), zap.Error(err))
		return fmt.Errorf("write outbox: %w", err)
	}

	if err := tx.Commit(); err != nil {
		p.logger.Error("failed to commit event", zap.String("event_type", eventType), zap.Error(err))
		return fmt.Errorf("commit: %w", err)
	}

	p.logger.Debug("event written to outbox", zap.String("event_type", eventType))
	return nil
}

// PublishOrderCreated publishes a pos.order.created event.
func (p *Publisher) PublishOrderCreated(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "order.created", data)
}

// PublishOrderStatusChanged publishes a pos.order.status_changed event.
func (p *Publisher) PublishOrderStatusChanged(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "order.status_changed", data)
}

// PublishPaymentRecorded publishes a pos.payment.recorded event.
func (p *Publisher) PublishPaymentRecorded(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "payment.recorded", data)
}

// PublishSaleFinalized publishes a pos.sale.finalized event consumed by treasury-api for ledger posting.
func (p *Publisher) PublishSaleFinalized(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "sale.finalized", data)
}

// PublishDrawerClosed publishes a pos.drawer.closed event consumed by treasury-api for cash position ledger.
func (p *Publisher) PublishDrawerClosed(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "drawer.closed", data)
}

// PublishInventoryConsumptionFailed publishes a pos.inventory.consumption.failed event.
// Consumed by a retry worker to re-attempt the inventory backflush.
func (p *Publisher) PublishInventoryConsumptionFailed(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "inventory.consumption.failed", data)
}

// PublishStockAlert publishes a pos.alert.stock_low event when inventory-api notifies of low stock.
func (p *Publisher) PublishStockAlert(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "alert.stock_low", data)
}

// PublishReturnInitiated publishes a pos.return.initiated event (audit trail).
func (p *Publisher) PublishReturnInitiated(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "return.initiated", data)
}

// PublishReturnCompleted publishes a pos.return.completed event.
// Consumed by inventory-api to restock items and treasury-api to process refund.
func (p *Publisher) PublishReturnCompleted(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "return.completed", data)
}

// PublishExchangeCompleted publishes a pos.exchange.completed event.
func (p *Publisher) PublishExchangeCompleted(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "exchange.completed", data)
}

// PublishKDSOrderUpdated publishes a pos.kds.order_updated event for real-time KDS UI refresh.
func (p *Publisher) PublishKDSOrderUpdated(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "kds.order_updated", data)
}

// PublishKDSWaiterCalled publishes a pos.kds.waiter.called event when kitchen/bar calls the waiter.
func (p *Publisher) PublishKDSWaiterCalled(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "kds.waiter.called", data)
}

// PublishKDSOrderReady publishes a pos.kds.order.ready event when a kitchen ticket is marked ready.
func (p *Publisher) PublishKDSOrderReady(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "kds.order.ready", data)
}

// PublishERPSalePosted publishes a pos.erp.sale_posted event for external ERP / accounting system sync.
func (p *Publisher) PublishERPSalePosted(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "erp.sale_posted", data)
}

// PublishHotelCheckIn publishes a pos.hotel.check_in event (treasury-api folio ledger, CRM audit).
func (p *Publisher) PublishHotelCheckIn(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "hotel.check_in", data)
}

// PublishHotelCheckOut publishes a pos.hotel.check_out event (treasury-api settlement, housekeeping).
func (p *Publisher) PublishHotelCheckOut(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "hotel.check_out", data)
}

// PublishHotelFolioCharge publishes a pos.hotel.folio_charge event when a charge is posted to a room folio.
func (p *Publisher) PublishHotelFolioCharge(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "hotel.folio_charge", data)
}

// PublishHotelBookingCreated publishes hotel.booking.created for a multi-room/group booking.
func (p *Publisher) PublishHotelBookingCreated(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "hotel.booking.created", data)
}

// PublishConferenceEventBooked publishes conference.event.booked when a BEO/event booking is created.
func (p *Publisher) PublishConferenceEventBooked(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "conference.event.booked", data)
}

// PublishConferenceMealcardIssued publishes conference.mealcard.issued when delegate meal cards are generated.
func (p *Publisher) PublishConferenceMealcardIssued(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "conference.mealcard.issued", data)
}

// PublishConferenceMealcardRedeemed publishes conference.mealcard.redeemed when a meal voucher is redeemed
// (consumed by inventory-api for meal-BOM backflush and notifications-api).
func (p *Publisher) PublishConferenceMealcardRedeemed(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "conference.mealcard.redeemed", data)
}

// PublishLoyaltyPointsEarned publishes a pos.loyalty.points.earned event.
// Consumed by notifications-service to send a WhatsApp/SMS "You earned X pts" message.
func (p *Publisher) PublishLoyaltyPointsEarned(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "loyalty.points.earned", data)
}

// PublishLoyaltyTierUpgraded publishes a pos.loyalty.tier_upgraded event when a customer reaches a new loyalty tier.
// Consumed by marketflow-api to update the CRM contact's loyalty metadata.
func (p *Publisher) PublishLoyaltyTierUpgraded(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "loyalty.tier_upgraded", data)
}

// PublishLayawayPaymentDue publishes a pos.layaway.payment_due event.
// Consumed by notifications-service to send a WhatsApp/SMS reminder to the customer.
func (p *Publisher) PublishLayawayPaymentDue(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "layaway.payment_due", data)
}

// PublishDeviceRegistered publishes a pos.device.registered event.
// Consumed by subscriptions-api to track max_devices plan limit usage.
func (p *Publisher) PublishDeviceRegistered(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "device.registered", data)
}

// PublishTableCreated publishes a pos.table.created event.
// Consumed by subscriptions-api to track max_tables plan limit usage.
func (p *Publisher) PublishTableCreated(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "table.created", data)
}
