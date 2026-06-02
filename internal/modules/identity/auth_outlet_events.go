package identity

import (
	"context"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
)

// posAcceptedUseCases is the set of outlet use_cases that POS supports.
// Outlets with other use_cases (logistics, warehouse, truload) are ACKed and skipped.
var posAcceptedUseCases = map[string]bool{
	"hospitality":  true,
	"retail":       true,
	"quick_service": true,
	"pharmacy":     true,
	"services":     true,
}

// authStream is the NATS JetStream stream name that auth-api publishes to.
const authStream = "auth"

// AuthOutletEventHandler syncs auth.outlet.* events from auth-api into the
// local pos-api outlets table.
type AuthOutletEventHandler struct {
	client *ent.Client
	tenantSyncer interface {
		SyncTenant(ctx context.Context, slug string) (uuid.UUID, error)
	}
	logger *zap.Logger
}

func NewAuthOutletEventHandler(client *ent.Client, ts interface {
	SyncTenant(ctx context.Context, slug string) (uuid.UUID, error)
}, logger *zap.Logger) *AuthOutletEventHandler {
	return &AuthOutletEventHandler{
		client:       client,
		tenantSyncer: ts,
		logger:       logger.Named("identity.auth_outlet_events"),
	}
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
		handler func(context.Context, *sharedevents.Event) error
	}
	subs := []sub{
		{"auth.outlet.created", "pos-auth-outlet-created", h.handleUpsert},
		{"auth.outlet.updated", "pos-auth-outlet-updated", h.handleUpsert},
		{"auth.outlet.archived", "pos-auth-outlet-archived", h.handleArchive},
	}

	for _, s := range subs {
		s := s
		_, subErr := jsSubscribeOrRebind(js, authStream, s.subject, s.durable, func(msg *nats.Msg) {
			evt, err := sharedevents.FromJSON(msg.Data)
			if err != nil {
				h.logger.Error("failed to unmarshal outlet event",
					zap.String("subject", s.subject), zap.Error(err))
				_ = msg.Nak()
				return
			}
			ctx := context.Background()
			if err := s.handler(ctx, evt); err != nil {
				h.logger.Error("failed to handle outlet event",
					zap.String("subject", s.subject), zap.Error(err))
				_ = msg.Nak()
				return
			}
			_ = msg.Ack()
		},
			nats.AckExplicit(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(5),
			nats.DeliverAll(),
		)
		if subErr != nil {
			h.logger.Error("auth outlet events: subscribe failed (will not retry)",
				zap.String("subject", s.subject), zap.Error(subErr))
		}
	}

	h.logger.Info("outlet event subscriptions active",
		zap.String("subjects", "auth.outlet.created, auth.outlet.updated, auth.outlet.archived"))
	return nil
}

// handleUpsert creates or updates a local outlet mirror from auth.outlet.created/updated.
func (h *AuthOutletEventHandler) handleUpsert(ctx context.Context, evt *sharedevents.Event) error {
	outletIDStr, _ := evt.Payload["outlet_id"].(string)
	code, _ := evt.Payload["code"].(string)
	name, _ := evt.Payload["name"].(string)
	useCase, _ := evt.Payload["use_case"].(string)
	isHQ, _ := evt.Payload["is_hq"].(bool)
	status, _ := evt.Payload["status"].(string)
	if status == "" {
		status = "active"
	}

	// Skip outlets that don't apply to POS (logistics hubs, warehouses, weighbridges).
	if useCase != "" && !posAcceptedUseCases[useCase] {
		h.logger.Info("skipping outlet: use_case not applicable to pos-api",
			zap.String("outlet_id", outletIDStr),
			zap.String("use_case", useCase))
		return nil
	}

	outletID, err := uuid.Parse(outletIDStr)
	if err != nil {
		return fmt.Errorf("invalid outlet_id %q: %w", outletIDStr, err)
	}
	if evt.TenantID == uuid.Nil {
		return fmt.Errorf("missing tenant_id in outlet event")
	}

	// Prefer tenant_slug from the event payload (included since auth-api v245b7a8+).
	// Fall back to local tenant table lookup for older events in the stream.
	tenantSlug, _ := evt.Payload["tenant_slug"].(string)
	if tenantSlug == "" {
		if t, tErr := h.client.Tenant.Get(ctx, evt.TenantID); tErr == nil {
			tenantSlug = t.Slug
		}
	}
	if tenantSlug == "" {
		return fmt.Errorf("tenant_slug unavailable for outlet %s (tenant %s) — retry after tenant sync", outletIDStr, evt.TenantID)
	}

	// Ensure the tenant row exists locally (outlets FK-references tenants).
	// If missing, sync it from auth-api now so the outlet INSERT can succeed.
	if _, tErr := h.client.Tenant.Get(ctx, evt.TenantID); tErr != nil {
		if h.tenantSyncer != nil {
			if _, syncErr := h.tenantSyncer.SyncTenant(ctx, tenantSlug); syncErr != nil {
				return fmt.Errorf("tenant %q not in local DB and sync failed: %w", tenantSlug, syncErr)
			}
		} else {
			return fmt.Errorf("tenant %q not in local DB and no syncer configured", tenantSlug)
		}
	}

	existing, findErr := h.client.Outlet.Get(ctx, outletID)
	if findErr != nil {
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
// If the outlet was never synced to POS (filtered by use_case), this is a no-op.
func (h *AuthOutletEventHandler) handleArchive(ctx context.Context, evt *sharedevents.Event) error {
	outletIDStr, _ := evt.Payload["outlet_id"].(string)
	outletID, err := uuid.Parse(outletIDStr)
	if err != nil {
		return fmt.Errorf("invalid outlet_id %q: %w", outletIDStr, err)
	}

	if err := h.client.Outlet.UpdateOneID(outletID).SetStatus("archived").Exec(ctx); err != nil {
		if ent.IsNotFound(err) {
			return nil // outlet was never synced to POS — safe to ignore
		}
		return fmt.Errorf("archive outlet mirror: %w", err)
	}
	h.logger.Info("outlet archived from auth event", zap.String("outlet_id", outletID.String()))
	return nil
}
