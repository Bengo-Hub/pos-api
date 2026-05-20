package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/bengobox/pos-service/internal/ent"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/google/uuid"
)

// ── OutletSetting toggle cache ────────────────────────────────────────────────

type settingCacheEntry struct {
	setting   *ent.OutletSetting
	fetchedAt time.Time
}

var (
	settingCacheMu sync.RWMutex
	settingCache   = make(map[uuid.UUID]settingCacheEntry)
	settingCacheTTL = 5 * time.Minute
)

func getOutletSetting(ctx context.Context, client *ent.Client, outletID uuid.UUID) *ent.OutletSetting {
	settingCacheMu.RLock()
	entry, ok := settingCache[outletID]
	settingCacheMu.RUnlock()
	if ok && time.Since(entry.fetchedAt) < settingCacheTTL {
		return entry.setting
	}

	s, err := client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Only(ctx)
	if err != nil {
		return nil
	}

	settingCacheMu.Lock()
	settingCache[outletID] = settingCacheEntry{setting: s, fetchedAt: time.Now()}
	settingCacheMu.Unlock()
	return s
}

// RequireKDSEnabled gates routes to outlets that have enable_kds=true in their
// OutletSetting. Must be used alongside RequireUseCase for full gating.
func RequireKDSEnabled(client *ent.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			outlet := OutletFromContext(r.Context())
			if outlet == nil {
				next.ServeHTTP(w, r)
				return
			}
			setting := getOutletSetting(r.Context(), client, outlet.ID)
			if setting != nil && !setting.EnableKds {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":   "kds_disabled",
					"message": "Kitchen Display System is not enabled for this outlet",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAppointmentsEnabled gates routes to outlets that have enable_appointments=true.
func RequireAppointmentsEnabled(client *ent.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			outlet := OutletFromContext(r.Context())
			if outlet == nil {
				next.ServeHTTP(w, r)
				return
			}
			setting := getOutletSetting(r.Context(), client, outlet.ID)
			if setting != nil && !setting.EnableAppointments {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":   "appointments_disabled",
					"message": "Appointment booking is not enabled for this outlet",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireUseCase gates a route to outlets whose use_case matches one of the
// allowed values. The outlet must already be resolved by OutletContextMiddleware.
//
// Usage in router:
//
//	r.With(mw.RequireUseCase("hospitality")).Get("/tables", ...)
//	r.With(mw.RequireUseCase("hospitality", "quick_service")).Mount("/kds", ...)
func RequireUseCase(allowed ...string) func(http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, uc := range allowed {
		set[uc] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			outlet := OutletFromContext(r.Context())
			if outlet == nil || outlet.UseCase == "" {
				// No outlet context — let RBAC/auth handle it
				next.ServeHTTP(w, r)
				return
			}
			if set[outlet.UseCase] {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":    "feature_not_available",
				"message":  "this feature is not available for your outlet type",
				"use_case": outlet.UseCase,
			})
		})
	}
}
