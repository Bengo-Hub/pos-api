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
	"github.com/bengobox/pos-service/internal/ent/user"
)

// AuthEventHandler handles auth-service user events for proactive user sync.
type AuthEventHandler struct {
	client       *ent.Client
	tenantSyncer interface {
		SyncTenant(ctx context.Context, slug string) (uuid.UUID, error)
	}
	logger *zap.Logger
}

// NewAuthEventHandler creates a new auth event handler.
func NewAuthEventHandler(client *ent.Client, svc *Service, logger *zap.Logger) *AuthEventHandler {
	return &AuthEventHandler{
		client:       client,
		tenantSyncer: svc.tenantSyncer,
		logger:       logger.Named("identity.auth_events"),
	}
}

// authUserEvent represents the shared-events envelope for auth user events.
type authUserEvent struct {
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	TenantID      uuid.UUID              `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

// SubscribeToAuthEvents subscribes to auth-service user events via NATS.
func (h *AuthEventHandler) SubscribeToAuthEvents(nc *nats.Conn) error {
	if nc == nil {
		h.logger.Warn("NATS connection not available, skipping auth event subscriptions")
		return nil
	}

	// Subscribe to auth.user.created
	_, err := nc.Subscribe("auth.user.created", func(msg *nats.Msg) {
		var evt authUserEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			h.logger.Error("failed to unmarshal auth.user.created event", zap.Error(err))
			return
		}

		ctx := context.Background()
		if err := h.handleUserCreated(ctx, &evt); err != nil {
			h.logger.Error("failed to handle auth.user.created event", zap.Error(err))
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("subscribe to auth.user.created: %w", err)
	}

	// Subscribe to auth.user.updated
	_, err = nc.Subscribe("auth.user.updated", func(msg *nats.Msg) {
		var evt authUserEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			h.logger.Error("failed to unmarshal auth.user.updated event", zap.Error(err))
			return
		}

		ctx := context.Background()
		if err := h.handleUserUpdated(ctx, &evt); err != nil {
			h.logger.Error("failed to handle auth.user.updated event", zap.Error(err))
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("subscribe to auth.user.updated: %w", err)
	}

	h.logger.Info("auth event subscriptions active",
		zap.Strings("subjects", []string{"auth.user.created", "auth.user.updated"}))
	return nil
}

func (h *AuthEventHandler) handleUserCreated(ctx context.Context, evt *authUserEvent) error {
	userIDStr, _ := evt.Payload["user_id"].(string)
	email, _ := evt.Payload["email"].(string)
	fullName, _ := evt.Payload["full_name"].(string)
	tenantSlug, _ := evt.Payload["tenant_slug"].(string)

	authServiceUserID, err := uuid.Parse(userIDStr)
	if err != nil {
		return fmt.Errorf("invalid user_id %q: %w", userIDStr, err)
	}

	// Check if user already exists
	exists, _ := h.client.User.Query().
		Where(user.AuthServiceUserIDEQ(authServiceUserID)).
		Exist(ctx)
	if exists {
		h.logger.Debug("user already exists, skipping",
			zap.String("user_id", userIDStr))
		return nil
	}

	// Resolve tenant ID — prefer tenant_slug from payload, fallback to event tenant_id
	var tenantID uuid.UUID
	if tenantSlug != "" && h.tenantSyncer != nil {
		tenantID, err = h.tenantSyncer.SyncTenant(ctx, tenantSlug)
		if err != nil {
			h.logger.Warn("failed to sync tenant from slug, using event tenant_id",
				zap.String("slug", tenantSlug), zap.Error(err))
			tenantID = evt.TenantID
		}
	} else {
		tenantID = evt.TenantID
	}

	if tenantID == uuid.Nil {
		return fmt.Errorf("no tenant_id available for user %s", userIDStr)
	}

	// Create user
	_, err = h.client.User.Create().
		SetID(authServiceUserID).
		SetAuthServiceUserID(authServiceUserID).
		SetTenantID(tenantID).
		SetEmail(email).
		SetFullName(fullName).
		SetStatus("active").
		SetSyncStatus("synced").
		SetSyncAt(time.Now()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create user from auth event: %w", err)
	}

	h.logger.Info("user created from auth.user.created event",
		zap.String("user_id", userIDStr),
		zap.String("tenant_id", tenantID.String()),
		zap.String("email", email))
	return nil
}

func (h *AuthEventHandler) handleUserUpdated(ctx context.Context, evt *authUserEvent) error {
	userIDStr, _ := evt.Payload["user_id"].(string)
	email, _ := evt.Payload["email"].(string)
	fullName, _ := evt.Payload["full_name"].(string)

	authServiceUserID, err := uuid.Parse(userIDStr)
	if err != nil {
		return fmt.Errorf("invalid user_id %q: %w", userIDStr, err)
	}

	// Find existing user
	u, err := h.client.User.Query().
		Where(user.AuthServiceUserIDEQ(authServiceUserID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			h.logger.Debug("user not found for update event, will be created on first login",
				zap.String("user_id", userIDStr))
			return nil
		}
		return fmt.Errorf("query user: %w", err)
	}

	// Update user fields
	update := h.client.User.UpdateOne(u)
	if email != "" {
		update = update.SetEmail(email)
	}
	if fullName != "" {
		update = update.SetFullName(fullName)
	}
	update = update.SetSyncStatus("synced").SetSyncAt(time.Now())

	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("update user from auth event: %w", err)
	}

	h.logger.Info("user updated from auth.user.updated event",
		zap.String("user_id", userIDStr),
		zap.String("email", email))
	return nil
}
