package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
)

// defaultTenantTZ is the platform-wide fallback timezone (East Africa Time) used
// whenever a tenant has no timezone projected or the stored value fails to load.
const defaultTenantTZ = "Africa/Nairobi"

// loadLocation resolves an IANA timezone name to a *time.Location, degrading
// gracefully: bad/empty name → Africa/Nairobi → UTC. A report must never fail to
// render because of a malformed timezone string.
func loadLocation(tz string) *time.Location {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		tz = defaultTenantTZ
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	if loc, err := time.LoadLocation(defaultTenantTZ); err == nil {
		return loc
	}
	return time.UTC
}

// tenantLocation resolves the reporting timezone for a tenant from the local
// tenants projection (synced from auth-api, default Africa/Nairobi). Every POS
// report/day-bucketing call site should compute its day boundaries in this zone so
// a Kenyan (or other) tenant's "today"/"this month" match the wall clock, not UTC.
func tenantLocation(ctx context.Context, client *ent.Client, tenantID uuid.UUID) *time.Location {
	tz := defaultTenantTZ
	if client != nil && tenantID != uuid.Nil {
		if t, err := client.Tenant.Query().Where(enttenant.ID(tenantID)).Only(ctx); err == nil && t != nil {
			if strings.TrimSpace(t.Timezone) != "" {
				tz = t.Timezone
			}
		}
	}
	return loadLocation(tz)
}

// requestTenantLocation resolves the tenant timezone for the current request,
// tolerating a missing/invalid tenant context by falling back to the default zone.
func requestTenantLocation(r *http.Request, client *ent.Client) *time.Location {
	tid, err := parseTenantUUID(r)
	if err != nil {
		return loadLocation(defaultTenantTZ)
	}
	return tenantLocation(r.Context(), client, tid)
}

// startOfDayIn returns midnight (00:00:00) of t's calendar day in loc.
func startOfDayIn(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
}

// parseDayStartIn parses a "YYYY-MM-DD" string as midnight of that calendar day in
// loc (not UTC), so a tenant-local day maps to the correct absolute instant.
func parseDayStartIn(s string, loc *time.Location) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", strings.TrimSpace(s), loc)
}
