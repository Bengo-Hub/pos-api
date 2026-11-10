package events

import (
	"context"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// nakDelay spaces out retries of a contact event whose apply failed.
const nakDelay = 30 * time.Second

// ContactSnapshots is what the subscriber needs from customers.Resolver.
type ContactSnapshots interface {
	RefreshSnapshots(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) error
	Unlink(ctx context.Context, tenantID, contactID uuid.UUID) error
}

// CRMContactUpdatedSubscriber keeps pos-api's customer snapshots in step with the CRM.
// crm.contact.updated reloads the contact and rewrites the name, phone and email kept on
// loyalty accounts, client records and room guests. crm.contact.deleted only clears the
// crm_contact_id; loyalty points and every other local record are kept.
type CRMContactUpdatedSubscriber struct {
	snaps ContactSnapshots
	log   *zap.Logger
}

// NewCRMContactUpdatedSubscriber builds the subscriber.
func NewCRMContactUpdatedSubscriber(snaps ContactSnapshots, log *zap.Logger) *CRMContactUpdatedSubscriber {
	return &CRMContactUpdatedSubscriber{snaps: snaps, log: log.Named("pos.crm_contact_updated")}
}

// Subscribe binds durable consumers for crm.contact.updated and crm.contact.deleted on the
// existing crm.> stream.
func (s *CRMContactUpdatedSubscriber) Subscribe(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("crm contact-updated subscriber: NATS connection is nil")
	}
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("crm contact-updated subscriber: jetstream init: %w", err)
	}
	stream, err := crmStreamFor(js, "crm.contact.updated")
	if err != nil {
		return fmt.Errorf("crm contact-updated subscriber: %w", err)
	}

	opts := func(durable string) []nats.SubOpt {
		return []nats.SubOpt{
			nats.Durable(durable),
			nats.AckExplicit(),
			nats.AckWait(30 * time.Second),
			nats.MaxDeliver(5),
			nats.DeliverNew(),
		}
	}
	sharedevents.SubscribeQueueWithRebind(s.log, js, stream, "crm.contact.updated", "pos-contact-updated",
		s.wrap(s.handleUpdated), opts("pos-contact-updated")...)
	sharedevents.SubscribeQueueWithRebind(s.log, js, stream, "crm.contact.deleted", "pos-contact-deleted",
		s.wrap(s.handleDeleted), opts("pos-contact-deleted")...)

	s.log.Info("crm.contact.updated and crm.contact.deleted subscriptions registered")
	return nil
}

// wrap decodes the envelope and acks or naks from the handler's result. A message that cannot
// be decoded is acked, since a retry cannot fix it.
func (s *CRMContactUpdatedSubscriber) wrap(h func(context.Context, *sharedevents.Event) error) nats.MsgHandler {
	return func(msg *nats.Msg) {
		evt, err := sharedevents.FromJSON(msg.Data)
		if err != nil {
			s.log.Warn("crm contact event: unmarshal failed, acking", zap.Error(err))
			_ = msg.Ack()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h(ctx, evt); err != nil {
			s.log.Error("crm contact event: apply failed, naking for retry",
				zap.String("event_type", evt.EventType), zap.Error(err))
			// Wait before the retry so a CRM outage does not use up the redeliveries at once.
			_ = msg.NakWithDelay(nakDelay)
			return
		}
		_ = msg.Ack()
	}
}

// handleUpdated refreshes the snapshot of the changed contact.
func (s *CRMContactUpdatedSubscriber) handleUpdated(ctx context.Context, evt *sharedevents.Event) error {
	contactID, ok := s.contactID(evt)
	if !ok {
		return nil
	}
	return s.snaps.RefreshSnapshots(ctx, evt.TenantID, []uuid.UUID{contactID})
}

// handleDeleted clears the deleted contact's id from local records. Points are never deleted.
func (s *CRMContactUpdatedSubscriber) handleDeleted(ctx context.Context, evt *sharedevents.Event) error {
	contactID, ok := s.contactID(evt)
	if !ok {
		return nil
	}
	return s.snaps.Unlink(ctx, evt.TenantID, contactID)
}

// contactID reads the tenant and contact id. A message missing either is skipped.
func (s *CRMContactUpdatedSubscriber) contactID(evt *sharedevents.Event) (uuid.UUID, bool) {
	id, _ := uuid.Parse(strFromPayloadAny(evt.Payload["contact_id"]))
	if evt.TenantID == uuid.Nil || id == uuid.Nil {
		s.log.Warn("crm contact event: missing tenant_id or contact_id, skipping",
			zap.String("event_type", evt.EventType))
		return uuid.Nil, false
	}
	return id, true
}
