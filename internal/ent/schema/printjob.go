package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PrintJob is a queued background print job (AccuPOS-style remote printing). Placing an order /
// clicking a Print button enqueues jobs with the ESC/POS payload rendered server-side; the on-site
// Local Print Agent polls, claims and prints them (network 9100 or a locally-installed USB/OS
// printer by name), so the till UI never blocks on printing and never opens a browser dialog.
type PrintJob struct {
	ent.Schema
}

// Fields of the PrintJob.
func (PrintJob) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}).Optional().Nillable(),
		field.String("job_type").
			Comment("bill | kitchen | bar | waiter | receipt | test | drawer"),
		// Target printer snapshot (from the outlet's printer_profiles at enqueue time), so the job
		// still prints correctly even if settings change while it is queued.
		field.String("profile_id").Optional(),
		field.String("printer_type").Optional().Comment("network | usb | os | bluetooth"),
		field.String("printer_ip").Optional(),
		field.Int("printer_port").Default(9100),
		field.String("printer_name").Optional().Comment("OS spooler name for usb/os targets"),
		field.String("paper").Optional().Comment("paper size label, e.g. 80mm"),
		field.Text("payload_hex").Comment("ESC/POS bytes, hex encoded — rendered at enqueue"),
		field.String("status").
			Default("queued").
			Comment("queued | claimed | printed | failed | expired"),
		field.Int("attempts").Default(0),
		field.String("claimed_by").Optional().Comment("print agent id that holds the lease"),
		field.Time("claim_expires_at").Optional().Nillable(),
		field.String("dedupe_key").Optional().
			Comment("idempotency key (e.g. order:type:profile) so retries never double-print"),
		field.Text("last_error").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

// Indexes of the PrintJob.
func (PrintJob) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id", "status"),
		index.Fields("tenant_id", "dedupe_key").Unique(),
		index.Fields("status", "created_at"),
	}
}
