package main

import (
	"context"
	"database/sql"
	"log"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"

	"github.com/bengobox/pos-service/internal/config"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/migrate"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Prefer direct PostgreSQL URL to bypass PgBouncer during migrations.
	dbURL := cfg.Postgres.URL
	if cfg.Postgres.MigrateURL != "" {
		dbURL = cfg.Postgres.MigrateURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(drv))
	defer client.Close()

	if err := client.Schema.Create(ctx, schema.WithDir(migrate.Dir)); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("migrations completed")
}
