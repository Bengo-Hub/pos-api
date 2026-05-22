package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ServiceQueueEntry tracks walk-in clients in the service queue.
type ServiceQueueEntry struct {
	ent.Schema
}

func (ServiceQueueEntry) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("customer_name").NotEmpty(),
		field.String("customer_phone").Optional(),
		field.String("service_name").Optional().Comment("Requested service description"),
		field.UUID("staff_member_id", uuid.UUID{}).Optional().Nillable().Comment("Assigned staff member"),
		field.Enum("status").Values("waiting", "in_progress", "done", "cancelled").Default("waiting"),
		field.Int("queue_position").Default(0),
		field.UUID("pos_order_id", uuid.UUID{}).Optional().Nillable().Comment("Linked POS order when service is started"),
		field.Text("notes").Optional(),
		field.Time("called_at").Optional().Nillable().Comment("When customer was called in"),
		field.Time("started_at").Optional().Nillable().Comment("When service started"),
		field.Time("completed_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (ServiceQueueEntry) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id", "status"),
		index.Fields("tenant_id", "outlet_id", "created_at"),
	}
}
