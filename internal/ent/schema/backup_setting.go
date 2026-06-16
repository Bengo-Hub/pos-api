package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// BackupSetting is per-tenant auto-backup configuration (one row per tenant). Auto-backup
// is OPT-IN: it runs only when auto_enabled=true (default false — activated from the UI).
type BackupSetting struct {
	ent.Schema
}

func (BackupSetting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).Unique().Comment("Owning tenant — one settings row per tenant"),
		field.Bool("auto_enabled").Default(false).Comment("Auto-backup runs ONLY when true (default off — activate from UI)"),
		field.Int("schedule_hour").Default(2).Comment("Service-local hour 0-23 for the daily auto-backup"),
		field.Int("retention_days").Default(4).Comment("Delete backups older than this many days"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}
