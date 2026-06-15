package middleware

import (
	"bytes"
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
			ctx := r.Context()
			endpoint := r.Method + " " + r.URL.Path

			// Try to claim the key. The unique (tenant_id, key) index makes this atomic.
			_, err = client.IdempotencyKey.Create().
				SetTenantID(tid).
				SetKey(key).
				SetEndpoint(endpoint).
				SetStatus("in_flight").
				SetExpiresAt(time.Now().Add(idempotencyTTL)).
				Save(ctx)
			if err != nil {
				if !ent.IsConstraintError(err) {
					// Unexpected DB error — fail open so a key-store outage can't block sales.
					next.ServeHTTP(w, r)
					return
				}
				// Key already exists — this is a replay.
				existing, qerr := client.IdempotencyKey.Query().
					Where(idempotencykey.TenantID(tid), idempotencykey.Key(key)).
					Only(ctx)
				if qerr != nil {
					next.ServeHTTP(w, r)
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
			next.ServeHTTP(cw, r)

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
