package middleware

import (
	"context"
	"encoding/json"
	sharedcache "github.com/Bengo-Hub/cache"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/modules/outletpolicy"
	"github.com/google/uuid"
)

// isSuperUserOrPlatformOwner mirrors the bypass already used by RequireServicePermission
// (see permission.go) — superusers/platform owners must always be able to reach a tenant's
// use-case-scoped screens for support, oversight, and configuring on the tenant's behalf,
// exactly like the frontend's hasModule() already assumes (see pos-ui's use-module-access.ts).
// Without this, a superuser browsing an outlet whose use_case/toggle doesn't match got 403'd
// even though the UI showed them the tab.
func isSuperUserOrPlatformOwner(ctx context.Context) bool {
	claims, ok := authclient.ClaimsFromContext(ctx)
	return ok && claims != nil && (claims.IsSuperuser() || claims.IsPlatformOwner)
}

// ── OutletSetting toggle cache ────────────────────────────────────────────────

// settingCache is bounded (least recently used outlets drop out) and expiring; writes clear it
// on every replica through cache_invalidation.go.
var settingCache = sharedcache.NewLocal[uuid.UUID, *ent.OutletSetting](5000, 5*time.Minute)

func getOutletSetting(ctx context.Context, client *ent.Client, outletID uuid.UUID) *ent.OutletSetting {
	if s, ok := settingCache.Get(outletID); ok {
		return s
	}
	s, err := client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Only(ctx)
	if err != nil {
		return nil
	}
	settingCache.Set(outletID, s)
	return s
}

// InvalidateOutletSetting drops the cached settings row for an outlet on every replica, so a
// module toggle or a service-profile change takes effect immediately everywhere.
func InvalidateOutletSetting(outletID uuid.UUID) {
	invalidate("outlet-setting", outletID, func() { settingCache.Delete(outletID) })
}

// RequireKDSEnabled gates routes to outlets that have enable_kds=true in their
// OutletSetting. Must be used alongside RequireUseCase for full gating.
func RequireKDSEnabled(client *ent.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSuperUserOrPlatformOwner(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}
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
			if isSuperUserOrPlatformOwner(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}
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
			if isSuperUserOrPlatformOwner(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}
			outlet := OutletFromContext(r.Context())
			if outlet == nil || outlet.UseCase == "" {
				// No outlet context — let RBAC/auth handle it
				next.ServeHTTP(w, r)
				return
			}
			// Compare the normalized profile, not the raw string: an outlet stored as "salon",
			// "hotel" or "Services" must reach the same routes its terminal already renders
			// (pos-ui normalizes the same way).
			if set[outlet.UseCase] || set[outletpolicy.NormalizeUseCase(outlet.UseCase)] {
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

// GateUnlessUseCase applies gate only to outlets whose normalized use case is NOT in exempt.
// The KDS routes use it so a services outlet's production board is not locked behind the
// hospitality "kds" plan feature: the services board is gated by the outlet's own
// enable_kds toggle and use case instead.
func GateUnlessUseCase(gate func(http.Handler) http.Handler, exempt ...string) func(http.Handler) http.Handler {
	skip := make(map[string]bool, len(exempt))
	for _, uc := range exempt {
		skip[uc] = true
	}
	return func(next http.Handler) http.Handler {
		gated := gate(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if outlet := OutletFromContext(r.Context()); outlet != nil &&
				skip[outletpolicy.NormalizeUseCase(outlet.UseCase)] {
				next.ServeHTTP(w, r)
				return
			}
			gated.ServeHTTP(w, r)
		})
	}
}
