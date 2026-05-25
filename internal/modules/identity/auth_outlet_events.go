package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
)

// authStream is the NATS JetStream stream name that auth-api publishes to.
const authStream = "auth"

// AuthOutletEventHandler syncs auth.outlet.* events from auth-api into the
// local pos-api outlets table. This keeps the pos-api mirror in sync with the
// source-of-truth outlet registry in auth-api.
type AuthOutletEventHandler struct {
	client *ent.Client
	logger *zap.Logger
}

func NewAuthOutletEventHandler(client *ent.Client, logger *zap.Logger) *AuthOutletEventHandler {
	return &AuthOutletEventHandler{
		client: client,
		logger: logger.Named("identity.auth_outlet_events"),
	}
}

type authOutletEvent struct {
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	TenantID      uuid.UUID              `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

// SubscribeToOutletEvents subscribes to auth.outlet.* JetStream subjects with durable consumers.
func (h *AuthOutletEventHandler) SubscribeToOutletEvents(nc *nats.Conn) error {
	if nc == nil {
		h.logger.Warn("NATS not available, skipping auth outlet event subscriptions")
		return nil
	}

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("auth outlet events: jetstream init: %w", err)
	}

	// Ensure the auth stream exists (auth-api creates it; guard against startup race).
	if _, err := js.StreamInfo(authStream); err != nil {
		if _, addErr := js.AddStream(&nats.StreamConfig{
			Name:      authStream,
			Subjects:  []string{"auth.>"},
			Retention: nats.LimitsPolicy,
			MaxAge:    72 * time.Hour,
			Storage:   nats.FileStorage,
		}); addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
			h.logger.Warn("auth outlet events: ensure auth stream failed", zap.Error(addErr))
		}
	}

	type sub struct {
		subject string
		durable string
		handler func(context.Context, *authOutletEvent) error
	}
	subs := []sub{
		{"auth.outlet.created", "pos-auth-outlet-created", h.handleUpsert},
		{"auth.outlet.updated", "pos-auth-outlet-updated", h.handleUpsert},
		{"auth.outlet.archived", "pos-auth-outlet-archived", h.handleArchive},
	}

	for _, s := range subs {
		s := s
		if _, subErr := js.Subscribe(s.subject, func(msg *nats.Msg) {
			var evt authOutletEvent
			if err := json.Unmarshal(msg.Data, &evt); err != nil {
				h.logger.Error("failed to unmarshal outlet event",
					zap.String("subject", s.subject), zap.Error(err))
				_ = msg.Nak()
				return
			}
			ctx := context.Background()
			if err := s.handler(ctx, &evt); err != nil {
				h.logger.Error("failed to handle outlet event",
					zap.String("subject", s.subject), zap.Error(err))
				_ = msg.Nak()
				return
			}
			_ = msg.Ack()
		},
			nats.Durable(s.durable),
			nats.AckExplicit(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(5),
			nats.DeliverAll(),
		); subErr != nil {
			h.logger.Warn("auth outlet events: subscribe failed",
				zap.String("subject", s.subject), zap.Error(subErr))
		}
	}

	h.logger.Info("outlet event subscriptions active",
		zap.String("subjects", "auth.outlet.created, auth.outlet.updated, auth.outlet.archived"))
	return nil
}

// handleUpsert creates or updates a local outlet mirror from auth.outlet.created/updated.
func (h *AuthOutletEventHandler) handleUpsert(ctx context.Context, evt *authOutletEvent) error {
	outletIDStr, _ := evt.Payload["outlet_id"].(string)
	code, _ := evt.Payload["code"].(string)
	name, _ := evt.Payload["name"].(string)
	useCase, _ := evt.Payload["use_case"].(string)
	isHQ, _ := evt.Payload["is_hq"].(bool)
	status, _ := evt.Payload["status"].(string)
	if status == "" {
		status = "active"
	}

	outletID, err := uuid.Parse(outletIDStr)
	if err != nil {
		return fmt.Errorf("invalid outlet_id %q: %w", outletIDStr, err)
	}
	if evt.TenantID == uuid.Nil {
		return fmt.Errorf("missing tenant_id in outlet event")
	}

	// We need the tenant_slug for the outlet record. Derive from a local tenant lookup.
	tenantSlug := ""
	t, tErr := h.client.Tenant.Get(ctx, evt.TenantID)
	if tErr == nil {
		tenantSlug = t.Slug
	}

	// Upsert: try to find existing outlet by its auth-service UUID (we use the same UUID).
	existing, findErr := h.client.Outlet.Get(ctx, outletID)
	if findErr != nil {
		// Not found — create it.
		createQ := h.client.Outlet.Create().
			SetID(outletID).
			SetTenantID(evt.TenantID).
			SetTenantSlug(tenantSlug).
			SetCode(code).
			SetName(name).
			SetIsHq(isHQ).
			SetStatus(status)
		if useCase != "" {
			createQ = createQ.SetUseCase(useCase)
		}
		if _, err := createQ.Save(ctx); err != nil {
			return fmt.Errorf("create outlet mirror: %w", err)
		}
		h.logger.Info("outlet created from auth event",
			zap.String("outlet_id", outletID.String()),
			zap.String("code", code))
		return nil
	}

	// Found — update mutable fields.
	upd := h.client.Outlet.UpdateOne(existing).
		SetName(name).
		SetIsHq(isHQ).
		SetStatus(status).
		SetUpdatedAt(time.Now())
	if useCase != "" {
		upd = upd.SetUseCase(useCase)
	}
	if _, err := upd.Save(ctx); err != nil {
		return fmt.Errorf("update outlet mirror: %w", err)
	}
	h.logger.Info("outlet updated from auth event",
		zap.String("outlet_id", outletID.String()),
		zap.String("code", code))
	return nil
}

// handleArchive sets status = "archived" for the outlet.
func (h *AuthOutletEventHandler) handleArchive(ctx context.Context, evt *authOutletEvent) error {
	outletIDStr, _ := evt.Payload["outlet_id"].(string)
	outletID, err := uuid.Parse(outletIDStr)
	if err != nil {
		return fmt.Errorf("invalid outlet_id %q: %w", outletIDStr, err)
	}

	if err := h.client.Outlet.UpdateOneID(outletID).SetStatus("archived").Exec(ctx); err != nil {
		if ent.IsNotFound(err) {
			return nil // already gone
		}
		return fmt.Errorf("archive outlet mirror: %w", err)
	}
	h.logger.Info("outlet archived from auth event", zap.String("outlet_id", outletID.String()))
	return nil
}
