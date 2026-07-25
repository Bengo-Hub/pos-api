// Package providerfooter resolves whether the platform-owner ("Developed & maintained by
// CodeVertex...") advertisement footer should render on this tenant's generated documents.
// Configuration lives in the existing ServiceConfig key/value store (see
// internal/ent/schema/serviceconfig.go): a platform-level default (tenant_id IS NULL) plus
// optional per-tenant overrides — the SAME table + precedence pattern already proven by
// treasury-api's backup/destination.Store.Resolve. Unlike that package this value is a plain,
// non-secret bool, so there is no encryption to do.
package providerfooter

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/serviceconfig"
)

// ConfigKey is the ServiceConfig.config_key the enabled/disabled flag is stored under, at both
// platform (tenant_id NULL) and tenant scope.
const ConfigKey = "provider_footer_enabled"

// Resolve reports whether the provider footer should render for tenantID: an explicit tenant
// override wins if present (true or false), else the platform default (tenant_id IS NULL) wins
// if present, else the footer defaults ON — it only stops showing once someone (platform owner)
// explicitly turns it off, per-tenant or platform-wide.
func Resolve(ctx context.Context, client *ent.Client, tenantID uuid.UUID) bool {
	if client == nil {
		return true
	}
	if tenantID != uuid.Nil {
		if v, ok := load(ctx, client, &tenantID); ok {
			return v
		}
	}
	if v, ok := load(ctx, client, nil); ok {
		return v
	}
	return true
}

// load reads the stored bool at the given scope (nil tenantID = platform default). ok=false when
// no row exists at that scope or it's malformed (never panics).
func load(ctx context.Context, client *ent.Client, tenantID *uuid.UUID) (bool, bool) {
	q := client.ServiceConfig.Query().Where(serviceconfig.ConfigKey(ConfigKey))
	if tenantID == nil {
		q = q.Where(serviceconfig.TenantIDIsNil())
	} else {
		q = q.Where(serviceconfig.TenantID(*tenantID))
	}
	row, err := q.First(ctx)
	if err != nil {
		return false, false
	}
	return row.ConfigValue == "true", true
}

// Set upserts the enabled flag at the given scope (tenantID nil = platform default).
func Set(ctx context.Context, client *ent.Client, tenantID *uuid.UUID, enabled bool, log *zap.Logger) error {
	value := "false"
	if enabled {
		value = "true"
	}
	q := client.ServiceConfig.Query().Where(serviceconfig.ConfigKey(ConfigKey))
	if tenantID == nil {
		q = q.Where(serviceconfig.TenantIDIsNil())
	} else {
		q = q.Where(serviceconfig.TenantID(*tenantID))
	}
	existing, err := q.First(ctx)
	if err == nil {
		_, uErr := existing.Update().
			SetConfigValue(value).
			SetConfigType("bool").
			SetDescription("Show the CodeVertex platform advertisement footer on generated documents").
			Save(ctx)
		return uErr
	}
	create := client.ServiceConfig.Create().
		SetConfigKey(ConfigKey).
		SetConfigValue(value).
		SetConfigType("bool").
		SetDescription("Show the CodeVertex platform advertisement footer on generated documents")
	if tenantID != nil {
		create = create.SetTenantID(*tenantID)
	}
	_, cErr := create.Save(ctx)
	return cErr
}

// Clear removes the override at the given scope so it falls back to inheriting from platform
// default (tenant scope) or the hardcoded true default (platform scope).
func Clear(ctx context.Context, client *ent.Client, tenantID *uuid.UUID) error {
	del := client.ServiceConfig.Delete().Where(serviceconfig.ConfigKey(ConfigKey))
	if tenantID == nil {
		del = del.Where(serviceconfig.TenantIDIsNil())
	} else {
		del = del.Where(serviceconfig.TenantID(*tenantID))
	}
	_, err := del.Exec(ctx)
	return err
}
