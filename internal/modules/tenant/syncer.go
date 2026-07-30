package tenant

import (
	"context"
	"database/sql"
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
	db      *sql.DB
}

// NewSyncer creates a new TenantSyncer.
// authURL is the base URL of the auth-api (e.g. from AUTH_SERVICE_URL config).
// c may be nil — in that case every call falls through to auth-api directly.
func NewSyncer(client *ent.Client, authURL string, c *sharedcache.Aside) *Syncer {
	return &Syncer{client: client, authURL: authURL, cache: c}
}

// WithDB attaches the raw database handle the drift self-heal needs. Without it a detected
// tenant-UUID drift is reported but not repaired (the stale local UUID is still returned),
// because re-keying spans every tenant-scoped table and cannot be expressed through Ent.
func (s *Syncer) WithDB(db *sql.DB) *Syncer {
	s.db = db
	return s
}

// sqlLiteral renders s as a single-quoted Postgres string literal, doubling embedded quotes.
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// adoptTenantIDSQL re-keys every tenant-scoped row from oldID onto newID in one transaction.
//
// Columns are discovered from the live catalog rather than hard-coded, using two sources:
// any column literally named tenant_id (most projections carry no FK at all), plus any column
// with a real FK to tenants(id) — Ent names one-to-one edge columns after the edge, not the
// entity, so a name-only sweep misses those and the final DELETE then fails on the FK.
//
// The clone lands on a throwaway slug so the unique(slug) index does not fire mid-transaction;
// the real slug is restored once the stale row is gone. Where a unique index makes a rewrite
// impossible (a row already exists under the new UUID with the same key — per-tenant sync
// bookkeeping), the stale duplicate is dropped instead.
const adoptTenantIDSQL = `
DO $$
DECLARE
  old_id text := %s;
  new_id text := %s;
  slug_v text := %s;
  r record;
  cols text;
  vals text;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM tenants WHERE id::text = old_id) THEN RETURN; END IF;

  IF NOT EXISTS (SELECT 1 FROM tenants WHERE id::text = new_id) THEN
    SELECT string_agg(quote_ident(column_name), ', ' ORDER BY ordinal_position) INTO cols
      FROM information_schema.columns
     WHERE table_schema='public' AND table_name='tenants';
    SELECT string_agg(CASE WHEN column_name='id'   THEN quote_literal(new_id)
                           WHEN column_name='slug' THEN quote_literal('__drift_migrating__')
                           ELSE quote_ident(column_name) END, ', ' ORDER BY ordinal_position) INTO vals
      FROM information_schema.columns
     WHERE table_schema='public' AND table_name='tenants';
    EXECUTE format('INSERT INTO tenants (%%s) SELECT %%s FROM tenants WHERE id::text = %%L', cols, vals, old_id);
  END IF;

  FOR r IN
    SELECT c.table_name AS tbl, c.column_name AS col
      FROM information_schema.columns c
      JOIN information_schema.tables t
        ON t.table_schema=c.table_schema AND t.table_name=c.table_name AND t.table_type='BASE TABLE'
     WHERE c.table_schema='public' AND c.column_name='tenant_id'
    UNION
    SELECT con.conrelid::regclass::text, a.attname
      FROM pg_constraint con
      JOIN unnest(con.conkey) k ON true
      JOIN pg_attribute a ON a.attrelid=con.conrelid AND a.attnum=k
     WHERE con.confrelid='tenants'::regclass AND con.contype='f'
  LOOP
    BEGIN
      EXECUTE format('UPDATE %%I SET %%I = %%L WHERE %%I = %%L', r.tbl, r.col, new_id, r.col, old_id);
    EXCEPTION WHEN unique_violation OR foreign_key_violation THEN
      EXECUTE format('DELETE FROM %%I WHERE %%I = %%L', r.tbl, r.col, old_id);
      RAISE WARNING 'tenant drift: dropped stale rows from %% (collided under new UUID)', r.tbl;
    END;
  END LOOP;

  EXECUTE format('DELETE FROM tenants WHERE id::text = %%L', old_id);
  EXECUTE format('UPDATE tenants SET slug = %%L WHERE id::text = %%L', slug_v, new_id);
END $$;`

