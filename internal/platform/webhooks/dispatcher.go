package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entwhd "github.com/bengobox/pos-service/internal/ent/webhookdelivery"
	entwh "github.com/bengobox/pos-service/internal/ent/webhooksubscription"
)

const (
	maxAttempts    = 3
	deliverTimeout = 10 * time.Second
)

// Dispatcher subscribes to NATS pos.> events and fans them out to matching webhook subscriptions.
type Dispatcher struct {
	db     *ent.Client
	logger *zap.Logger
	client *http.Client
	sub    *nats.Subscription
}

func NewDispatcher(db *ent.Client, logger *zap.Logger) *Dispatcher {
	return &Dispatcher{
		db:     db,
		logger: logger.Named("webhooks.dispatcher"),
		client: &http.Client{Timeout: deliverTimeout},
	}
}

func (d *Dispatcher) Start(conn *nats.Conn) error {
	// QueueSubscribe (own group, separate from the loyalty consumer's group): with
	// >1 replica only one pod fans each pos.* event out to webhooks — no double POSTs.
	//
	// DELIBERATELY core NATS (at-most-once), NOT a durable JetStream consumer: webhook
	// fan-out POSTs to EXTERNAL endpoints, and JetStream redelivery after an ack gap
	// would double-POST third parties. An event published while no replica is
	// subscribed is simply not fanned out — acceptable for best-effort webhooks.
	// (Fleet uniform-consumer rule exception, audited 2026-07-18.)
	sub, err := conn.QueueSubscribe("pos.>", "pos-webhooks", d.handle)
	if err != nil {
		return err
	}
	d.sub = sub
	d.logger.Info("webhook dispatcher subscribed to pos.> (queue group pos-webhooks)")
	return nil
}

func (d *Dispatcher) Stop() {
	if d.sub != nil {
		_ = d.sub.Drain()
	}
}

type natsEnvelope struct {
	EventType string          `json:"event_type"`
	TenantID  string          `json:"tenant_id"`
	Payload   json.RawMessage `json:"payload"`
}

func (d *Dispatcher) handle(msg *nats.Msg) {
	var env natsEnvelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return
	}

	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Load all active webhook subscriptions matching the event type for this tenant.
	subs, err := d.db.WebhookSubscription.Query().
		Where(
			entwh.TenantID(tenantID),
			entwh.EventType(env.EventType),
			entwh.IsActive(true),
		).
		All(ctx)
	if err != nil || len(subs) == 0 {
		return
	}

	rawPayload := string(msg.Data)

	for _, ws := range subs {
		d.deliver(ctx, ws.ID, ws.TargetURL, env.EventType, rawPayload)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, subID uuid.UUID, targetURL, eventType, payload string) {
	// Create a pending delivery record.
	delivery, err := d.db.WebhookDelivery.Create().
		SetSubscriptionID(subID).
		SetEventType(eventType).
		SetPayload(payload).
		Save(ctx)
	if err != nil {
		d.logger.Error("failed to create webhook delivery record", zap.Error(err))
		return
	}

	var (
		httpStatus   *int
		responseBody string
		deliveryErr  string
		status       = "success"
	)

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		code, body, postErr := d.post(targetURL, payload)
		if postErr == nil && code < 300 {
			httpStatus = &code
			responseBody = body
			break
		}

		// Exponential backoff before retry.
		if attempt < maxAttempts {
			time.Sleep(time.Duration(attempt*attempt) * time.Second)
		}

		if postErr != nil {
			deliveryErr = postErr.Error()
		} else {
			httpStatus = &code
			responseBody = body
			deliveryErr = "non-2xx response"
		}
		status = "failed"
	}

	now := time.Now()
	u := d.db.WebhookDelivery.UpdateOneID(delivery.ID).
		SetStatus(status).
		SetAttempt(maxAttempts).
		SetDeliveredAt(now)
	if httpStatus != nil {
		u = u.SetHTTPStatus(*httpStatus)
	}
	if responseBody != "" {
		u = u.SetResponseBody(responseBody)
	}
	if deliveryErr != "" {
		u = u.SetErrorMessage(deliveryErr)
	}

	if _, err := u.Save(ctx); err != nil {
		d.logger.Error("failed to update webhook delivery record", zap.Error(err),
			zap.String("delivery_id", delivery.ID.String()))
	}

	if status == "failed" {
		d.logger.Warn("webhook delivery failed after retries",
			zap.String("target_url", targetURL),
			zap.String("event_type", eventType))
	}
}

func (d *Dispatcher) post(targetURL, payload string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewBufferString(payload))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "POS-Webhook/1.0")

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String(), nil
}

// DeliveryStats returns pending/failed counts for a given subscription (used in list deliveries).
func DeliveryStats(ctx context.Context, db *ent.Client, subID uuid.UUID) (pending, failed int, err error) {
	pending, err = db.WebhookDelivery.Query().
		Where(entwhd.SubscriptionID(subID), entwhd.Status("pending")).
		Count(ctx)
	if err != nil {
		return
	}
	failed, err = db.WebhookDelivery.Query().
		Where(entwhd.SubscriptionID(subID), entwhd.Status("failed")).
		Count(ctx)
	return
}
