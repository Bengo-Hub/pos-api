package identity

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/ent/user"
)

// TestJITStaffRowStaysInTokenTenant locks in I-C: sign-in provisioning (EnsureUserFromToken)
// creates the staff row in the TOKEN's tenant, never in the tenant where pos first saw the
// person, and never for someone who holds only self-service roles.
func TestJITStaffRowStaysInTokenTenant(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:posjit_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	s := &Service{client: client, tenantSyncer: failingSyncer{}}
	now := time.Now()
	mkTenant := func(slug string) uuid.UUID {
		id := uuid.New()
		client.Tenant.Create().SetID(id).SetName(slug).SetSlug(slug).SetStatus("active").
			SetSyncStatus("synced").SetTimezone("Africa/Nairobi").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		client.Outlet.Create().SetTenantID(id).SetTenantSlug(slug).SetCode("HQ").SetName("HQ").SetIsHq(true).
			SetUseCase("retail").ExecX(ctx)
		return id
	}
	tA, tB := mkTenant("jit-a"), mkTenant("jit-b")
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
	existingUserIn := func(tid uuid.UUID) uuid.UUID {
		id := uuid.New()
		client.User.Create().SetID(id).SetAuthServiceUserID(id).SetTenantID(tid).SetEmail(id.String() + "@x.test").
			SetFullName("Person").SetStatus("active").SetSyncStatus("synced").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		return id
	}
	signIn := func(person uuid.UUID, slug string, roles ...string) {
		t.Helper()
		if _, err := s.EnsureUserFromToken(ctx, person, slug, map[string]any{
			"email": person.String() + "@x.test", "roles": roles,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Revoked in A (User row kept, no staff row there), then uses POS with their tenant-B token.
	revoked := existingUserIn(tA)
	signIn(revoked, "jit-b", "admin")
	if rowIn(revoked, tA) != nil {
		t.Fatal("a tenant-B token must never recreate the person's staff row in tenant A")
	}
	if b := rowIn(revoked, tB); b == nil || b.Role != "admin" {
		t.Fatalf("the tenant-B token provisions B's own row with B's role, got %+v", b)
	}

	// A member of A with no staff row who runs tenant B: signing in to B creates nothing in A.
	member := existingUserIn(tA)
	signIn(member, "jit-b", "admin")
	if rowIn(member, tA) != nil {
		t.Fatal("a member of A signing in to B must get no staff row in A")
	}
	// Signing in to A with A's own token as an invited member (business tenants are invite only
	// for "member") gives A's row the cashier role, as invited staff always had.
	signIn(member, "jit-a", "member")
	if a := rowIn(member, tA); a == nil || a.Role != "cashier" {
		t.Fatalf("an invited member signing in to their own tenant keeps cashier, got %+v", a)
	}

	// A customer of A signing in with A's token gets no staff row there.
	shopper := existingUserIn(tA)
	signIn(shopper, "jit-a", "customer")
	if rowIn(shopper, tA) != nil {
		t.Fatal("a customer-only token must not create a staff row")
	}

	// A brand-new storefront customer gets a User row but no staff row.
	customer := uuid.New()
	signIn(customer, "jit-b", "customer")
	if !client.User.Query().Where(user.AuthServiceUserIDEQ(customer)).ExistX(ctx) {
		t.Fatal("the customer's pos User row is still created")
	}
	if rowIn(customer, tB) != nil {
		t.Fatal("a self-service-only person must not get a staff row")
	}

	// A token for a tenant pos cannot resolve: an existing user carries on, nothing is written.
	stray := existingUserIn(tA)
	signIn(stray, "jit-unknown", "admin")
	if n := client.StaffMember.Query().Where(staffmember.UserID(stray)).CountX(ctx); n != 0 {
		t.Fatalf("an unresolvable token tenant must write no staff row, got %d", n)
	}
}

// TestAuthRedeliveryBackoff locks in M-1: failed auth events are retried with a growing delay,
// and the delivery that reaches MaxDeliver is reported as the last one.
func TestAuthRedeliveryBackoff(t *testing.T) {
	want := []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	for i, w := range want {
		got, last := authRedelivery(uint64(i + 1))
		if got != w || last {
			t.Fatalf("delivery %d: delay %v last %v; want %v, not last", i+1, got, last, w)
		}
	}
	if _, last := authRedelivery(authMaxDeliver); !last {
		t.Fatal("the MaxDeliver-th delivery must be the last")
	}
	if got, last := authRedelivery(0); got != 2*time.Second || last {
		t.Fatalf("missing metadata (0) must use the first delay, got %v %v", got, last)
	}
}
