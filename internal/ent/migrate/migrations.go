package migrate

import (
	"embed"
	"log"

	atlasmigrate "ariga.io/atlas/sql/migrate"
)

//go:embed migrations/*.sql migrations/atlas.sum
var migrations embed.FS

// Dir is the migration directory.
var Dir atlasmigrate.Dir

func init() {
	var err error
	// Use NewLocalDir from atlas migrate package
	Dir, err = atlasmigrate.NewLocalDir("internal/ent/migrate/migrations")
	if err != nil {
		log.Printf("Warning: failed to create local migration dir: %v. Falling back to default if applicable.", err)
	}
}
