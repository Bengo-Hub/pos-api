package kdsroute

import (
	"context"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Fetcher loads a tenant's inventory categories (S2S, inventory-api).
type Fetcher func(ctx context.Context, tenantID uuid.UUID) ([]Category, error)

// Source serves each tenant's category tree from a per-pod cache so the order hot path makes
// at most one inventory call per tenant every freshTTL. When inventory is slow or down it
// serves the last good tree, and after a failure it waits failBackoff before trying again, so
// order creation never stalls on a category lookup. A nil *Source returns nil trees, and the
// router then matches flat on each line's own category.
type Source struct {
	fetch    Fetcher
	log      *zap.Logger
	fresh    *sharedcache.Local[uuid.UUID, *Tree]
	lastGood *sharedcache.Local[uuid.UUID, *Tree]
	failed   *sharedcache.Local[uuid.UUID, struct{}]
	timeout  time.Duration
}

const (
	freshTTL    = 5 * time.Minute
	lastGoodTTL = 24 * time.Hour
	failBackoff = 30 * time.Second
	fetchBudget = 3 * time.Second
	maxTenants  = 5000
)

// NewSource wires a cached tree source around fetch.
func NewSource(fetch Fetcher, log *zap.Logger) *Source {
	if log == nil {
		log = zap.NewNop()
	}
	return &Source{
		fetch:    fetch,
		log:      log,
		fresh:    sharedcache.NewLocal[uuid.UUID, *Tree](maxTenants, freshTTL),
		lastGood: sharedcache.NewLocal[uuid.UUID, *Tree](maxTenants, lastGoodTTL),
		failed:   sharedcache.NewLocal[uuid.UUID, struct{}](maxTenants, failBackoff),
		timeout:  fetchBudget,
	}
}

// Tree returns the tenant's category tree, or nil when it has never been loaded successfully.
func (s *Source) Tree(ctx context.Context, tenantID uuid.UUID) *Tree {
	if s == nil || s.fetch == nil || tenantID == uuid.Nil {
		return nil
	}
	if t, ok := s.fresh.Get(tenantID); ok {
		return t
	}
	if _, backoff := s.failed.Get(tenantID); backoff {
		t, _ := s.lastGood.Get(tenantID)
		return t
	}
	return s.load(ctx, tenantID)
}

// Refresh reloads the tenant's tree now (the station settings screen calls it so a category
// rename shows up immediately on this pod) and returns it, or the last good tree on failure.
func (s *Source) Refresh(ctx context.Context, tenantID uuid.UUID) *Tree {
	if s == nil || s.fetch == nil || tenantID == uuid.Nil {
		return nil
	}
	return s.load(ctx, tenantID)
}

func (s *Source) load(ctx context.Context, tenantID uuid.UUID) *Tree {
	// Detached from the caller's cancellation (an order request may finish first) but bounded.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()
	cats, err := s.fetch(fctx, tenantID)
	if err != nil {
		s.failed.Set(tenantID, struct{}{})
		s.log.Warn("kdsroute: category tree fetch failed, using last good tree",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		t, _ := s.lastGood.Get(tenantID)
		return t
	}
	t := NewTree(cats)
	s.fresh.Set(tenantID, t)
	s.lastGood.Set(tenantID, t)
	s.failed.Delete(tenantID)
	return t
}
