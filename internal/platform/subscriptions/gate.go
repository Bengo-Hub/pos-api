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

// isReadMethod reports whether r is a read (GET/HEAD/OPTIONS) — always allowed during grace,
// even for an otherwise-gated tenant, so a tenant in grace can still see their data.
func isReadMethod(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
}

// graceOutcome is the result of comparing "now" against the tenant's period_end directly,
// independent of the sub_status claim.
type graceOutcome int

const (
	// graceNone: not past period_end (or no period_end known) — fully normal.
	graceNone graceOutcome = iota
	// graceActive: past period_end but within the grace deadline — reads pass, writes blocked.
	graceActive
	// graceEnded: past the grace deadline entirely — fully blocked.
	graceEnded
)

// graceStateOf compares "now" to claims.SubscriptionExpires (sub_expires) directly, NEVER via
// claims.SubscriptionStatus. subscriptions-api's expiry job (internal/jobs/renewal.go) keeps
// tenant_subscription.status == ACTIVE for the entire 7-day post-expiry grace window by design
// (only metadata.grace_until is set) and flips it to EXPIRED only once grace is fully exhausted.
// So a switch on SubscriptionStatus alone can never observe "in grace" — that state IS "ACTIVE"
// as far as the claim is concerned. This was the actual bug behind a paid-looking order going
// through during a tenant's grace window: the write-block was written into a `case "EXPIRED":`
// branch that grace never reaches.
func graceStateOf(claims *authclient.Claims) (outcome graceOutcome, daysLeft int) {
	if claims.SubscriptionExpires == nil {
		return graceNone, 0
	}
	expAt := claims.ExpiresAt()
	now := time.Now()
	if expAt == nil || !now.After(*expAt) {
		return graceNone, 0
	}
	deadline := expAt.Add(gracePeriodDays * 24 * time.Hour)
	if now.Before(deadline) {
		return graceActive, int(time.Until(deadline).Hours()/24) + 1
	}
	return graceEnded, 0
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
				// TRIAL/"" have their own separate expiry handling and never carry the post-
				// payment grace semantics, so only re-check ACTIVE against period_end directly.
				if claims.SubscriptionStatus != "ACTIVE" {
					next.ServeHTTP(w, r)
					return
				}
				switch outcome, daysLeft := graceStateOf(claims); outcome {
				case graceActive:
					w.Header().Set("X-Sub-Grace-Days-Left", fmt.Sprintf("%d", daysLeft))
					// Grace keeps the tenant readable but blocks mutations — matching what the
					// pos-ui grace banner has always told tenants ("Write operations (create,
					// edit, delete) are currently restricted"), which used to be unenforced
					// everywhere (frontend and backend both let every write through during
					// grace, silently contradicting the banner).
					if isReadMethod(r) {
						next.ServeHTTP(w, r)
						return
					}
					writeGraceWriteBlocked(w, daysLeft)
					return
				case graceEnded:
					// The claim still says ACTIVE (stale token, or the expiry job hasn't run its
					// hourly pass yet) but we're independently certain grace is over — don't
					// trust a stale claim to keep a lapsed tenant writing indefinitely.
					writeSubscriptionError(w, true)
					return
				default:
					next.ServeHTTP(w, r)
					return
				}
			case "EXPIRED":
				if outcome, daysLeft := graceStateOf(claims); outcome == graceActive {
					w.Header().Set("X-Sub-Grace-Days-Left", fmt.Sprintf("%d", daysLeft))
					if isReadMethod(r) {
						next.ServeHTTP(w, r)
						return
					}
					writeGraceWriteBlocked(w, daysLeft)
					return
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

// writeGraceWriteBlocked is the structured 402 the pos-ui error handler shows as a blocking
// "renew to continue" modal for a mutating request made while the tenant is in its post-expiry
// grace window. Reads are never blocked (see isReadMethod).
func writeGraceWriteBlocked(w http.ResponseWriter, daysLeft int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":            "subscription_grace_write_blocked",
		"error":           "subscription_grace_write_blocked",
		"message":         fmt.Sprintf("Your subscription expired. You have %d day(s) left to renew — creating, editing, and deleting is disabled until you renew.", daysLeft),
		"grace_days_left": daysLeft,
		"upgrade_url":     upgradeURL,
	})
}
