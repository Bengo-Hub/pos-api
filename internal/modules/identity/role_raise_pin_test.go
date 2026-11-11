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
	"github.com/bengobox/pos-service/internal/ent/posuserroleassignment"
	"github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/ent/user"
)

// TestAuthSyncRoleRaiseClearsPIN locks in S2-12 (b) for the auth sync: when an auth event raises
// a staff row into a higher tier, the PIN that was set for the lower tier (possibly by a manager)
// is cleared and the person sets a new one. A move within a tier keeps the PIN, and a pin_set
// event, which carries the person's new PIN, still applies it.
func TestAuthSyncRoleRaiseClearsPIN(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:posraise_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	h := &AuthEventHandler{client: client, logger: zap.NewNop()}
	tenantID := uuid.New()
	now := time.Now()
	client.Tenant.Create().SetID(tenantID).SetName("Shop").SetSlug("shop-raise").SetStatus("active").
		SetSyncStatus("synced").SetTimezone("Africa/Nairobi").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
	outletRow := client.Outlet.Create().SetTenantID(tenantID).SetTenantSlug("shop-raise").SetCode("HQ").
		SetName("HQ").SetIsHq(true).SaveX(ctx)

	seed := func(role string) uuid.UUID {
		id := uuid.New()
		client.User.Create().SetID(id).SetAuthServiceUserID(id).SetTenantID(tenantID).SetEmail(id.String() + "@shop.test").
			SetFullName("Staff").SetStatus("active").SetSyncStatus("synced").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		client.StaffMember.Create().SetTenantID(tenantID).SetUserID(id).SetName("Staff").SetRole(role).
			SetPinHash("manager-set-hash").SetPinFastHash("fast-" + id.String()).SetPinFailedAttempts(2).
			SetIsActive(true).SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		return id
	}
	event := func(id uuid.UUID, kind string, payload map[string]any) *sharedevents.Event {
		payload["user_id"] = id.String()
		return sharedevents.NewEvent(kind, "auth.user", id, tenantID, payload)
	}
	row := func(id uuid.UUID) *ent.StaffMember {
		m, err := client.StaffMember.Query().Where(staffmember.UserID(id)).Only(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	// auth.user.updated raising cashier -> admin clears the PIN.
	raised := seed("cashier")
	if err := h.handleUserUpdated(ctx, event(raised, "updated", map[string]any{"roles": []any{"admin"}})); err != nil {
		t.Fatal(err)
	}
	if m := row(raised); m.Role != "admin" || m.PinHash != nil || m.PinFastHash != nil || m.PinFailedAttempts != 0 {
		t.Fatalf("updated cashier -> admin: role %q, pin cleared %v; want admin with no PIN", m.Role, m.PinHash == nil)
	}

	// auth.user.updated within the same tier keeps the PIN.
	same := seed("cashier")
	if err := h.handleUserUpdated(ctx, event(same, "updated", map[string]any{"roles": []any{"waiter"}})); err != nil {
		t.Fatal(err)
	}
	if m := row(same); m.Role != "waiter" || m.PinHash == nil {
		t.Fatalf("updated cashier -> waiter must keep the PIN, got role %q pin %v", m.Role, m.PinHash != nil)
	}

	// The created-event upsert raising cashier -> manager clears the PIN too.
	upserted := seed("cashier")
	h.upsertStaffMember(ctx, upserted, tenantID, "Staff", event(upserted, "created", map[string]any{
		"roles": []any{"manager"}, "outlet_ids": []any{outletRow.ID.String()},
	}))
	if m := row(upserted); m.Role != "manager" || m.PinHash != nil {
		t.Fatalf("upsert cashier -> manager: role %q, pin kept %v; want manager with no PIN", m.Role, m.PinHash != nil)
	}

	// A pin_set event raising the role carries the person's new PIN, which applies.
	pinSet := seed("cashier")
	if err := h.handleUserPINSet(ctx, event(pinSet, "pin_set", map[string]any{
		"service": "pos", "pin_hash": "new-hash", "roles": []any{"admin"},
	})); err != nil {
		t.Fatal(err)
	}
	if m := row(pinSet); m.Role != "admin" || m.PinHash == nil || *m.PinHash != "new-hash" {
		t.Fatalf("pin_set with a raise must apply the new PIN, got role %q", m.Role)
	}
}

// TestAuthSyncNeverCrossesTenants locks in C-1: an auth event for tenant B never changes the
// person's staff row in tenant A (role, name, PIN hash, fast hash). The person's pos User row
// sits in A, where pos first saw them, which used to decide the row the sync patched. A pin_set
// for B with no row there creates B's row only. An event whose tenant pos does not hold changes
// nothing.
func TestAuthSyncNeverCrossesTenants(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:poscross_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	h := &AuthEventHandler{client: client, logger: zap.NewNop()}
	now := time.Now()
	mkTenant := func(slug string) uuid.UUID {
		id := uuid.New()
		client.Tenant.Create().SetID(id).SetName(slug).SetSlug(slug).SetStatus("active").
			SetSyncStatus("synced").SetTimezone("Africa/Nairobi").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		// use_case set: the pin_set fallback's outlet query excludes logistics/warehouse with <>,
		// which a NULL use_case never satisfies.
		client.Outlet.Create().SetTenantID(id).SetTenantSlug(slug).SetCode("HQ").SetName("HQ").SetIsHq(true).
			SetUseCase("retail").ExecX(ctx)
		return id
	}
	tenantA, tenantB := mkTenant("cross-a"), mkTenant("cross-b")

	person := uuid.New()
	client.User.Create().SetID(person).SetAuthServiceUserID(person).SetTenantID(tenantA).SetEmail(person.String() + "@a.test").
		SetFullName("Till Person").SetStatus("active").SetSyncStatus("synced").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
	client.StaffMember.Create().SetTenantID(tenantA).SetUserID(person).SetName("Till Person").SetRole("cashier").
		SetPinHash("a-hash").SetPinFastHash("a-fast").SetIsActive(true).SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)

	rowIn := func(tid uuid.UUID) *ent.StaffMember {
		m, err := client.StaffMember.Query().Where(staffmember.TenantID(tid), staffmember.UserID(person)).Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	assertAUntouched := func(step string) {
		t.Helper()
		a := rowIn(tenantA)
		if a == nil || a.Role != "cashier" || a.Name != "Till Person" ||
			a.PinHash == nil || *a.PinHash != "a-hash" || a.PinFastHash == nil || *a.PinFastHash != "a-fast" {
			t.Fatalf("%s changed tenant A's row: %+v", step, a)
		}
	}
	event := func(kind string, tid uuid.UUID, payload map[string]any) *sharedevents.Event {
		payload["user_id"] = person.String()
		return sharedevents.NewEvent(kind, "auth.user", person, tid, payload)
	}

	// auth.user.updated for tenant B, where the person is admin: A's row stays a cashier.
	if err := h.handleUserUpdated(ctx, event("updated", tenantB, map[string]any{
		"roles": []any{"admin"}, "full_name": "Boss Of B",
	})); err != nil {
		t.Fatal(err)
	}
	assertAUntouched("updated for tenant B")

	// auth.user.pin_set for tenant B: the PIN and admin role land on a new row in B only.
	if err := h.handleUserPINSet(ctx, event("pin_set", tenantB, map[string]any{
		"service": "pos", "pin_hash": "b-hash", "pin": "9182", "roles": []any{"admin"},
	})); err != nil {
		t.Fatal(err)
	}
	assertAUntouched("pin_set for tenant B")
	b := rowIn(tenantB)
	if b == nil || b.Role != "admin" || b.PinHash == nil || *b.PinHash != "b-hash" {
		t.Fatalf("pin_set for tenant B must create B's row with B's PIN and role, got %+v", b)
	}

	// An event for a tenant pos does not hold changes nothing anywhere.
	unknown := uuid.New()
	if err := h.handleUserUpdated(ctx, event("updated", unknown, map[string]any{"roles": []any{"admin"}})); err != nil {
		t.Fatal(err)
	}
	if err := h.handleUserPINSet(ctx, event("pin_set", unknown, map[string]any{
		"service": "pos", "pin_hash": "x-hash", "roles": []any{"admin"},
	})); err != nil {
		t.Fatal(err)
	}
	assertAUntouched("events for an unknown tenant")
	if n := client.StaffMember.Query().Where(staffmember.UserID(person)).CountX(ctx); n != 2 {
		t.Fatalf("an unknown tenant's events must not create rows, got %d rows", n)
	}

	// An updated event for A itself still applies to A.
	if err := h.handleUserUpdated(ctx, event("updated", tenantA, map[string]any{"roles": []any{"waiter"}})); err != nil {
		t.Fatal(err)
	}
	if a := rowIn(tenantA); a.Role != "waiter" || a.PinHash == nil {
		t.Fatalf("an event for tenant A must still update A within the tier, got %+v", a)
	}
}

// TestRevokeInOneTenantKeepsOtherTenants: a "services without pos" revoke for one tenant removes
// the person's copy in that tenant only (staff row, staff outlets, role assignments). Their row
// in another tenant stays intact and usable, and the User row goes only with the last staff row.
func TestRevokeInOneTenantKeepsOtherTenants(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:posrevoke_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	h := &AuthEventHandler{client: client, logger: zap.NewNop()}
	now := time.Now()
	type tenantSeed struct{ id, outlet, role uuid.UUID }
	mkTenant := func(slug string) tenantSeed {
		id := uuid.New()
		client.Tenant.Create().SetID(id).SetName(slug).SetSlug(slug).SetStatus("active").
			SetSyncStatus("synced").SetTimezone("Africa/Nairobi").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		o := client.Outlet.Create().SetTenantID(id).SetTenantSlug(slug).SetCode("HQ").SetName("HQ").SetIsHq(true).
			SetUseCase("retail").SaveX(ctx)
		r := client.POSRoleV2.Create().SetTenantID(id).SetRoleCode("cashier").SetName("Cashier").
			SetIsSystemRole(true).SetCreatedAt(now).SetUpdatedAt(now).SaveX(ctx)
		return tenantSeed{id: id, outlet: o.ID, role: r.ID}
	}
	tA, tB := mkTenant("revoke-a"), mkTenant("revoke-b")

	// A person whose pos User row sits in A, with a staff row (PIN, outlet, assignment) in both.
	seedPerson := func() uuid.UUID {
		person := uuid.New()
		client.User.Create().SetID(person).SetAuthServiceUserID(person).SetTenantID(tA.id).
			SetEmail(person.String() + "@a.test").SetFullName("Two Tenants").SetStatus("active").
			SetSyncStatus("synced").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		for _, ts := range []tenantSeed{tA, tB} {
			sm := client.StaffMember.Create().SetTenantID(ts.id).SetUserID(person).SetName("Two Tenants").
				SetRole("cashier").SetPinHash("hash-" + ts.id.String()).SetIsActive(true).
				SetCreatedAt(now).SetUpdatedAt(now).SaveX(ctx)
			client.StaffOutlet.Create().SetTenantID(ts.id).SetStaffMemberID(sm.ID).SetOutletID(ts.outlet).
				SetIsHomeOutlet(true).ExecX(ctx)
			client.POSUserRoleAssignment.Create().SetTenantID(ts.id).SetUserID(person).SetRoleID(ts.role).
				SetAssignedBy(person).SetAssignedAt(now).ExecX(ctx)
		}
		return person
	}
	revoke := func(person, tid uuid.UUID) {
		t.Helper()
		evt := sharedevents.NewEvent("updated", "auth.user", person, tid, map[string]any{
			"user_id": person.String(), "services": []any{"inventory"},
		})
		if err := h.handleUserUpdated(ctx, evt); err != nil {
			t.Fatal(err)
		}
	}
	rowIn := func(person, tid uuid.UUID) *ent.StaffMember {
		m, err := client.StaffMember.Query().Where(staffmember.TenantID(tid), staffmember.UserID(person)).
			WithOutlets().Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	assignmentsIn := func(person, tid uuid.UUID) int {
		return client.POSUserRoleAssignment.Query().
			Where(posuserroleassignment.TenantID(tid), posuserroleassignment.UserID(person)).CountX(ctx)
	}
	userExists := func(person uuid.UUID) bool {
		return client.User.Query().Where(user.AuthServiceUserIDEQ(person)).ExistX(ctx)
	}
	assertIntact := func(person uuid.UUID, ts tenantSeed, step string) {
		t.Helper()
		m := rowIn(person, ts.id)
		if m == nil || !m.IsActive || m.PinHash == nil || *m.PinHash != "hash-"+ts.id.String() ||
			len(m.Edges.Outlets) != 1 || assignmentsIn(person, ts.id) != 1 {
			t.Fatalf("%s: the other tenant's row must stay intact and usable, got %+v", step, m)
		}
	}

	// Revoke in A (the User row's tenant): only A's copy goes.
	p1 := seedPerson()
	revoke(p1, tA.id)
	if rowIn(p1, tA.id) != nil || assignmentsIn(p1, tA.id) != 0 {
		t.Fatal("revoke in A must remove A's staff row and assignments")
	}
	assertIntact(p1, tB, "revoke in A")
	if !userExists(p1) {
		t.Fatal("the User row must stay while tenant B still has a staff row")
	}
	// B's row is still usable: an update for B applies to it.
	if err := h.handleUserUpdated(ctx, sharedevents.NewEvent("updated", "auth.user", p1, tB.id, map[string]any{
		"user_id": p1.String(), "roles": []any{"waiter"},
	})); err != nil {
		t.Fatal(err)
	}
	if m := rowIn(p1, tB.id); m == nil || m.Role != "waiter" {
		t.Fatal("an update for B must still reach B's row after A's revoke")
	}
	// Revoke in B too: the last staff row goes, and the User row with it.
	revoke(p1, tB.id)
	if rowIn(p1, tB.id) != nil || userExists(p1) {
		t.Fatal("revoking the last tenant must remove the staff row and the User row")
	}

	// Revoke in B (not the User row's tenant): B's copy goes, A's stays (it used to do nothing).
	p2 := seedPerson()
	revoke(p2, tB.id)
	if rowIn(p2, tB.id) != nil || assignmentsIn(p2, tB.id) != 0 {
		t.Fatal("revoke in B must remove B's staff row and assignments")
	}
	assertIntact(p2, tA, "revoke in B")
	if !userExists(p2) {
		t.Fatal("the User row must stay while tenant A still has a staff row")
	}
}
