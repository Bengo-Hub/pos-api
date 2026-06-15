package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// IdempotencyKey stores the outcome of a mutating request so that a replay of the
// same client-generated key (sent by the offline-sync worker after a reconnect, or
// after a "request succeeded but response was lost" failure) returns the original
// response instead of re-executing the operation.
//
// Lifecycle: a row is created "in_flight" before the handler runs; the wrapping
// middleware updates it to "completed" with the captured response once the handler
// returns a cacheable status. A second request with the same (tenant_id, key) while
// the first is still in_flight gets 409; once completed it gets the stored response.
type IdempotencyKey struct {
	ent.Schema
}

// Fields of the IdempotencyKey.
func (IdempotencyKey) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		// The client-supplied Idempotency-Key header value (typically the offline local_id uuid).
		field.String("key").
			NotEmpty(),
		// Request fingerprint (method + path) — guards against accidental key reuse across endpoints.
		field.String("endpoint").
			Default(""),
		field.String("status").
			Default("in_flight").
			Comment("in_flight | completed"),
		field.Int("response_code").
			Default(0),
		// Captured JSON response body, replayed verbatim on a duplicate request.
		field.Bytes("response_body").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		// Sweep window — rows past this are eligible for deletion by maintenance.
		field.Time("expires_at"),
	}
}

// Indexes of the IdempotencyKey.
func (IdempotencyKey) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "key").Unique(),
		index.Fields("expires_at"),
	}
}
