//go:build ignore

package main

import (
	"log"

	atlasmigrate "ariga.io/atlas/sql/migrate"
)

func main() {
	dir, err := atlasmigrate.NewLocalDir("internal/ent/migrate/migrations")
	if err != nil {
		log.Fatalf("open dir: %v", err)
	}
	sum, err := dir.Checksum()
	if err != nil {
		log.Fatalf("checksum: %v", err)
	}
	if err := atlasmigrate.WriteSumFile(dir, sum); err != nil {
		log.Fatalf("write sum: %v", err)
	}
	log.Println("atlas.sum updated successfully")
}
