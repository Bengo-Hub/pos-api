package payments

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
)

// TreasurySubscriber handles NATS events published by treasury-api.
type TreasurySubscriber struct {
	client     *ent.Client
	paymentSvc *Service
	log        *zap.Logger
}

// NewTreasurySubscriber creates a subscriber for treasury-api events.
func NewTreasurySubscriber(client *ent.Client, paymentSvc *Service, log *zap.Logger) *TreasurySubscriber {
	return &TreasurySubscriber{
		client:     client,
		paymentSvc: paymentSvc,
		log:        log.Named("pos.treasury_subscriber"),
	}
}

// treasuryPaymentEvent is the common envelope for treasury payment events.
// It matches the shared-events (github.com/Bengo-Hub/shared-events) Event wire format:
// the business fields live under "payload" and the kind under "event_type" — NOT "data"/"type".
type treasuryPaymentEvent struct {
	ID        string         `json:"id"`
	EventType string         `json:"event_type"`
	TenantID  string         `json:"tenant_id"`
	Payload   map[string]any `json:"payload"`
}

// SubscribeToTreasuryEvents wires subscriptions to:
//   - treasury.payment.succeeded → confirm pending payment + complete order
//   - treasury.payment.failed    → mark pending payment as failed
//   - treasury.etims.invoice_transmitted → store eTIMS invoice number + QR URL on pos_order
func (s *TreasurySubscriber) SubscribeToTreasuryEvents(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("treasury subscriber: NATS connection is nil")
	}

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("treasury subscriber: jetstream: %w", err)
	}

	if err := s.subscribePaymentSuccess(js); err != nil {
		return err
	}
	if err := s.subscribePaymentFailed(js); err != nil {
		return err
	}
	if err := s.subscribeEtimsTransmitted(js); err != nil {
		return err
	}

	s.log.Info("treasury event subscriptions registered")
	return nil
}

func (s *TreasurySubscriber) subscribePaymentSuccess(js nats.JetStreamContext) error {
	// Subject is "treasury.payment.succeeded" (shared-events builds {aggregate_type}.{event_type}
	// = "treasury" + "payment.succeeded"). The durable is suffixed "-succeeded" because the previous
	// consumer ("pos-treasury-payment-success") was bound to the wrong, never-published subject
	// "treasury.payment.success"; a JetStream durable's filter subject is immutable, so we must use a
	// new durable name to rebind. The orphaned consumer is removed during deploy.
	_, err := js.QueueSubscribe("treasury.payment.succeeded", "pos-treasury-payment-succeeded", func(msg *nats.Msg) {
		defer func() { _ = msg.Ack() }()

		var evt treasuryPaymentEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			s.log.Error("treasury.payment.succeeded: unmarshal", zap.Error(err))
			return
		}

		intentID, _ := evt.Payload["payment_intent_id"].(string)
		if intentID == "" {
			intentID, _ = evt.Payload["intent_id"].(string)
		}
		if intentID == "" {
			s.log.Warn("treasury.payment.succeeded: missing intent id", zap.Any("payload", evt.Payload))
			return
		}

		tenantID, _ := uuid.Parse(evt.TenantID)
		if err := s.paymentSvc.ConfirmPaymentByIntentID(context.Background(), tenantID, intentID); err != nil {
			s.log.Error("treasury.payment.succeeded: confirm payment", zap.String("intent", intentID), zap.Error(err))
		}
	}, nats.Durable("pos-treasury-payment-succeeded"), nats.ManualAck())
	return err
}

func (s *TreasurySubscriber) subscribePaymentFailed(js nats.JetStreamContext) error {
	_, err := js.QueueSubscribe("treasury.payment.failed", "pos-treasury-payment-failed", func(msg *nats.Msg) {
		defer func() { _ = msg.Ack() }()

		var evt treasuryPaymentEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			s.log.Error("treasury.payment.failed: unmarshal", zap.Error(err))
			return
		}

		intentID, _ := evt.Payload["payment_intent_id"].(string)
		if intentID == "" {
			intentID, _ = evt.Payload["intent_id"].(string)
		}
		if intentID == "" {
			return
		}

		if err := s.paymentSvc.FailPaymentByIntentID(context.Background(), intentID); err != nil {
			s.log.Error("treasury.payment.failed: mark failed", zap.String("intent", intentID), zap.Error(err))
		}
	}, nats.Durable("pos-treasury-payment-failed"), nats.ManualAck())
	return err
}

// etimsEvent is the payload for treasury.etims.invoice_transmitted.
type etimsEvent struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Data     struct {
		ReferenceID     string `json:"reference_id"`      // pos_order UUID
		ReferenceType   string `json:"reference_type"`    // "pos_order"
		InvoiceNumber   string `json:"invoice_number"`
		QRCodeURL       string `json:"qr_code_url"`
	} `json:"payload"`
}

func (s *TreasurySubscriber) subscribeEtimsTransmitted(js nats.JetStreamContext) error {
	_, err := js.QueueSubscribe("treasury.etims.invoice_transmitted", "pos-etims-invoice", func(msg *nats.Msg) {
		defer func() { _ = msg.Ack() }()

		var evt etimsEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			s.log.Error("treasury.etims.invoice_transmitted: unmarshal", zap.Error(err))
			return
		}

		if evt.Data.ReferenceType != "pos_order" {
			return
		}

		orderID, err := uuid.Parse(evt.Data.ReferenceID)
		if err != nil {
			s.log.Warn("etims event: invalid order id", zap.String("reference_id", evt.Data.ReferenceID))
			return
		}

		// Persist eTIMS invoice number + QR code URL on the pos_order.
		// The eTIMS device submission is treasury-api's responsibility; pos-api only stores the outcome.
		_, err = s.client.POSOrder.Update().
			Where(posorder.ID(orderID)).
			SetNillableEtimsInvoiceNumber(nilIfEmpty(evt.Data.InvoiceNumber)).
			SetNillableEtimsQrCodeURL(nilIfEmpty(evt.Data.QRCodeURL)).
			Save(context.Background())
		if err != nil {
			s.log.Error("etims: failed to store invoice data on order",
				zap.String("order_id", orderID.String()), zap.Error(err))
		}
	}, nats.Durable("pos-etims-invoice"), nats.ManualAck())
	return err
}

