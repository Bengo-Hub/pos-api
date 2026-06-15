package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Backup tracks a TENANT-SCOPED logical export artifact produced by the backup module
// (see internal/modules/backup). Each row points at one gzipped-JSON file under the
// per-tenant backup path. The scheduler (daily) inserts rows; churn deletes the file AND
// its row once older than the retention window. Never a platform-wide dump — the row's
// tenant_id always scopes ownership.
type Backup struct {
	ent.Schema
}

func (Backup) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant — scopes ownership of the artifact"),
		field.String("name").
			NotEmpty().
			Comment("Artifact filename (base name within the tenant's backup dir)"),
		field.String("path").
			NotEmpty().
			Comment("Absolute path to the artifact on the backup volume"),
		field.Int64("size_bytes").
			Default(0).
			Comment("Size of the artifact in bytes"),
		field.String("status").
			Default("completed").
			Comment("completed | failed"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (Backup) Edges() []ent.Edge { return nil }

func (Backup) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "created_at"),
		index.Fields("created_at"),
	}
}
