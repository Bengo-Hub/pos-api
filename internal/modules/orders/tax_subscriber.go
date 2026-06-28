package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
)

// TaxSubscriber listens for treasury.tax.code_updated and invalidates pos-api's cached
// tax rates so a rate/code change propagates immediately instead of waiting for the
// 10-minute Redis TTL on the keys owned by TaxResolver.
type TaxSubscriber struct {
	client   *ent.Client
	resolver *TaxResolver
	log      *zap.Logger
}

// NewTaxSubscriber creates a subscriber that invalidates cached tax data on
// treasury.tax.code_updated. client is used to map the event's tenant UUID → slug
// (TaxResolver's cache keys are slug-scoped). resolver must be non-nil.
func NewTaxSubscriber(client *ent.Client, resolver *TaxResolver, log *zap.Logger) *TaxSubscriber {
	return &TaxSubscriber{
		client:   client,
		resolver: resolver,
		log:      log.Named("pos.tax_subscriber"),
	}
}

// taxCodeUpdatedEvent is the payload for treasury.tax.code_updated.
// Per the treasury-api contract: {"tenant_id":"<uuid>","code":"<taxcode>","action":"..."}.
// The shared-events envelope nests business fields under "payload".
type taxCodeUpdatedEvent struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Payload  struct {
		TenantID string `json:"tenant_id"`
		Code     string `json:"code"`
		Action   string `json:"action"`
	} `json:"payload"`
}

// SubscribeToTaxEvents wires the treasury.tax.code_updated subscription on the JetStream
// context of the provided NATS connection, mirroring the treasury payment/eTIMS subscriber
// (JetStream QueueSubscribe + durable + manual ack).
func (s *TaxSubscriber) SubscribeToTaxEvents(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("tax subscriber: NATS connection is nil")
	}
	if s.resolver == nil {
		return fmt.Errorf("tax subscriber: tax resolver is nil")
	}

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("tax subscriber: jetstream: %w", err)
	}

	// Subject is "treasury.tax.code_updated" (shared-events builds {aggregate_type}.{event_type}
	// = "treasury" + "tax.code_updated"). Durable + queue group so a single pod handles each
	// event across replicas.
	sharedevents.SubscribeQueueWithRebind(s.log, js, "treasury", "treasury.tax.code_updated", "pos-treasury-tax-updated", func(msg *nats.Msg) {
		// Always ack — invalidation is best-effort; a no-op or a transient miss must not
		// redeliver forever (the 10-minute TTL is the backstop).
		defer func() { _ = msg.Ack() }()

		var evt taxCodeUpdatedEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			s.log.Error("treasury.tax.code_updated: unmarshal", zap.Error(err))
			return
		}

		tenantID := evt.Payload.TenantID
		if tenantID == "" {
			tenantID = evt.TenantID
		}
		code := evt.Payload.Code
		action := evt.Payload.Action

		s.log.Info("treasury.tax.code_updated received",
			zap.String("tenant_id", tenantID),
			zap.String("code", code),
			zap.String("action", action),
		)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Prefer the slug-scoped resolver path: map the event tenant UUID → slug via the
		// local Tenant projection (its id IS the auth-api real tenant UUID). Delete the exact
		// pos:tax:{slug}:{code} + pos:vatactive:{slug} keys.
		if slug := s.slugForTenant(ctx, tenantID); slug != "" {
			s.resolver.InvalidateCode(ctx, slug, code)
			return
		}

		// Fallback: tenant UUID not resolvable to a slug (tenant not yet synced into pos-api,
		// or no tenant id on the event). Pattern-scan pos:tax:*:{code} across all tenants.
		s.log.Debug("treasury.tax.code_updated: tenant slug unresolved — falling back to cross-tenant pattern scan",
			zap.String("tenant_id", tenantID), zap.String("code", code))
		s.resolver.InvalidateCodeAllTenants(ctx, code)
	}, nats.Durable("pos-treasury-tax-updated"), nats.ManualAck())

	s.log.Info("tax event subscription registered (treasury.tax.code_updated)")
	return nil
}

// slugForTenant maps a tenant UUID string to its slug using the local Tenant projection.
// Returns "" when the id is empty/unparseable or the tenant is not present locally.
func (s *TaxSubscriber) slugForTenant(ctx context.Context, tenantID string) string {
	if s.client == nil || tenantID == "" {
		return ""
	}
	id, err := uuid.Parse(tenantID)
	if err != nil {
		return ""
	}
	t, err := s.client.Tenant.Query().Where(enttenant.ID(id)).Only(ctx)
	if err != nil || t == nil {
		return ""
	}
	return t.Slug
}
