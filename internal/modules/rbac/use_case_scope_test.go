package rbac

import "testing"

func TestSystemRoleUseCases(t *testing.T) {
	cases := []struct {
		role    string
		useCase string
		want    bool
	}{
		// The front desk exists on hospitality, services and retail outlets.
		{"receptionist", "hospitality", true},
		{"receptionist", "services", true},
		{"receptionist", "retail", true},
		{"receptionist", "quick_service", false},
		// Table service stays hospitality-only.
		{"waiter", "services", false},
		{"bar", "retail", false},
		// Trade specialists stay on services outlets.
		{"technician", "services", true},
		{"technician", "hospitality", false},
		{"stylist", "retail", false},
		// Cross-cutting roles show everywhere.
		{"cashier", "services", true},
		{"manager", "hospitality", true},
	}
	for _, c := range cases {
		got := RoleMatchesUseCase(RoleUseCases(c.role, true, nil), c.useCase)
		if got != c.want {
			t.Errorf("%s on %s = %v, want %v", c.role, c.useCase, got, c.want)
		}
	}
}

func TestHotelModuleIsHospitalityOnly(t *testing.T) {
	for _, uc := range []string{"retail", "services", "quick_service"} {
		if ModuleMatchesUseCase("hotel", uc) || ModuleMatchesUseCase("conference", uc) {
			t.Errorf("hotel permissions must not show on a %s outlet", uc)
		}
	}
}

func TestMapGlobalRolesToServiceRoleKeepsTradeRoles(t *testing.T) {
	for _, r := range []string{"technician", "stylist", "therapist", "barista", "pharmacist", "receptionist"} {
		if got := MapGlobalRolesToServiceRole([]string{r}); got != r {
			t.Errorf("MapGlobalRolesToServiceRole(%q) = %q, want %q", r, got, r)
		}
	}
}
