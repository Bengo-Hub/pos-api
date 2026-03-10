package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// PriceBookItem holds the schema definition for the PriceBookItem entity.
type PriceBookItem struct {
	ent.Schema
}

// Fields of the PriceBookItem.
func (PriceBookItem) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("price_book_id", uuid.UUID{}),
		field.UUID("catalog_item_id", uuid.UUID{}),
		field.Float("price_amount"),
		field.String("currency").
			Default("KES"),
	}
}

// Edges of the PriceBookItem.
func (PriceBookItem) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("price_book", PriceBook.Type).
			Ref("items").
			Field("price_book_id").
			Unique().
			Required(),
		edge.From("catalog_item", CatalogItem.Type).
			Ref("price_book_items").
			Field("catalog_item_id").
			Unique().
			Required(),
	}
}
