package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
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

// IPRateLimit is a Redis sliding-window rate limiter keyed by client IP.
// It returns 429 Too Many Requests with standard Retry-After and X-RateLimit-* headers.
func IPRateLimit(rc *redis.Client, cfg RateLimitConfig) func(http.Handler) http.Handler {
	if rc == nil {
		// No Redis — pass through (allow all). Warn via no-op so callers can detect this.
		return func(next http.Handler) http.Handler { return next }
	}
	if cfg.Requests <= 0 {
		cfg = DefaultRateLimitConfig()
	}

	return func(next http.Handler) http.Handler {
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

			ip := r.RemoteAddr
			// Prefer the real IP set by middleware.RealIP (stored in RemoteAddr after httpware).
			if forwarded := r.Header.Get("X-Real-IP"); forwarded != "" {
				ip = forwarded
			}

			windowSec := int(cfg.Window.Seconds())
			key := fmt.Sprintf("rl:pos:%s:%d", ip, time.Now().Unix()/int64(windowSec))

			ctx := context.Background()
			count, err := rc.Incr(ctx, key).Result()
			if err == nil && count == 1 {
				rc.Expire(ctx, key, cfg.Window+time.Second)
			}

			remaining := cfg.Requests - int(count)
			if remaining < 0 {
				remaining = 0
			}
			reset := (time.Now().Unix()/int64(windowSec)+1)*int64(windowSec) - time.Now().Unix()

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(cfg.Requests))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))

			if err == nil && int(count) > cfg.Requests {
				w.Header().Set("Retry-After", strconv.FormatInt(reset, 10))
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
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
// authentication routes (identify/login/step-up). It uses its own Redis key namespace ("rl:pin:"
// vs IPRateLimit's "rl:pos:") so it counts independently of — and stacks with — the general
// per-IP limiter rather than sharing/racing its counter.
//
// Why this exists: PIN uniqueness is enforced per-tenant, not globally, so the SAME weak/common
// PIN (e.g. "1111") can independently be valid for two entirely unrelated tenants' admin
// accounts — confirmed live in production. Without a much stricter limit specifically on these
// routes, the generous general-purpose budget (100 req/60s) lets an attacker who knows or
// guesses one tenant's PIN cheaply try it against hundreds of other tenant IDs per minute,
// looking for a collision. This does not fix any single request's tenant isolation (each lookup
// was already correctly scoped to its own tenant) — it makes that scanning pattern impractical.
func PINRateLimit(rc *redis.Client, cfg RateLimitConfig) func(http.Handler) http.Handler {
	if rc == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if cfg.Requests <= 0 {
		cfg = DefaultPINRateLimitConfig()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := r.RemoteAddr
			if forwarded := r.Header.Get("X-Real-IP"); forwarded != "" {
				ip = forwarded
			}

			windowSec := int(cfg.Window.Seconds())
			key := fmt.Sprintf("rl:pin:%s:%d", ip, time.Now().Unix()/int64(windowSec))

			ctx := context.Background()
			count, err := rc.Incr(ctx, key).Result()
			if err == nil && count == 1 {
				rc.Expire(ctx, key, cfg.Window+time.Second)
			}

			reset := (time.Now().Unix()/int64(windowSec)+1)*int64(windowSec) - time.Now().Unix()

			if err == nil && int(count) > cfg.Requests {
				w.Header().Set("Retry-After", strconv.FormatInt(reset, 10))
				http.Error(w, `{"error":"too many PIN login attempts — please wait and try again"}`, http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
