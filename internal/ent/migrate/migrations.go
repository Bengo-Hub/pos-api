package migrate

import (
	"embed"
	"io/fs"
	"log"

	atlasmigrate "ariga.io/atlas/sql/migrate"
)

//go:embed migrations/*.sql migrations/atlas.sum
var migrations embed.FS

// Dir is the Atlas migration directory, loaded from the embedded SQL files.
// Using MemDir so the binary works in Docker without needing the source tree on disk.
var Dir atlasmigrate.Dir

func init() {
	mem := &atlasmigrate.MemDir{}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		log.Fatalf("read embedded migrations: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(migrations, "migrations/"+e.Name())
		if err != nil {
			log.Fatalf("read embedded migration %s: %v", e.Name(), err)
		}
		if err := mem.WriteFile(e.Name(), data); err != nil {
			log.Fatalf("load migration %s into MemDir: %v", e.Name(), err)
		}
	}
	Dir = mem
}
