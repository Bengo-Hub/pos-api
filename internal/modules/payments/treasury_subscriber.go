package payments

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	sharedevents "github.com/Bengo-Hub/shared-events"
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
	// hasFeature gates treasury→POS data sync by subscription entitlement. Nil → fail open.
	hasFeature func(ctx context.Context, tenantID, feature string) bool
}

// NewTreasurySubscriber creates a subscriber for treasury-api events.
func NewTreasurySubscriber(client *ent.Client, paymentSvc *Service, log *zap.Logger) *TreasurySubscriber {
	return &TreasurySubscriber{
		client:     client,
		paymentSvc: paymentSvc,
		log:        log.Named("pos.treasury_subscriber"),
	}
}

// SetFeatureGate wires the subscription entitlement check used to gate treasury sync.
func (s *TreasurySubscriber) SetFeatureGate(fn func(ctx context.Context, tenantID, feature string) bool) {
	s.hasFeature = fn
}

// entitled reports whether tenant may receive synced treasury data. Fails open when no
// gate is wired or tenantID is unparseable (never block a real payment on a parse miss).
func (s *TreasurySubscriber) entitled(ctx context.Context, tenantID, feature string) bool {
	if s.hasFeature == nil || tenantID == "" {
		return true
	}
	return s.hasFeature(ctx, tenantID, feature)
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
	sharedevents.SubscribeQueueWithRebind(s.log, js, "treasury", "treasury.payment.succeeded", "pos-treasury-payment-succeeded", func(msg *nats.Msg) {
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

		if !s.entitled(context.Background(), evt.TenantID, "basic_treasury_access") {
			s.log.Debug("treasury.payment.succeeded: tenant lacks basic_treasury_access — skipping POS sync",
				zap.String("tenant_id", evt.TenantID))
			return
		}
		tenantID, _ := uuid.Parse(evt.TenantID)
		// Pass the amount treasury ACTUALLY settled (payload "amount", stringified decimal)
		// so the local pending row is corrected if the captured amount differs from the
		// intent's opening amount — a payment can only count what was really collected.
		settled := parseEventAmount(evt.Payload["amount"])
		// Gateway-resolved payer name (Paystack customer; M-Pesa STK carries none) → stamped onto
		// the POS payment so the receipt shows who paid for online payments.
		payerName, _ := evt.Payload["payer_name"].(string)
		if err := s.paymentSvc.ConfirmPaymentByIntentID(context.Background(), tenantID, intentID, settled, payerName); err != nil {
			s.log.Error("treasury.payment.succeeded: confirm payment", zap.String("intent", intentID), zap.Error(err))
		}
	}, nats.Durable("pos-treasury-payment-succeeded"), nats.ManualAck())
	return nil
}

// parseEventAmount coerces an event payload amount (stringified decimal from treasury, or a
// raw JSON number) to float64; 0 means "unknown — keep the locally recorded amount".
func parseEventAmount(v any) float64 {
	switch t := v.(type) {
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0
		}
		return f
	case float64:
		return t
	default:
		return 0
	}
}

func (s *TreasurySubscriber) subscribePaymentFailed(js nats.JetStreamContext) error {
	sharedevents.SubscribeQueueWithRebind(s.log, js, "treasury", "treasury.payment.failed", "pos-treasury-payment-failed", func(msg *nats.Msg) {
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

		if !s.entitled(context.Background(), evt.TenantID, "basic_treasury_access") {
			return
		}
		if err := s.paymentSvc.FailPaymentByIntentID(context.Background(), intentID); err != nil {
			s.log.Error("treasury.payment.failed: mark failed", zap.String("intent", intentID), zap.Error(err))
		}
	}, nats.Durable("pos-treasury-payment-failed"), nats.ManualAck())
	return nil
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
	sharedevents.SubscribeQueueWithRebind(s.log, js, "treasury", "treasury.etims.invoice_transmitted", "pos-etims-invoice", func(msg *nats.Msg) {
		defer func() { _ = msg.Ack() }()

		var evt etimsEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			s.log.Error("treasury.etims.invoice_transmitted: unmarshal", zap.Error(err))
			return
		}

		if evt.Data.ReferenceType != "pos_order" {
			return
		}

		if !s.entitled(context.Background(), evt.TenantID, "etims_integration") {
			s.log.Debug("treasury.etims.invoice_transmitted: tenant lacks etims_integration — skipping POS sync",
				zap.String("tenant_id", evt.TenantID))
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
	return nil
}

