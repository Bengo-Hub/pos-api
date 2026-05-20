package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entdelivery "github.com/bengobox/pos-service/internal/ent/webhookdelivery"
	entsubscription "github.com/bengobox/pos-service/internal/ent/webhooksubscription"
)

const (
	deliveryInterval    = 10 * time.Second
	deliveryHTTPTimeout = 10 * time.Second
	maxAttempts         = 5
)

// DeliveryWorker polls webhook_deliveries with status=pending and POSTs payloads
// to subscriber target_url with exponential backoff (capped at maxAttempts).
type DeliveryWorker struct {
	db     *ent.Client
	client *http.Client
	log    *zap.Logger
}

// NewDeliveryWorker creates a webhook delivery worker.
func NewDeliveryWorker(db *ent.Client, log *zap.Logger) *DeliveryWorker {
	return &DeliveryWorker{
		db:     db,
		client: &http.Client{Timeout: deliveryHTTPTimeout},
		log:    log.Named("webhook.delivery"),
	}
}

// Start runs the delivery loop until ctx is cancelled.
func (w *DeliveryWorker) Start(ctx context.Context) {
	ticker := time.NewTicker(deliveryInterval)
	defer ticker.Stop()

	w.log.Info("webhook delivery worker started")
	for {
		select {
		case <-ctx.Done():
			w.log.Info("webhook delivery worker stopped")
			return
		case <-ticker.C:
			w.processPending(ctx)
		}
	}
}

// Dispatch enqueues a delivery record for every active subscription matching eventType + tenantID.
// Call this from handlers after state transitions (order.completed, payment.received, etc.).
func (w *DeliveryWorker) Dispatch(ctx context.Context, tenantID uuid.UUID, eventType string, data any) {
	subs, err := w.db.WebhookSubscription.Query().
		Where(
			entsubscription.TenantID(tenantID),
			entsubscription.EventType(eventType),
			entsubscription.IsActive(true),
		).
		All(ctx)
	if err != nil {
		w.log.Warn("dispatch: query subscriptions failed", zap.Error(err))
		return
	}
	if len(subs) == 0 {
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"event":     eventType,
		"tenant_id": tenantID.String(),
		"data":      data,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})

	for _, sub := range subs {
		_, err := w.db.WebhookDelivery.Create().
			SetSubscriptionID(sub.ID).
			SetEventType(eventType).
			SetPayload(string(payload)).
			SetStatus("pending").
			SetAttempt(0).
			Save(ctx)
		if err != nil {
			w.log.Warn("dispatch: create delivery record failed",
				zap.String("sub_id", sub.ID.String()), zap.Error(err))
		}
	}
}

func (w *DeliveryWorker) processPending(ctx context.Context) {
	deliveries, err := w.db.WebhookDelivery.Query().
		Where(entdelivery.Status("pending")).
		Limit(50).
		All(ctx)
	if err != nil {
		w.log.Warn("delivery worker: query failed", zap.Error(err))
		return
	}

	for _, d := range deliveries {
		sub, err := w.db.WebhookSubscription.Get(ctx, d.SubscriptionID)
		if err != nil {
			w.log.Warn("delivery: subscription not found",
				zap.String("delivery_id", d.ID.String()), zap.Error(err))
			w.markFailed(ctx, d.ID, d.Attempt, "subscription not found", 0, "")
			continue
		}

		if !sub.IsActive {
			w.markFailed(ctx, d.ID, d.Attempt, "subscription inactive", 0, "")
			continue
		}

		if d.Attempt >= maxAttempts {
			w.markFailed(ctx, d.ID, d.Attempt, "max attempts reached", 0, "")
			continue
		}

		w.deliver(ctx, d, sub)
	}
}

func (w *DeliveryWorker) deliver(ctx context.Context, d *ent.WebhookDelivery, sub *ent.WebhookSubscription) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.TargetURL, bytes.NewBufferString(d.Payload))
	if err != nil {
		w.markFailed(ctx, d.ID, d.Attempt+1, err.Error(), 0, "")
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Event", d.EventType)
	req.Header.Set("X-Delivery-ID", d.ID.String())

	if sub.Secret != "" {
		sig := hmacSHA256(sub.Secret, d.Payload)
		req.Header.Set("X-Webhook-Signature", "sha256="+sig)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		w.markFailed(ctx, d.ID, d.Attempt+1, err.Error(), 0, "")
		return
	}
	defer resp.Body.Close()

	var respBody bytes.Buffer
	respBody.ReadFrom(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		now := time.Now()
		_ = w.db.WebhookDelivery.UpdateOneID(d.ID).
			SetStatus("success").
			SetHTTPStatus(resp.StatusCode).
			SetResponseBody(respBody.String()).
			SetAttempt(d.Attempt + 1).
			SetDeliveredAt(now).
			Exec(ctx)
		w.log.Debug("webhook delivered",
			zap.String("delivery_id", d.ID.String()),
			zap.String("url", sub.TargetURL),
			zap.Int("status", resp.StatusCode))
	} else {
		w.markFailed(ctx, d.ID, d.Attempt+1, fmt.Sprintf("HTTP %d", resp.StatusCode), resp.StatusCode, respBody.String())
	}
}

func (w *DeliveryWorker) markFailed(ctx context.Context, id uuid.UUID, attempt int, errMsg string, httpStatus int, body string) {
	upd := w.db.WebhookDelivery.UpdateOneID(id).
		SetStatus("failed").
		SetAttempt(attempt).
		SetErrorMessage(errMsg)
	if httpStatus > 0 {
		upd = upd.SetHTTPStatus(httpStatus).SetResponseBody(body)
	}
	if err := upd.Exec(ctx); err != nil {
		w.log.Warn("markFailed: update failed", zap.Error(err))
	}
}

func hmacSHA256(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
