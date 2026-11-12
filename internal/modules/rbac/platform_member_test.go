package rbac

import (
	"testing"

	"github.com/google/uuid"
)

// On the platform tenant only an explicit POS role gives a POS role: a plain member, a generic
// staff or viewer role, or a role POS does not know gives none. Outside it nothing changes.
func TestMapGlobalRolesToServiceRoleInPlatformTenant(t *testing.T) {
	cases := []struct {
		roles []string
		want  string
	}{
		{[]string{"member"}, ""},
		{[]string{"staff"}, ""},
		{[]string{"viewer"}, ""},
		{[]string{"support_agent"}, ""},
		{[]string{"member", "cashier"}, "cashier"},
		{[]string{"member", "manager"}, "manager"},
		{[]string{"superuser"}, "admin"},
		{[]string{"technician"}, "technician"},
		{[]string{"customer"}, ""},
	}
	for _, c := range cases {
		if got := MapGlobalRolesToServiceRoleIn(c.roles, true); got != c.want {
			t.Errorf("platform tenant %v = %q, want %q", c.roles, got, c.want)
		}
	}
}

func TestMapGlobalRolesToServiceRoleInBusinessTenant(t *testing.T) {
	for _, c := range []struct {
		roles []string
		want  string
	}{
		{[]string{"member"}, "cashier"},
		{[]string{"staff"}, "cashier"},
		{[]string{"support_agent"}, "cashier"},
		{[]string{"customer"}, ""},
		{[]string{"manager"}, "manager"},
	} {
		if got := MapGlobalRolesToServiceRoleIn(c.roles, false); got != c.want {
			t.Errorf("business tenant %v = %q, want %q", c.roles, got, c.want)
		}
	}
}

// The permission middleware's role candidates on the platform tenant leave out the generic
// staff roles and the cashier default, so a platform member resolves no POS role at all.
func TestGlobalRoleCandidates(t *testing.T) {
	platform := uuid.New()
	s := &Service{}
	s.SetPlatformTenantID(platform.String())

	if got := s.globalRoleCandidates(platform, []string{"member"}); len(got) != 0 {
		t.Fatalf("platform member candidates = %v, want none", got)
	}
	got := s.globalRoleCandidates(platform, []string{"member", "cashier"})
	if len(got) != 1 || got[0] != "cashier" {
		t.Fatalf("platform member+cashier candidates = %v, want [cashier]", got)
	}
	got = s.globalRoleCandidates(uuid.New(), []string{"member"})
	if len(got) != 2 || got[0] != "member" || got[1] != "cashier" {
		t.Fatalf("business member candidates = %v, want [member cashier]", got)
	}

	// Without a platform tenant configured every tenant is a business tenant.
	unset := &Service{}
	unset.SetPlatformTenantID("not-a-uuid")
	if got := unset.globalRoleCandidates(platform, []string{"member"}); len(got) != 2 {
		t.Fatalf("unset platform tenant candidates = %v, want [member cashier]", got)
	}
}
