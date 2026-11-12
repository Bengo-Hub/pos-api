package tenant

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/migrate"
	_ "github.com/bengobox/pos-service/internal/ent/runtime"
)

// openSlugTestClient mirrors handlers' openSQLiteTestClient: a temp-file SQLite DB, one
// connection, foreign keys on.
func openSlugTestClient(t *testing.T) *ent.Client {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "tenant_slug.sqlite") + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
	t.Cleanup(func() { _ = client.Close() })
	tables, err := schema.CopyTables(migrate.Tables)
	if err != nil {
		t.Fatalf("copy tables: %v", err)
	}
	if err := migrate.Create(context.Background(), client.Schema, tables); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return client
}

// Auth renamed the tenant (same UUID, new slug). SyncTenant by the new slug moves the tenants row
// and the tenant's outlets that still carry the old slug; another tenant's outlets stay put.
func TestSyncTenantRenameMovesTenantAndOutlets(t *testing.T) {
	client := openSlugTestClient(t)
	ctx := context.Background()
	id, otherID := uuid.New(), uuid.New()
	client.Tenant.Create().SetID(id).SetName("Old").SetSlug("old-co").ExecX(ctx)
	client.Tenant.Create().SetID(otherID).SetName("Other").SetSlug("other-co").ExecX(ctx)
	mine := client.Outlet.Create().SetTenantID(id).SetTenantSlug("old-co").SetCode("HQ").SetName("HQ").SaveX(ctx)
	theirs := client.Outlet.Create().SetTenantID(otherID).SetTenantSlug("old-co").SetCode("X").SetName("X").SaveX(ctx)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id.String(), "slug": "new-co", "name": "New", "status": "active"})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AUTH_API_URL", srv.URL)

	got, err := NewSyncer(client, srv.URL, nil).SyncTenant(ctx, "new-co")
	if err != nil || got != id {
		t.Fatalf("SyncTenant = %s, %v; want %s", got, err, id)
	}
	if row := client.Tenant.GetX(ctx, id); row.Slug != "new-co" || row.Name != "New" {
		t.Fatalf("tenant = slug %q name %q; want new-co / New", row.Slug, row.Name)
	}
	if s := client.Outlet.GetX(ctx, mine.ID).TenantSlug; s != "new-co" {
		t.Fatalf("own outlet tenant_slug = %q, want new-co", s)
	}
	if s := client.Outlet.GetX(ctx, theirs.ID).TenantSlug; s != "old-co" {
		t.Fatalf("another tenant's outlet must not move, got %q", s)
	}
}

// A new slug already held by another local tenant is refused and nothing moves.
func TestRenameTenantSlugRefusesClash(t *testing.T) {
	client := openSlugTestClient(t)
	ctx := context.Background()
	id, otherID := uuid.New(), uuid.New()
	client.Tenant.Create().SetID(id).SetName("A").SetSlug("a-co").ExecX(ctx)
	client.Tenant.Create().SetID(otherID).SetName("B").SetSlug("b-co").ExecX(ctx)
	o := client.Outlet.Create().SetTenantID(id).SetTenantSlug("a-co").SetCode("HQ").SetName("HQ").SaveX(ctx)

	if _, err := NewSyncer(client, "", nil).RenameTenantSlug(ctx, id, "b-co"); err == nil {
		t.Fatal("expected a clash error")
	}
	if s := client.Tenant.GetX(ctx, id).Slug; s != "a-co" {
		t.Fatalf("tenant slug moved to %q", s)
	}
	if s := client.Outlet.GetX(ctx, o.ID).TenantSlug; s != "a-co" {
		t.Fatalf("outlet slug moved to %q", s)
	}
}
