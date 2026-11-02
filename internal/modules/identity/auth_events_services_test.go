package identity

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/ent/user"
)

// Pure-Go sqlite shim, as in the other pos-api test packages (internal/http/middleware): ent
// expects a "sqlite3" driver, modernc.org/sqlite registers "sqlite"; foreign keys on per connection.
type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() {
	sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}})
}

// auth-api's services list decides whether pos-api keeps a member: a list without pos removes the
// copy (user and staff member) on updated and pin_set, but only when the copy belongs to the
// event's tenant; an event without a list keeps today's behaviour.
func TestAuthEventServicesListRemovesPOSCopy(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:posusersvc_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	h := &AuthEventHandler{client: client, logger: zap.NewNop()}
	tenantID := uuid.New()
	now := time.Now()
	client.Tenant.Create().SetID(tenantID).SetName("Shop").SetSlug("shop").SetStatus("active").
		SetSyncStatus("synced").SetTimezone("Africa/Nairobi").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
	seed := func() uuid.UUID {
		id := uuid.New()
		client.User.Create().SetID(id).SetAuthServiceUserID(id).SetTenantID(tenantID).SetEmail(id.String() + "@shop.test").
			SetFullName("Staff").SetStatus("active").SetSyncStatus("synced").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		client.StaffMember.Create().SetTenantID(tenantID).SetUserID(id).SetName("Staff").SetRole("cashier").
			SetIsActive(true).SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		return id
	}
	event := func(id, tenant uuid.UUID, kind string, payload map[string]any) *sharedevents.Event {
		payload["user_id"] = id.String()
		return sharedevents.NewEvent(kind, "auth.user", id, tenant, payload)
	}
	present := func(id uuid.UUID) bool {
		return client.User.Query().Where(user.AuthServiceUserIDEQ(id)).ExistX(ctx) ||
			client.StaffMember.Query().Where(staffmember.UserID(id)).ExistX(ctx)
	}

	storekeeper := seed()
	if err := h.handleUserUpdated(ctx, event(storekeeper, tenantID, "updated", map[string]any{"services": []any{"inventory"}})); err != nil {
		t.Fatal(err)
	}
	if present(storekeeper) {
		t.Fatal("a member whose services leave out pos must be removed")
	}

	terminal := seed()
	if err := h.handleUserPINSet(ctx, event(terminal, tenantID, "pin_set", map[string]any{"service": "pos", "pin_hash": "x", "services": []any{}})); err != nil {
		t.Fatal(err)
	}
	if present(terminal) {
		t.Fatal("an empty services list is a decided no, on pin_set too")
	}

	// The person's pos copy belongs to tenantID; a "no pos" from another tenant leaves it.
	multi := seed()
	if err := h.handleUserUpdated(ctx, event(multi, uuid.New(), "updated", map[string]any{"services": []any{"inventory"}})); err != nil {
		t.Fatal(err)
	}
	if !present(multi) {
		t.Fatal("a revoke from another tenant must not remove this tenant's copy")
	}

	// Without a list the event updates as before.
	if err := h.handleUserUpdated(ctx, event(multi, tenantID, "updated", map[string]any{"full_name": "Renamed"})); err != nil {
		t.Fatal(err)
	}
	if u := client.User.Query().Where(user.AuthServiceUserIDEQ(multi)).OnlyX(ctx); u.FullName != "Renamed" {
		t.Fatalf("an event without services must update as before, got %q", u.FullName)
	}
}
