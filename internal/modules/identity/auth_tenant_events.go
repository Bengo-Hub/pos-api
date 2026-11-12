package identity

import (
	"context"
	"fmt"
	"strings"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// tenantSlugRenamer is the part of tenant.Syncer this handler needs.
type tenantSlugRenamer interface {
	RenameTenantSlug(ctx context.Context, tenantID uuid.UUID, newSlug string) (string, error)
}

// AuthTenantEventHandler projects auth.tenant.updated into pos-api. Auth owns the slug, so a
// rename (same UUID, new slug) moves the local tenant row and that tenant's outlets onto the new
// slug and drops the cached tenant under both slugs. Receipts pass the outlet slug to treasury,
// so the outlets must move with the tenant.
type AuthTenantEventHandler struct {
	renamer tenantSlugRenamer
	logger  *zap.Logger
}

func NewAuthTenantEventHandler(renamer tenantSlugRenamer, logger *zap.Logger) *AuthTenantEventHandler {
	return &AuthTenantEventHandler{renamer: renamer, logger: logger.Named("identity.auth_tenant_events")}
}

// SubscribeToTenantEvents subscribes to auth.tenant.updated with a durable consumer.
func (h *AuthTenantEventHandler) SubscribeToTenantEvents(nc *nats.Conn) error {
	if nc == nil {
		h.logger.Warn("NATS not available, skipping auth tenant event subscription")
		return nil
	}
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("auth tenant events: jetstream init: %w", err)
	}
	const subject, durable = "auth.tenant.updated", "pos-auth-tenant-updated"
	sharedevents.SubscribeQueueWithRebind(h.logger, js, authStream, subject, durable, func(msg *nats.Msg) {
		evt, err := sharedevents.FromJSON(msg.Data)
		if err != nil {
			h.logger.Error("failed to unmarshal tenant event", zap.Error(err))
			_ = msg.Term()
			return
		}
		if err := h.handleTenantUpdated(context.Background(), evt); err != nil {
			h.logger.Error("failed to handle tenant event", zap.String("tenant_id", evt.TenantID.String()), zap.Error(err))
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	},
		nats.Durable(durable),
		nats.AckExplicit(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(5),
		nats.DeliverAll(),
	)
	h.logger.Info("tenant event subscription active", zap.String("subject", subject))
	return nil
}

// handleTenantUpdated renames the local slug when auth's differs. A slug held by another local
// tenant is logged and acknowledged: retrying cannot fix it, and it needs a person to look.
func (h *AuthTenantEventHandler) handleTenantUpdated(ctx context.Context, evt *sharedevents.Event) error {
	slug, _ := evt.Payload["slug"].(string)
	slug = strings.TrimSpace(slug)
	tenantID := evt.TenantID
	if tenantID == uuid.Nil {
		if v, ok := evt.Payload["tenant_id"].(string); ok {
			tenantID, _ = uuid.Parse(v)
		}
	}
	if slug == "" || tenantID == uuid.Nil {
		return nil
	}
	old, err := h.renamer.RenameTenantSlug(ctx, tenantID, slug)
	if err != nil {
		if strings.Contains(err.Error(), "held by another local tenant") {
			h.logger.Error("tenant slug rename refused, slug held by another local tenant",
				zap.String("tenant_id", tenantID.String()), zap.String("slug", slug))
			return nil
		}
		return err
	}
	if old != "" {
		h.logger.Info("tenant slug renamed from auth.tenant.updated",
			zap.String("tenant_id", tenantID.String()), zap.String("old_slug", old), zap.String("slug", slug))
	}
	return nil
}
