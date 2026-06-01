package migrate

import (
	"embed"
	"io/fs"
	"log"

	atlasmigrate "ariga.io/atlas/sql/migrate"
)

//go:embed migrations/*.sql migrations/atlas.sum
var migrations embed.FS

// Dir is the migration directory.
var Dir atlasmigrate.Dir

func init() {
	var err error
	// Try local dir first (works during development and migration generation).
	Dir, err = atlasmigrate.NewLocalDir("internal/ent/migrate/migrations")
	if err == nil {
		return
	}
	// Fall back to embedded filesystem (works in production Docker container
	// where the local path does not exist).
	Dir, err = newEmbedDir(migrations, "migrations")
	if err != nil {
		log.Fatalf("failed to open migration directory from embedded FS: %v", err)
	}
}

// newEmbedDir creates a MemDir pre-populated from an embed.FS sub-path.
func newEmbedDir(fsys embed.FS, path string) (*atlasmigrate.MemDir, error) {
	sub, err := fs.Sub(fsys, path)
	if err != nil {
		return nil, err
	}
	md := atlasmigrate.OpenMemDir(path)
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(sub, e.Name())
		if err != nil {
			return nil, err
		}
		md.WriteFile(e.Name(), data)
	}
	return md, nil
}
