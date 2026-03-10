package schema

import "entgo.io/ent"

// FeatureOverride holds the schema definition for the FeatureOverride entity.
type FeatureOverride struct {
	ent.Schema
}

// Fields of the FeatureOverride.
func (FeatureOverride) Fields() []ent.Field {
	return nil
}

// Edges of the FeatureOverride.
func (FeatureOverride) Edges() []ent.Edge {
	return nil
}
