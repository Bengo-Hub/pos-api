package handlers

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/migrate"
	// required by schema hooks, same as enttest.Open pulls in.
	_ "github.com/bengobox/pos-service/internal/ent/runtime"
)

// openSQLiteTestClient opens a fresh SQLite ent.Client backed by a real temp file (t.TempDir(),
// auto-removed after the test) and runs schema migration — a drop-in replacement for
// `enttest.Open(t, "sqlite3", "file:X?mode=memory&cache=shared")`, pinned to a single connection.
//
// Why not `mode=memory&cache=shared` (what every call site used before): this package intermittently
// saw a seeded row go "missing" from a query moments later — two sibling tests
// (TestGetSummary_GrossProfitReadsLocalCache_NotEqualToRevenue /
// TestGetSummary_GrossProfitZeroCost_WhenNoCatalogCacheRow) traded which one failed depending on
// what else in the full suite ran around them, never reproducible in isolation. Pinning
// MaxOpenConns(1) (still done below — also just correct for a test that never needs concurrent DB
// access) did not fix it. The remaining suspect is shared-cache in-memory mode itself: across ~40
// tests in this package opening/closing a same-shaped `mode=memory&cache=shared` URI in a tight
// loop, `modernc.org/sqlite`'s shared in-memory VFS is a process-wide, name-keyed resource — a real
// temp file has no such shared/global state to race on, so it sidesteps the class of bug entirely
// rather than needing to prove which specific driver-internal race it was.
func openSQLiteTestClient(t *testing.T, namePrefix string) *ent.Client {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), namePrefix+".sqlite")
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open sqlite test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := ent.NewClient(ent.Driver(drv))
	t.Cleanup(func() { _ = client.Close() })

	tables, err := schema.CopyTables(migrate.Tables)
	if err != nil {
		t.Fatalf("copy migrate tables: %v", err)
	}
	if err := migrate.Create(context.Background(), client.Schema, tables); err != nil {
		t.Fatalf("migrate sqlite test db: %v", err)
	}
	return client
}
