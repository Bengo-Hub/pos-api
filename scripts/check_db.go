//go:build ignore

package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	dbURL := os.Getenv("POSTGRES_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/pos?sslmode=disable"
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil { fmt.Println("connect error:", err); os.Exit(1) }
	defer db.Close()
	if err := db.Ping(); err != nil { fmt.Println("ping error:", err); os.Exit(1) }
	_, err = db.Exec("DROP SCHEMA IF EXISTS ent_dev CASCADE; CREATE SCHEMA ent_dev;")
	if err != nil { fmt.Println("schema error:", err); os.Exit(1) }
	fmt.Println("ent_dev schema created successfully")
}
