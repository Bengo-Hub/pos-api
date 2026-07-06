package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PrintAgent is a paired on-site Local Print Agent instance (one per terminal/back-office PC).
// Pairing issues a one-time plaintext key (returned once, stored here as a SHA-256 hash); the
// agent authenticates its job-poll requests with that key via the X-Agent-Key header.
type PrintAgent struct {
	ent.Schema
}

// Fields of the PrintAgent.
func (PrintAgent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("name").NotEmpty().Comment("Operator-friendly label, e.g. 'Front counter PC'."),
		field.String("key_hash").NotEmpty().Unique().
			Comment("SHA-256 hex of the pairing key; the plaintext is shown once at pairing."),
		field.Time("last_seen_at").Optional().Nillable().
			Comment("Bumped on every job poll — an agent is 'online' when this is recent."),
		field.String("version").Optional().Comment("Agent build version from its last poll."),
		field.Bool("revoked").Default(false),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

// Indexes of the PrintAgent.
func (PrintAgent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
	}
}
