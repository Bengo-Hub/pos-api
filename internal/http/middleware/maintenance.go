package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/google/uuid"
)

// UnderMaintenance reports whether t's scheduled maintenance window ("Repair Mode" in the
// product UI, internally named maintenance to avoid confusion with the RepairJob device-repair
// ticket module) covers now. Both bounds must be set for a window to be active — a half-set
// window (only a start or only an end) never blocks anything, so a scheduling mistake fails open
// rather than locking a tenant out indefinitely by accident.
func UnderMaintenance(t *ent.Tenant, now time.Time) bool {
	if t == nil || t.MaintenanceStartsAt == nil || t.MaintenanceEndsAt == nil {
		return false
	}
	return !now.Before(*t.MaintenanceStartsAt) && !now.After(*t.MaintenanceEndsAt)
}

// ── short-lived tenant cache for the maintenance check ─────────────────────────
// Mirrors use_case.go's getOutletSetting pattern but with a much shorter TTL: this gate is meant
// to take effect promptly on activation/expiry, not just eventually. InvalidateMaintenanceCache
// lets the admin toggle handler make its own write visible immediately instead of waiting out
// the TTL.

type maintenanceCacheEntry struct {
	tenant    *ent.Tenant
	fetchedAt time.Time
}

var (
	maintenanceCacheMu  sync.RWMutex
	maintenanceCache    = make(map[uuid.UUID]maintenanceCacheEntry)
	maintenanceCacheTTL = 15 * time.Second
)

// InvalidateMaintenanceCache evicts a tenant's cached row so the very next request re-reads the
// DB. Call this right after writing a new maintenance window so activation/cancellation is
// visible immediately rather than after up to maintenanceCacheTTL.
func InvalidateMaintenanceCache(tenantID uuid.UUID) {
	maintenanceCacheMu.Lock()
	delete(maintenanceCache, tenantID)
	maintenanceCacheMu.Unlock()
}

func getTenantForMaintenanceCheck(ctx context.Context, client *ent.Client, tenantID uuid.UUID) *ent.Tenant {
	maintenanceCacheMu.RLock()
	entry, ok := maintenanceCache[tenantID]
	maintenanceCacheMu.RUnlock()
	if ok && time.Since(entry.fetchedAt) < maintenanceCacheTTL {
		return entry.tenant
	}

	t, err := client.Tenant.Get(ctx, tenantID)
	if err != nil {
		return nil
	}

	maintenanceCacheMu.Lock()
	maintenanceCache[tenantID] = maintenanceCacheEntry{tenant: t, fetchedAt: time.Now()}
	maintenanceCacheMu.Unlock()
	return t
}

// RequireNotUnderMaintenance blocks every request for a tenant currently inside a scheduled
// maintenance window, for everyone except platform owners (who need full access to actually do
// the repair work). Mounted tenant-wide in router.go's protected route chain, ahead of the
// per-route RBAC/use-case gates, so a maintenance window always wins over anything else.
//
// Deliberately checks claims.TenantUUID() (the JWT's own tenant claim) rather than depending on
// TenantV2/OutletContextMiddleware having already run — this is meant to sit earlier in the
// chain than either, so a maintenance block never depends on outlet resolution succeeding first.
func RequireNotUnderMaintenance(client *ent.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := authclient.ClaimsFromContext(r.Context())
			if !ok || claims == nil {
				next.ServeHTTP(w, r)
				return
			}
			if claims.IsPlatformOwner {
				next.ServeHTTP(w, r)
				return
			}
			tid, err := claims.TenantUUID()
			if err != nil || tid == nil {
				next.ServeHTTP(w, r)
				return
			}

			t := getTenantForMaintenanceCheck(r.Context(), client, *tid)
			if !UnderMaintenance(t, time.Now()) {
				next.ServeHTTP(w, r)
				return
			}
			writeMaintenanceError(w, t)
		})
	}
}

func writeMaintenanceError(w http.ResponseWriter, t *ent.Tenant) {
	reason := ""
	if t.MaintenanceReason != nil {
		reason = *t.MaintenanceReason
	}
	var endsAt string
	if t.MaintenanceEndsAt != nil {
		endsAt = t.MaintenanceEndsAt.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    "tenant_under_repair",
		"error":   "tenant_under_repair",
		"message": "This system is temporarily under maintenance. Please try again later.",
		"reason":  reason,
		"ends_at": endsAt,
	})
}
