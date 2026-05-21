package subscriptions

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// CacheSubscriber listens for tenant.subscription.updated events and invalidates
// the shared tenant branding/metadata cache so downstream reads pick up new plan data.
type CacheSubscriber struct {
	redis  *redis.Client
	logger *zap.Logger
	sub    *nats.Subscription
}

// NewCacheSubscriber creates a CacheSubscriber.
func NewCacheSubscriber(redisClient *redis.Client, logger *zap.Logger) *CacheSubscriber {
	return &CacheSubscriber{
		redis:  redisClient,
		logger: logger.Named("subscriptions.cache-subscriber"),
	}
}

// Start subscribes to tenant.subscription.updated on the provided NATS connection.
func (s *CacheSubscriber) Start(conn *nats.Conn) error {
	sub, err := conn.Subscribe("tenant.subscription.updated", s.handle)
	if err != nil {
		return err
	}
	s.sub = sub
	s.logger.Info("subscribed to tenant.subscription.updated")
	return nil
}

// Stop drains the NATS subscription.
func (s *CacheSubscriber) Stop() {
	if s.sub != nil {
		_ = s.sub.Drain()
	}
}

func (s *CacheSubscriber) handle(msg *nats.Msg) {
	var wrapper struct {
		TenantSlug string                 `json:"tenant_slug,omitempty"`
		Payload    map[string]interface{} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &wrapper); err != nil {
		s.logger.Warn("failed to parse subscription.updated event", zap.Error(err))
		return
	}

	slug := wrapper.TenantSlug
	if slug == "" {
		if v, ok := wrapper.Payload["tenant_slug"].(string); ok {
			slug = v
		}
	}
	if slug == "" {
		s.logger.Warn("subscription.updated event missing tenant_slug, skipping")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cacheKey := "tenant:" + slug
	if err := s.redis.Del(ctx, cacheKey).Err(); err != nil {
		s.logger.Warn("failed to invalidate tenant cache",
			zap.String("key", cacheKey),
			zap.Error(err),
		)
		return
	}
	s.logger.Debug("invalidated tenant cache on subscription update", zap.String("key", cacheKey))
}
