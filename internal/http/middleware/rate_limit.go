package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	sharedratelimit "github.com/Bengo-Hub/shared-ratelimit"
)

// RateLimitConfig holds defaults for the sliding-window rate limiter.
type RateLimitConfig struct {
	// Requests allowed per Window per unique IP.
	Requests int
	Window   time.Duration
}

// DefaultRateLimitConfig returns sensible production defaults (100 req / 60s per IP).
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{Requests: 100, Window: 60 * time.Second}
}

// IPRateLimit is a Redis sliding-window rate limiter keyed by client IP, built on
// github.com/Bengo-Hub/shared-ratelimit's Limiter (extracted from treasury-api/notifications-api;
// same ZSET sliding-window algorithm and fail-open-on-Redis-error posture pos-api's own
// hand-rolled fixed-window counter approximated less precisely). Returns 429 with standard
// Retry-After and X-RateLimit-* headers.
func IPRateLimit(rc *redis.Client, log *zap.Logger, cfg RateLimitConfig) func(http.Handler) http.Handler {
	if rc == nil {
		// No Redis — pass through (allow all). Warn via no-op so callers can detect this.
		return func(next http.Handler) http.Handler { return next }
	}
	if cfg.Requests <= 0 {
		cfg = DefaultRateLimitConfig()
	}

	limiter := sharedratelimit.NewLimiter(rc, log, "pos")
	limited := limiter.Middleware(sharedratelimit.IPKey, cfg.Requests, cfg.Window)

	return func(next http.Handler) http.Handler {
		wrapped := limited(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Exempt long-lived SSE streams and the lightweight payment-status poll from the per-IP
			// counter: a stream holds one connection but the browser reconnects on the 30s request
			// timeout, and a bounded payment poll fires a handful of times — counting either against
			// the shared 100/60s budget starves normal POS traffic and 429-storms the very endpoint
			// the cashier is waiting on. Confirmation correctness is owned by the treasury NATS
			// subscriber, not these endpoints.
			// /catalog/version is the terminal's tiny catalog-freshness poll (two aggregates on an
			// indexed column, fired every ~45s per terminal) — same starvation math as payment-status.
			if p := r.URL.Path; strings.HasSuffix(p, "/stream") || strings.HasSuffix(p, "/payment-status") || strings.HasSuffix(p, "/catalog/version") {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

// DefaultPINRateLimitConfig throttles PIN identify/login attempts much harder than ordinary
// traffic (see PINRateLimit) — 8 requests/minute per IP. Generous enough for a cashier fumbling
// their own PIN a few times, far too tight to scan a common/guessed PIN across many tenant IDs.
func DefaultPINRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{Requests: 8, Window: 60 * time.Second}
}

// PINRateLimit is a Redis sliding-window rate limiter keyed by client IP, dedicated to PIN
// authentication routes (identify/login/step-up), built on shared-ratelimit's Limiter. It uses
// its own Redis key namespace ("rl:pos-pin:" vs IPRateLimit's "rl:pos:") so it counts
// independently of — and stacks with — the general per-IP limiter rather than sharing/racing its
// counter.
//
// Why this exists: PIN uniqueness is enforced per-tenant, not globally, so the SAME weak/common
// PIN (e.g. "1111") can independently be valid for two entirely unrelated tenants' admin
// accounts — confirmed live in production. Without a much stricter limit specifically on these
// routes, the generous general-purpose budget (100 req/60s) lets an attacker who knows or
// guesses one tenant's PIN cheaply try it against hundreds of other tenant IDs per minute,
// looking for a collision. This does not fix any single request's tenant isolation (each lookup
// was already correctly scoped to its own tenant) — it makes that scanning pattern impractical.
func PINRateLimit(rc *redis.Client, log *zap.Logger, cfg RateLimitConfig) func(http.Handler) http.Handler {
	if rc == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if cfg.Requests <= 0 {
		cfg = DefaultPINRateLimitConfig()
	}

	limiter := sharedratelimit.NewLimiter(rc, log, "pos-pin")
	return limiter.Middleware(sharedratelimit.IPKey, cfg.Requests, cfg.Window)
}
