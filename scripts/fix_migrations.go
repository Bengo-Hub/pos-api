//go:build ignore

package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	atlasmigrate "ariga.io/atlas/sql/migrate"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	dbURL := os.Getenv("POSTGRES_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/pos?sslmode=disable"
	}

	cwd, _ := os.Getwd()
	migrationsDir := filepath.Join(cwd, "internal", "ent", "migrate", "migrations")

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("failed to open db: %v", err)
	}

	_, _ = db.Exec(`DROP SCHEMA IF EXISTS public CASCADE; DROP SCHEMA IF EXISTS ent_dev CASCADE; CREATE SCHEMA public; CREATE SCHEMA ent_dev; GRANT ALL ON SCHEMA public TO postgres; GRANT ALL ON SCHEMA public TO public; GRANT ALL ON SCHEMA ent_dev TO postgres; GRANT ALL ON SCHEMA ent_dev TO public;`)
	db.Close()

	// Clean migrations directory
	files, _ := os.ReadDir(migrationsDir)
	for _, f := range files {
		if !f.IsDir() {
			os.Remove(filepath.Join(migrationsDir, f.Name()))
		}
	}

	// Write placeholder so atlas has a valid dir
	localDir, err := atlasmigrate.NewLocalDir(migrationsDir)
	if err != nil {
		log.Fatalf("failed creating local dir: %v", err)
	}
	_ = localDir.WriteFile("00000000000000_placeholder.sql", []byte("-- placeholder\n"))
	sum, _ := localDir.Checksum()
	atlasmigrate.WriteSumFile(localDir, sum)

	// Run the ent migration diff
	cmd := exec.Command("go", "run", "-mod=mod", "internal/ent/migrate/main.go", "initial_schema")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "POSTGRES_URL="+dbURL)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("migration diff failed: %v", err)
	}

	// Remove placeholder
	os.Remove(filepath.Join(migrationsDir, "00000000000000_placeholder.sql"))

	// Recompute checksum
	localDir2, _ := atlasmigrate.NewLocalDir(migrationsDir)
	sum2, _ := localDir2.Checksum()
	atlasmigrate.WriteSumFile(localDir2, sum2)

	fmt.Println("Done! Migrations regenerated.")
}
