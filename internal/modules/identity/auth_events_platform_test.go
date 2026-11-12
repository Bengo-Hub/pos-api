package identity

import (
	"context"
	"fmt"
	"testing"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/ent/staffmember"
)

// On the platform tenant the auth sync maps only explicit POS roles: member, staff, viewer and
// roles POS does not know give none (staff no longer means cashier there). Business tenants keep
// the old mapping.
func TestMapSSORoleToPOSIn(t *testing.T) {
	roles := func(rs ...string) map[string]interface{} {
		out := make([]interface{}, len(rs))
		for i, r := range rs {
			out[i] = r
		}
		return map[string]interface{}{"roles": out}
	}
	platform := []struct {
		in   []string
		want string
	}{
		{[]string{"member"}, ""},
		{[]string{"staff"}, ""},
		{[]string{"viewer"}, ""},
		{[]string{"support_agent"}, ""},
		{[]string{"staff", "manager"}, "manager"},
		{[]string{"cashier"}, "cashier"},
		{[]string{"admin"}, "admin"},
	}
	for _, c := range platform {
		if got := mapSSORoleToPOSIn(roles(c.in...), true); got != c.want {
			t.Errorf("platform %v = %q, want %q", c.in, got, c.want)
		}
	}
	if got := mapSSORoleToPOSIn(roles("staff"), false); got != "cashier" {
		t.Errorf("business staff = %q, want cashier", got)
	}
	if got := mapSSORoleToPOSIn(roles("member"), false); got != "" {
		t.Errorf("business member = %q, want none (unchanged)", got)
	}
}

// Auth events on the platform tenant create no staff row and set no role for a generic role, and
// a pin_set never falls back to cashier there. Explicit roles and business tenants work as before.
func TestAuthSyncPlatformTenantGenericRolesGetNoPOSRole(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:posauthplatform_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	now := time.Now()
	mkTenant := func(slug string) uuid.UUID {
		id := uuid.New()
		client.Tenant.Create().SetID(id).SetName(slug).SetSlug(slug).SetStatus("active").
			SetSyncStatus("synced").SetTimezone("Africa/Nairobi").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		// A real HQ carries its use case from auth; the staff upsert skips outlets with none.
		client.Outlet.Create().SetTenantID(id).SetTenantSlug(slug).SetCode("HQ").SetName("HQ").SetIsHq(true).
			SetUseCase("retail").ExecX(ctx)
		return id
	}
	platform, business := mkTenant("auth-platform"), mkTenant("auth-business")
	h := &AuthEventHandler{client: client, logger: zap.NewNop(), platformTenantID: platform}

	rowIn := func(person, tid uuid.UUID) *ent.StaffMember {
		m, err := client.StaffMember.Query().Where(staffmember.TenantID(tid), staffmember.UserID(person)).Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	event := func(person, tid uuid.UUID, kind string, payload map[string]any) *sharedevents.Event {
		payload["user_id"] = person.String()
		return sharedevents.NewEvent(kind, "auth.user", person, tid, payload)
	}

	// Created-event upsert: staff on the platform tenant gets no row; on a business tenant it is
	// a cashier; an explicit cashier on the platform tenant gets its row.
	p1 := uuid.New()
	h.upsertStaffMember(ctx, p1, platform, "P1", event(p1, platform, "created", map[string]any{"roles": []any{"staff"}}))
	if rowIn(p1, platform) != nil {
		t.Fatal("platform staff must get no staff row")
	}
	b1 := uuid.New()
	h.upsertStaffMember(ctx, b1, business, "B1", event(b1, business, "created", map[string]any{"roles": []any{"staff"}}))
	if m := rowIn(b1, business); m == nil || m.Role != "cashier" {
		t.Fatalf("business staff keeps cashier, got %+v", m)
	}
	p2 := uuid.New()
	h.upsertStaffMember(ctx, p2, platform, "P2", event(p2, platform, "created", map[string]any{"roles": []any{"cashier"}}))
	if m := rowIn(p2, platform); m == nil || m.Role != "cashier" {
		t.Fatalf("explicit cashier on the platform tenant gets a row, got %+v", m)
	}

	// pin_set with no row: a platform member gets no row (no cashier fallback); business keeps it.
	p3 := uuid.New()
	if err := h.handleUserPINSet(ctx, event(p3, platform, "pin_set", map[string]any{
		"service": "pos", "pin_hash": "h", "roles": []any{"member"},
	})); err != nil {
		t.Fatal(err)
	}
	if rowIn(p3, platform) != nil {
		t.Fatal("a platform member's pin_set must not create a cashier row")
	}
	b3 := uuid.New()
	if err := h.handleUserPINSet(ctx, event(b3, business, "pin_set", map[string]any{
		"service": "pos", "pin_hash": "h", "roles": []any{"member"},
	})); err != nil {
		t.Fatal(err)
	}
	if m := rowIn(b3, business); m == nil || m.Role != "cashier" {
		t.Fatalf("business pin_set keeps the cashier fallback, got %+v", m)
	}

	// updated: a generic role on the platform tenant never sets cashier on an existing row.
	p4 := uuid.New()
	client.StaffMember.Create().SetTenantID(platform).SetUserID(p4).SetName("P4").SetRole("waiter").
		SetIsActive(true).SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
	if err := h.handleUserUpdated(ctx, event(p4, platform, "updated", map[string]any{
		// the user row it backfills needs an email and a name
		"roles": []any{"staff"}, "email": p4.String() + "@x.test", "full_name": "P4",
	})); err != nil {
		t.Fatal(err)
	}
	if m := rowIn(p4, platform); m == nil || m.Role != "waiter" {
		t.Fatalf("platform staff update must not set cashier, got %+v", m)
	}
}
