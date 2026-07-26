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

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	// Every replica runs this binary on startup — without coordination, N pods launching at once
	// each run their own Schema.Create concurrently against the same tables. Reproduced in
	// production 2026-07-26: concurrent migrate attempts from multiple starting replicas corrupted
	// a nullable-then-backfill migration on the live pos_orders table. A single physical
	// connection (MaxOpenConns=1) + a session-level advisory lock ensures only ONE pod across the
	// whole cluster ever executes the migration at a time; the rest block here until it finishes,
	// then find nothing pending and return immediately.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	const migrationLockKey = 727271001
	if _, err := db.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		log.Fatalf("acquire migration lock: %v", err)
	}
	defer func() {
		if _, err := db.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
			log.Printf("release migration lock: %v", err)
		}
	}()

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(drv))
	defer client.Close()

	// WithDropIndex must be explicit — without it, ent's live diff (schema.WithDir here compares
	// the CURRENT ent/schema/*.go structs against the live DB, it does not replay the checked-in
	// .sql files verbatim; there is no schema-revisions table) silently SKIPS any index/constraint
	// removal a struct change implies. Confirmed the actual outage mechanism in production
	// 2026-07-26: the stray global pos_orders_order_number_key unique index was removed from
	// posorder.go and a migration file was written to drop it hours earlier, but the live DB still
	// had it (verified via psql) — the runtime migrate binary never dropped it because this flag
	// was missing, so the SAME "duplicate key" outage recurred after looking fixed for hours.
	if err := client.Schema.Create(ctx,
		schema.WithDir(migrate.Dir),
		schema.WithDropColumn(true),
		schema.WithDropIndex(true),
	); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("migrations completed")
}
