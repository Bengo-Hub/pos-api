package posrole

import "testing"

func TestTierRanksStaffManagerAdmin(t *testing.T) {
	cases := map[string]int{
		"cashier": 0, "waiter": 0, "staff": 0, "viewer": 0, "": 0,
		"manager": 1, "store_manager": 1, " Supervisor ": 1,
		"admin": 2, "owner": 2, "superuser": 2, "SUPER_ADMIN": 2, "pos_admin": 2,
		"superusers": 2, "platform_owner": 2, "platform_admin": 2,
	}
	for role, want := range cases {
		if got := Tier(role); got != want {
			t.Errorf("Tier(%q) = %d; want %d", role, got, want)
		}
	}
}

func TestPlatformRolesAreAdminLevel(t *testing.T) {
	for _, role := range []string{"superusers", "platform_owner", "Platform_Admin"} {
		if !IsAdminLevel(role) {
			t.Errorf("IsAdminLevel(%q) = false; a platform role must count as admin level", role)
		}
	}
	if IsAdminLevelAny([]string{"cashier", "member"}) {
		t.Error("cashier and member are not admin level")
	}
	if !IsAdminLevelAny([]string{"member", "admin"}) {
		t.Error("a list holding admin is admin level")
	}
}

func TestRaisesTier(t *testing.T) {
	if !RaisesTier("cashier", "manager") || !RaisesTier("manager", "owner") || !RaisesTier("waiter", "admin") {
		t.Error("moving up a tier must count as a raise")
	}
	if RaisesTier("cashier", "waiter") || RaisesTier("admin", "owner") || RaisesTier("admin", "cashier") {
		t.Error("a move within a tier or down a tier is not a raise")
	}
}
