package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
)

// Syncer handles dynamic syncing of tenant data from auth-api using Ent ORM.
type Syncer struct {
	client  *ent.Client
	authURL string
}

// NewSyncer creates a new TenantSyncer.
// authURL is the base URL of the auth-api (e.g. from AUTH_SERVICE_URL config).
func NewSyncer(client *ent.Client, authURL string) *Syncer {
	return &Syncer{client: client, authURL: authURL}
}

// authAPITenantResponse is the full tenant JSON response from GET /api/v1/tenants/by-slug/{slug}.
type authAPITenantResponse struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Slug               string         `json:"slug"`
	Status             string         `json:"status"`
	ContactEmail       string         `json:"contact_email,omitempty"`
	ContactPhone       string         `json:"contact_phone,omitempty"`
	LogoURL            string         `json:"logo_url,omitempty"`
	Website            string         `json:"website,omitempty"`
	Country            string         `json:"country,omitempty"`
	Timezone           string         `json:"timezone,omitempty"`
	BrandColors        map[string]any `json:"brand_colors,omitempty"`
	OrgSize            string         `json:"org_size,omitempty"`
	UseCase            string         `json:"use_case,omitempty"`
	SubscriptionPlan   string         `json:"subscription_plan,omitempty"`
	SubscriptionStatus string         `json:"subscription_status,omitempty"`
	TierLimits         map[string]any `json:"tier_limits,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

// SyncTenant fetches the FULL tenant record from auth-api and persists it
// in the local PG DB using Ent.
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
	endpoint := strings.TrimRight(authAPIURL, "/") + "/api/v1/tenants/by-slug/" + slug

	log.Printf("  [tenant-sync] dynamically fetching %s from %s", slug, endpoint)
	resp, err := http.Get(endpoint) //nolint:noctx
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: tenant %q not found (404)", slug)
	}
	if resp.StatusCode != http.StatusOK {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: auth-api HTTP %d for %q", resp.StatusCode, slug)
	}

	var remote authAPITenantResponse
	if err := json.NewDecoder(resp.Body).Decode(&remote); err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: decode response: %w", err)
	}
	realID, err := uuid.Parse(remote.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: invalid UUID %q: %w", remote.ID, err)
	}

	extMeta := map[string]any{}
	for k, v := range remote.Metadata {
		extMeta[k] = v
	}

	// Use Ent Upsert
	err = s.client.Tenant.Create().
		SetID(realID).
		SetName(remote.Name).
		SetSlug(remote.Slug).
		SetStatus(remote.Status).
		SetContactEmail(remote.ContactEmail).
		SetContactPhone(remote.ContactPhone).
		SetLogoURL(remote.LogoURL).
		SetWebsite(remote.Website).
		SetCountry(remote.Country).
		SetTimezone(remote.Timezone).
		SetBrandColors(remote.BrandColors).
		SetOrgSize(remote.OrgSize).
		SetUseCase(remote.UseCase).
		SetSubscriptionPlan(remote.SubscriptionPlan).
		SetSubscriptionStatus(remote.SubscriptionStatus).
		SetTierLimits(remote.TierLimits).
		SetMetadata(extMeta).
		OnConflictColumns(enttenant.FieldID).
		UpdateNewValues().
		Exec(ctx)

	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: upsert failed: %w", err)
	}

	log.Printf("  [tenant-sync] dynamically synced %s (UUID %s) into pos-api DB", slug, realID)
	return realID, nil
}
