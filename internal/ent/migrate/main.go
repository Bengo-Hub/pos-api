//go:build ignore

package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/bengobox/pos-service/internal/ent/migrate"

	atlasmigrate "ariga.io/atlas/sql/migrate"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/schema"
	_ "github.com/lib/pq"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	ctx := context.Background()
	// Create a local migration directory able to write to "internal/ent/migrate/migrations".
	dir, err := atlasmigrate.NewLocalDir("internal/ent/migrate/migrations")
	if err != nil {
		log.Fatalf("failed creating atlas migration directory: %v", err)
	}
	// Migrate diff options.
	opts := []schema.MigrateOption{
		schema.WithDir(dir),                         // provide migration directory
		schema.WithMigrationMode(schema.ModeReplay), // provide migration mode
		schema.WithDialect(dialect.Postgres),        // Ent dialect to use
		schema.WithFormatter(atlasmigrate.DefaultFormatter),
	}
	if len(os.Args) != 2 {
		log.Fatalln("migration name is required. use: 'go run -mod=mod internal/ent/migrate/main.go <name>'")
	}
	
	// Generate migrations using Atlas support.
	dbURL := os.Getenv("POSTGRES_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/pos?sslmode=disable"
	}
	devURL := dbURL
	if strings.Contains(devURL, "?") {
		devURL += "&search_path=ent_dev"
	} else {
		devURL += "?search_path=ent_dev"
	}

	err = migrate.NamedDiff(ctx, devURL, os.Args[1], opts...)
	if err != nil {
		log.Fatalf("failed generating migration: %v", err)
	}
}