// adoptAuthTenantID re-keys the locally projected tenant onto the UUID auth-api reports,
// then returns that UUID. When no raw DB handle is wired up it degrades to a loud warning
// and keeps returning the local UUID — a partial rewrite would be worse than none.
func (s *Syncer) adoptAuthTenantID(ctx context.Context, localID, remoteID uuid.UUID, slug, name, timezone string) (uuid.UUID, error) {
	if s.db == nil {
		log.Printf("  [tenant-sync] cannot self-heal drift for %s: no DB handle wired (still using local %s)", slug, localID)
		return localID, nil
	}

	// A DO block takes no bind parameters, so the three values are inlined as quoted SQL
	// literals. localID/remoteID are already-parsed UUIDs; slug is quoted defensively.
	stmt := fmt.Sprintf(adoptTenantIDSQL,
		sqlLiteral(localID.String()), sqlLiteral(remoteID.String()), sqlLiteral(slug))
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		log.Printf("  [tenant-sync] self-heal for %s FAILED (still using local %s): %v", slug, localID, err)
		return localID, nil
	}

	upd := s.client.Tenant.UpdateOneID(remoteID).SetSyncStatus("synced").SetLastSyncAt(time.Now())
	if n := strings.TrimSpace(name); n != "" {
		upd = upd.SetName(n)
	}
	if tz := strings.TrimSpace(timezone); tz != "" {
		upd = upd.SetTimezone(tz)
	}
	if err := upd.Exec(ctx); err != nil {
		log.Printf("  [tenant-sync] post-heal metadata refresh for %s failed: %v", slug, err)
	}

	log.Printf("  [tenant-sync] self-healed %s: %s -> %s", slug, localID, remoteID)
	return remoteID, nil
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
			// Drift guard: auth-api owns the tenant UUID. If the tenant is deleted and
			// recreated upstream it comes back with a NEW UUID, and returning the locally
			// cached one silently forks the tenant — seed/slug-resolved writes land under
			// the stale UUID while event-driven writes land under auth's, so PIN login and
			// every other tenant-scoped read miss their own data. Report the mismatch and
			// let auth win rather than pinning the stale ID forever.
			if remoteID, parseErr := uuid.Parse(strings.TrimSpace(td.ID)); parseErr == nil && remoteID != uuid.Nil && remoteID != existing.ID {
				log.Printf("  [tenant-sync] DRIFT: %s is %s locally but %s in auth-api — adopting auth's UUID", slug, existing.ID, remoteID)
				return s.adoptAuthTenantID(ctx, existing.ID, remoteID, slug, td.Name, td.Timezone)
			}
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
	ID       string         `json:"id"`
	Code     string         `json:"code"`
	Name     string         `json:"name"`
	UseCase  string         `json:"use_case"`
	IsHQ     bool           `json:"is_hq"`
	Status   string         `json:"status"`
	Address  string         `json:"address,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// addressJSONFrom builds the outlet mirror's address_json ("street" + "contact_phones" +
// "contact_email") from auth-api's plain address string + metadata blob — the same shape the
// NATS event path writes (identity.outletAddressJSON), so an outlet synced via this REST pull
// (fresh DB, no event backlog yet) isn't missing address/contact info until the next
// auth.outlet.updated event arrives.
func addressJSONFrom(address string, meta map[string]any) map[string]any {
	if address == "" && len(meta) == 0 {
		return nil
	}
	out := map[string]any{}
	if address != "" {
		out["street"] = address
	}
	if phones, ok := meta["contact_phones"]; ok {
		out["contact_phones"] = phones
	}
	if email, ok := meta["contact_email"].(string); ok && email != "" {
		out["contact_email"] = email
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
		if addrJSON := addressJSONFrom(item.Address, item.Metadata); addrJSON != nil {
			q = q.SetAddressJSON(addrJSON)
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
