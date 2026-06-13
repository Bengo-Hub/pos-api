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
			if p := r.URL.Path; strings.HasSuffix(p, "/stream") || strings.HasSuffix(p, "/payment-status") {
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
