package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
)

// s2sHTTPClient is the shared HTTP client for service-to-service calls in this
// package. It carries a timeout so upstream calls cannot hang indefinitely.
var s2sHTTPClient = &http.Client{Timeout: 15 * time.Second}

// posAcceptedUseCases mirrors the set in auth_outlet_events.go — only sync outlets
// that the POS service actually uses.
var posAcceptedUseCases = map[string]bool{
	"hospitality":   true,
	"retail":        true,
	"quick_service": true,
	"pharmacy":      true,
	"services":      true,
}

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
	authAPIURL := s.authURL
	if envURL := os.Getenv("AUTH_API_URL"); envURL != "" {
		authAPIURL = envURL
	}

	// Fast path: check if tenant exists locally.
	existing, err := s.client.Tenant.Query().Where(enttenant.SlugEQ(slug)).Only(ctx)
	if err == nil && existing != nil {
		// Best-effort refresh of the projected timezone so an admin's change in
		// auth-api propagates on the next auth event without a full re-sync. The
		// read is Redis-backed (cheap); failures and no-ops are non-fatal.
		if td, tzErr := sharedcache.GetTenantDetails(ctx, s.cache, authAPIURL, slug, sharedcache.DefaultTenantTTL); tzErr == nil {
			if tz := strings.TrimSpace(td.Timezone); tz != "" && tz != existing.Timezone {
				if uErr := s.client.Tenant.UpdateOneID(existing.ID).SetTimezone(tz).Exec(ctx); uErr != nil {
					log.Printf("  [tenant-sync] timezone refresh for %s failed: %v", slug, uErr)
				}
			}
		}
		return existing.ID, nil
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
	create := s.client.Tenant.Create().
		SetID(realID).
		SetName(remote.Name).
		SetSlug(remote.Slug).
		SetStatus(remote.Status).
		SetNillableUseCase(nillableStr(remote.UseCase)).
		SetSyncStatus("synced").
		SetLastSyncAt(now)
	if tz := strings.TrimSpace(remote.Timezone); tz != "" {
		create = create.SetTimezone(tz)
	}
	err = create.
		OnConflictColumns(enttenant.FieldID).
		UpdateNewValues().
		Exec(ctx)

	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: upsert failed: %w", err)
	}

	log.Printf("  [tenant-sync] synced %s (UUID %s) into pos-api DB via shared cache", slug, realID)
	return realID, nil
}

// authOutletItem is the shape of one entry from GET /api/v1/tenants/{slug}/outlets.
type authOutletItem struct {
	ID      string `json:"id"`
	Code    string `json:"code"`
	Name    string `json:"name"`
	UseCase string `json:"use_case"`
	IsHQ    bool   `json:"is_hq"`
	Status  string `json:"status"`
}

// SyncOutlets calls auth-api's public outlet list endpoint and upserts every
// POS-applicable outlet into the local outlets table. It is called on demand
// when handleUserPINSet finds no outlet for the tenant (e.g. on a fresh DB).
func (s *Syncer) SyncOutlets(ctx context.Context, tenantID uuid.UUID, tenantSlug string) error {
	authAPIURL := s.authURL
	if envURL := os.Getenv("AUTH_API_URL"); envURL != "" {
		authAPIURL = envURL
	}

	url := strings.TrimRight(authAPIURL, "/") + "/api/v1/tenants/" + tenantSlug + "/outlets"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("tenant.Syncer.SyncOutlets: build request: %w", err)
	}

	resp, err := s2sHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("tenant.Syncer.SyncOutlets: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tenant.Syncer.SyncOutlets: auth-api returned HTTP %d for %s", resp.StatusCode, url)
	}

	var items []authOutletItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return fmt.Errorf("tenant.Syncer.SyncOutlets: decode response: %w", err)
	}

	synced := 0
	for _, item := range items {
		// Skip outlets that POS doesn't serve.
		if item.UseCase != "" && !posAcceptedUseCases[item.UseCase] {
			continue
		}
		if item.Status == "archived" {
			continue
		}

		outletID, err := uuid.Parse(item.ID)
		if err != nil {
			continue
		}

		status := item.Status
		if status == "" {
			status = "active"
		}

		q := s.client.Outlet.Create().
			SetID(outletID).
			SetTenantID(tenantID).
			SetTenantSlug(tenantSlug).
			SetCode(item.Code).
			SetName(item.Name).
			SetIsHq(item.IsHQ).
			SetStatus(status)
		if item.UseCase != "" {
			q = q.SetUseCase(item.UseCase)
		}

		if err := q.OnConflict().DoNothing().Exec(ctx); err != nil {
			log.Printf("  [outlet-sync] upsert outlet %s (%s): %v", item.Code, outletID, err)
			continue
		}
		synced++
	}

	log.Printf("  [outlet-sync] synced %d outlet(s) for tenant %s from auth-api", synced, tenantSlug)
	return nil
}

// nillableStr returns a *string if non-empty, nil otherwise.
func nillableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
