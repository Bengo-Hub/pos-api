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
	upgradeURL      = "/subscription/upgrade"
)

func SubscriptionGate() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := authclient.ClaimsFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			if claims.IsPlatformOwner || claims.IsSuperuser() {
				next.ServeHTTP(w, r)
				return
			}

			switch claims.SubscriptionStatus {
			case "ACTIVE", "TRIAL", "":
				next.ServeHTTP(w, r)
				return
			case "EXPIRED":
				if claims.SubscriptionExpires != nil {
					deadline := claims.SubscriptionExpires.Add(gracePeriodDays * 24 * time.Hour)
					if time.Now().Before(deadline) {
						daysLeft := int(time.Until(deadline).Hours()/24) + 1
						w.Header().Set("X-Sub-Grace-Days-Left", fmt.Sprintf("%d", daysLeft))
						next.ServeHTTP(w, r)
						return
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

func RequireFeature(featureCode string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := authclient.ClaimsFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			if claims.IsPlatformOwner || claims.IsSuperuser() {
				next.ServeHTTP(w, r)
				return
			}

			if !claims.HasFeature(featureCode) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":            "feature_not_available",
					"required_feature": featureCode,
					"upgrade_url":      upgradeURL,
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func CheckDeviceLimit(w http.ResponseWriter, r *http.Request, activeDeviceCount int) bool {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		return true
	}
	if claims.IsPlatformOwner || claims.IsSuperuser() {
		return true
	}
	maxDevices := claims.GetLimit("max_devices")
	if maxDevices <= 0 {
		return true
	}
	if activeDeviceCount >= maxDevices {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":       "device_limit_reached",
			"max_devices": maxDevices,
			"upgrade_url": upgradeURL,
		})
		return false
	}
	return true
}

func writeSubscriptionError(w http.ResponseWriter, gracePeriodEnded bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":              "subscription_expired",
		"grace_period_ended": gracePeriodEnded,
		"upgrade_url":        upgradeURL,
	})
}
