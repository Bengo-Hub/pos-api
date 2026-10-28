package middleware

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/idempotencykey"
)

// idempotencyTTL is how long a stored response is replayable. Wider than Stripe's 24h
// because a POS terminal can be offline for several days; the offline-sync worker may
// not replay until reconnect.
const idempotencyTTL = 72 * time.Hour

// staleInFlightAfter is how long an in_flight claim may stand before a retry takes it over. A
// request finishes or is removed well inside this; a claim this old was left by a process that
// died mid-request, and leaving it would answer 409 to every retry for 72 hours.
const staleInFlightAfter = 2 * time.Minute

// captureWriter buffers the handler's response so it can be persisted for replay.
type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}

// Idempotency makes mutating requests replay-safe. When a request carries an
// "Idempotency-Key" header it:
//   - on first sight: records an in_flight row, runs the handler, then stores the
//     response (only for cacheable statuses) for future replays;
//   - on a replay of a completed key: returns the stored response without re-running;
//   - on a replay while the first is still in_flight: returns 409 (client retries later).
//
// Requests without the header pass straight through, so this is a no-op for normal
// online traffic and only engages for the offline-sync worker (which always sends a key).
func Idempotency(client *ent.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if key == "" || client == nil {
				next.ServeHTTP(w, r)
				return
			}

			tenantStr := httpware.GetTenantID(r.Context())
			tid, err := uuid.Parse(tenantStr)
			if err != nil {
				// No tenant context — can't scope the key safely; just run normally.
				next.ServeHTTP(w, r)
				return
			}
			// The key store writes outlive the client's connection: an offline-sync client that drops
			// mid-request must still leave the row completed (or removed), never stuck in_flight.
			ctx := context.WithoutCancel(r.Context())
			endpoint := r.Method + " " + r.URL.Path
			claim := func() error {
				_, err := client.IdempotencyKey.Create().
					SetTenantID(tid).
					SetKey(key).
					SetEndpoint(endpoint).
					SetStatus("in_flight").
					SetExpiresAt(time.Now().Add(idempotencyTTL)).
					Save(ctx)
				return err
			}

			// Try to claim the key. The unique (tenant_id, key) index makes this atomic.
			err = claim()
			if err != nil && ent.IsConstraintError(err) {
				// A claim left in_flight by a process that died mid-request is taken over.
				if n, derr := client.IdempotencyKey.Delete().
					Where(idempotencykey.TenantID(tid), idempotencykey.Key(key),
						idempotencykey.Status("in_flight"),
						idempotencykey.CreatedAtLT(time.Now().Add(-staleInFlightAfter))).
					Exec(ctx); derr == nil && n > 0 {
					err = claim()
				}
			}
			if err != nil {
				if !ent.IsConstraintError(err) {
					// The key store is unreachable. Running the request without it could create the
					// same sale twice, so refuse with a retryable 503; offline sync retries it.
					w.Header().Set("Retry-After", "5")
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{
						"error": "duplicate protection unavailable, retry shortly",
					})
					return
				}
				// Key already exists: this is a replay.
				existing, qerr := client.IdempotencyKey.Query().
					Where(idempotencykey.TenantID(tid), idempotencykey.Key(key)).
					Only(ctx)
				if qerr != nil {
					w.Header().Set("Retry-After", "5")
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{
						"error": "duplicate protection unavailable, retry shortly",
					})
					return
				}
				if existing.Endpoint != endpoint {
					// One key must not replay another endpoint's stored response.
					writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
						"error": "Idempotency-Key already used for a different request",
					})
					return
				}
				if existing.Status != "completed" {
					// Original is still running — tell the client to retry later.
					writeJSON(w, http.StatusConflict, map[string]any{
						"error":             "request already in progress",
						"idempotency_state": "in_flight",
					})
					return
				}
				// Replay the stored response verbatim.
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replayed", "true")
				code := existing.ResponseCode
				if code == 0 {
					code = http.StatusOK
				}
				w.WriteHeader(code)
				_, _ = w.Write(existing.ResponseBody)
				return
			}

			// First execution — capture the response.
			cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
			completed := false
			// If the handler panics (recovered upstream), drop the in_flight row so the client's
			// retry isn't 409'd forever — the panic surfaces as a retryable 5xx.
			defer func() {
				if !completed {
					_, _ = client.IdempotencyKey.Delete().
						Where(idempotencykey.TenantID(tid), idempotencykey.Key(key)).
						Exec(ctx)
				}
			}()
			next.ServeHTTP(cw, r)
			completed = true

			// Cache 2xx and client-error 4xx (terminal — replaying won't change the outcome),
			// but never 5xx: a transient server error must be retryable by the client.
			if cw.status >= 500 {
				_, _ = client.IdempotencyKey.Delete().
					Where(idempotencykey.TenantID(tid), idempotencykey.Key(key)).
					Exec(ctx)
				return
			}
			_, _ = client.IdempotencyKey.Update().
				Where(idempotencykey.TenantID(tid), idempotencykey.Key(key)).
				SetStatus("completed").
				SetResponseCode(cw.status).
				SetResponseBody(cw.body.Bytes()).
				Save(ctx)
		})
	}
}
