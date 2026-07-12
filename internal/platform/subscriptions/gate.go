package subscriptions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

const (
	gracePeriodDays = 7
	upgradeURL      = "/settings?tab=subscription"
)

// exempt reports whether the request's token bypasses all subscription gating
// (platform owners, explicitly subscription-exempt tenants, demo tenants, and
// service-charge tenants). Tenant superusers are NOT exempt (auth-client v0.10.0 / SEC-3).
// Delegates to the shared claims helper so every gate path stays consistent.
func exempt(r *http.Request) bool {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		return true // no claims (e.g. S2S/pin paths) — don't block here
	}
	return claims.IsGatingExempt()
}

func SubscriptionGate() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := authclient.ClaimsFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			if claims.IsGatingExempt() {
				next.ServeHTTP(w, r)
				return
			}

			switch claims.SubscriptionStatus {
			case "ACTIVE", "TRIAL", "":
				next.ServeHTTP(w, r)
				return
			case "EXPIRED":
				if claims.SubscriptionExpires != nil {
					expAt := claims.ExpiresAt()
					if expAt != nil {
						deadline := expAt.Add(gracePeriodDays * 24 * time.Hour)
						if time.Now().Before(deadline) {
							daysLeft := int(time.Until(deadline).Hours()/24) + 1
							w.Header().Set("X-Sub-Grace-Days-Left", fmt.Sprintf("%d", daysLeft))
							next.ServeHTTP(w, r)
							return
						}
					}
				}
				writeSubscriptionError(w, true)
				return
			default:
				writeSubscriptionError(w, false)
				return
			}
		})
	}
}

// RequireFeature gates a route group on a subscription feature code. Exempt tokens pass.
// The feature code must be a real code seeded by subscription-service (see features.go).
func RequireFeature(featureCode string) func(http.Handler) http.Handler {
	return authclient.RequireFeatureCode(featureCode)
}

// CheckStructuralLimit enforces a hard-block structural cap (devices, tables, cashiers,
// outlets, …) before creating a new resource. It returns true when the request may
// proceed, or writes a structured 402 and returns false when the cap is reached.
//
// `metric` is the UI-facing metric name (e.g. "devices", "tables"); `limitKey` is the
// plan-limit key in the JWT (e.g. "max_devices"). Structural caps are never overage-eligible
// — the limit-reached modal will show an "Upgrade plan" CTA.
func CheckStructuralLimit(w http.ResponseWriter, r *http.Request, metric, limitKey string, currentCount int) bool {
	if exempt(r) {
		return true
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		return true
	}
	limit := claims.GetLimit(limitKey)
	if limit <= 0 {
		return true // 0 = not configured, -1 = unlimited
	}
	if currentCount >= limit {
		writeLimitReached(w, metric, limit, currentCount, false)
		return false
	}
	return true
}

// CheckDeviceLimit is a convenience wrapper around CheckStructuralLimit for max_devices.
func CheckDeviceLimit(w http.ResponseWriter, r *http.Request, activeDeviceCount int) bool {
	return CheckStructuralLimit(w, r, "devices", "max_devices", activeDeviceCount)
}

// writeLimitReached emits the structured 402 the pos-ui LimitReachedModal consumes.
func writeLimitReached(w http.ResponseWriter, metric string, limit, used int, overageEligible bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":             "usage_limit_exceeded",
		"error":            "usage_limit_exceeded",
		"message":          fmt.Sprintf("You've reached your plan's %s limit (%d).", metric, limit),
		"metric":           metric,
		"limit":            limit,
		"used":             used,
		"overage_eligible": overageEligible,
		"upgrade_url":      upgradeURL,
	})
}

func writeSubscriptionError(w http.ResponseWriter, gracePeriodEnded bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":               "subscription_expired",
		"error":              "subscription_expired",
		"grace_period_ended": gracePeriodEnded,
		"upgrade_url":        upgradeURL,
	})
}
