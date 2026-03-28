package tenant

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
)

// Syncer handles dynamic syncing of tenant data from auth-api using Ent ORM.
// It uses the shared cache library to fetch tenant details from Redis first,
// falling back to auth-api on cache miss.
type Syncer struct {
	client  *ent.Client
	authURL string
	cache   *sharedcache.Aside
}

// NewSyncer creates a new TenantSyncer.
// authURL is the base URL of the auth-api (e.g. from AUTH_SERVICE_URL config).
// c may be nil — in that case every call falls through to auth-api directly.
func NewSyncer(client *ent.Client, authURL string, c *sharedcache.Aside) *Syncer {
	return &Syncer{client: client, authURL: authURL, cache: c}
}

// SyncTenant fetches the tenant record (via shared cache / auth-api) and
// persists the minimal reference in the local PG DB using Ent.
func (s *Syncer) SyncTenant(ctx context.Context, slug string) (uuid.UUID, error) {
	// Fast path: check if tenant exists locally
	existing, err := s.client.Tenant.Query().Where(enttenant.SlugEQ(slug)).Only(ctx)
	if err == nil && existing != nil {
		return existing.ID, nil
	}

	authAPIURL := s.authURL
	if envURL := os.Getenv("AUTH_API_URL"); envURL != "" {
		authAPIURL = envURL
	}

	// Use shared cache library to get tenant details (Redis → auth-api fallback).
	remote, err := sharedcache.GetTenantDetails(ctx, s.cache, authAPIURL, slug, sharedcache.DefaultTenantTTL)
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: cache.GetTenantDetails: %w", err)
	}

	realID, err := uuid.Parse(remote.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: invalid UUID %q: %w", remote.ID, err)
	}

	now := time.Now()

	// Use Ent Upsert
	err = s.client.Tenant.Create().
		SetID(realID).
		SetName(remote.Name).
		SetSlug(remote.Slug).
		SetStatus(remote.Status).
		SetNillableUseCase(nillableStr(remote.UseCase)).
		SetSyncStatus("synced").
		SetLastSyncAt(now).
		OnConflictColumns(enttenant.FieldID).
		UpdateNewValues().
		Exec(ctx)

	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: upsert failed: %w", err)
	}

	log.Printf("  [tenant-sync] synced %s (UUID %s) into pos-api DB via shared cache", slug, realID)
	return realID, nil
}

// nillableStr returns a *string if non-empty, nil otherwise.
func nillableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
