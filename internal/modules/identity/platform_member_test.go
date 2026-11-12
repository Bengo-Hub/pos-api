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
)

// A plain member of the platform tenant gets no POS staff row at sign-in: platform staff get POS
// only through an explicit POS role. An invited member of a business tenant still gets cashier.
func TestPlatformTenantMemberGetsNoPOSRole(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:posplatformmember_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	now := time.Now()
	mkTenant := func(slug string) uuid.UUID {
		id := uuid.New()
		client.Tenant.Create().SetID(id).SetName(slug).SetSlug(slug).SetStatus("active").
			SetSyncStatus("synced").SetTimezone("Africa/Nairobi").SetCreatedAt(now).SetUpdatedAt(now).ExecX(ctx)
		client.Outlet.Create().SetTenantID(id).SetTenantSlug(slug).SetCode("HQ").SetName("HQ").SetIsHq(true).
			SetUseCase("retail").ExecX(ctx)
		return id
	}
	platform, business := mkTenant("pm-platform"), mkTenant("pm-business")

	s := &Service{client: client, tenantSyncer: failingSyncer{}}
	s.SetPlatformTenantID(platform.String())

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
	signIn := func(slug string, roles ...string) uuid.UUID {
		t.Helper()
		person := uuid.New()
		if _, err := s.EnsureUserFromToken(ctx, person, slug, map[string]any{
			"email": person.String() + "@x.test", "roles": roles,
		}); err != nil {
			t.Fatal(err)
		}
		return person
	}

	for _, roles := range [][]string{{"member"}, {"staff"}, {"viewer"}, {"support_agent"}} {
		if p := signIn("pm-platform", roles...); rowIn(p, platform) != nil {
			t.Errorf("platform tenant %v must get no staff row", roles)
		}
	}
	if p := signIn("pm-platform", "member", "cashier"); rowIn(p, platform) == nil || rowIn(p, platform).Role != "cashier" {
		t.Error("an explicit cashier role on the platform tenant gives a cashier row")
	}
	if p := signIn("pm-platform", "manager"); rowIn(p, platform) == nil || rowIn(p, platform).Role != "manager" {
		t.Error("an explicit manager role on the platform tenant gives a manager row")
	}
	if p := signIn("pm-business", "member"); rowIn(p, business) == nil || rowIn(p, business).Role != "cashier" {
		t.Error("an invited member of a business tenant keeps cashier")
	}
}

func TestExplicitPOSRole(t *testing.T) {
	for _, c := range []struct {
		roles []string
		want  string
	}{
		{[]string{"member"}, ""},
		{[]string{"staff"}, ""},
		{[]string{"viewer"}, ""},
		{[]string{"support_agent"}, ""},
		{[]string{"member", "accountant"}, "accountant"},
		{[]string{"superuser"}, "admin"},
	} {
		if got := explicitPOSRole(c.roles); got != c.want {
			t.Errorf("explicitPOSRole(%v) = %q, want %q", c.roles, got, c.want)
		}
	}
}
