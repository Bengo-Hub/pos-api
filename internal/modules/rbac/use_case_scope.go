package rbac

// UseCase-scoping keeps the Roles & Permissions UI from showing pharmacy/services/retail-only
// modules and roles on a hospitality outlet (and vice versa). There is deliberately NO new DB
// column/migration here: the module set is a small, static, compile-time-known enumeration
// (mirrors pos-ui's lib/rbac/permissions.ts MODULES), so a static Go map is self-consistent and
// can never drift from seeded data the way a migrated + backfilled column could. "Common"
// modules (an empty/absent entry) are visible under every use case.

// moduleUseCases maps a permission MODULE (e.g. "hotel", "pharmacy") to the outlet use cases it
// applies to. A module absent from this map — or mapped to an empty slice — is COMMON and
// matches every use case. Only modules with strong, direct evidence in pos-ui's nav-config
// (buildNavGroups / USE_CASE_MODULES) are restricted here; everything else fails open so a
// legitimate permission is never hidden by a missing/incomplete mapping.
var moduleUseCases = map[string][]string{
	// Hospitality-exclusive (Hotel group, table service).
	"hotel":      {"hospitality"},
	"conference": {"hospitality"},
	// "promotions" is deliberately ABSENT (common): discounts are a cross-use-case
	// Sell surface since the happy-hour editor was folded into Sell → Discounts.
	"tables": {"hospitality"},
	// Shared between hospitality (table service + counter) and quick_service.
	"kds": {"hospitality", "quick_service"},
	// Shared between hospitality (dine-in reservations/room service) and services (bookings).
	"appointments": {"hospitality", "services"},
	"packages":     {"hospitality", "services"},
	// Retail-exclusive.
	"retail":  {"retail"},
	"layaway": {"retail"},
	// Shared between retail and services (both have a loyalty/commission model).
	"loyalty":     {"retail", "services"},
	"commissions": {"retail", "services"},
	// Pharmacy-exclusive.
	"pharmacy": {"pharmacy"},
}

// UseCasesForModule returns the use cases a permission module is scoped to, or nil for a
// common/unscoped module.
func UseCasesForModule(module string) []string {
	return moduleUseCases[module]
}

// ModuleMatchesUseCase reports whether a permission module should be visible for the given
// outlet use case. An empty useCase (no filter requested) always matches.
func ModuleMatchesUseCase(module, useCase string) bool {
	if useCase == "" {
		return true
	}
	scopes := moduleUseCases[module]
	if len(scopes) == 0 {
		return true // common module — visible everywhere
	}
	for _, uc := range scopes {
		if uc == useCase {
			return true
		}
	}
	return false
}

// systemRoleUseCases tags each SEEDED system role code with the use case(s) it was designed
// for (see cmd/seed/main.go seedRBACRoles). A code absent here (including admin/manager/
// cashier/viewer/accountant) is treated as common — visible under every use case, matching
// their cross-cutting seeded permission grants.
var systemRoleUseCases = map[string][]string{
	"waiter":              {"hospitality"},
	"floor_supervisor":    {"hospitality"},
	"kitchen":             {"hospitality", "quick_service"},
	"bar":                 {"hospitality"},
	"receptionist":        {"hospitality"},
	"barista":             {"hospitality", "quick_service"},
	"stylist":             {"services"},
	"therapist":           {"services"},
	"technician":          {"services"},
	"pharmacist":          {"pharmacy"},
	"pharmacy_technician": {"pharmacy"},
}

// RoleUseCases resolves the use case(s) a role applies to. System roles use the static map
// above. Custom (tenant-created) roles are derived dynamically from the modules of their
// CURRENTLY granted permissions — a role with only common-module grants (or none yet) is
// common; a role spanning multiple restricted use cases is visible under all of them (fail
// open rather than hide a role someone deliberately built to span use cases).
func RoleUseCases(roleCode string, isSystemRole bool, grantedModules []string) []string {
	if isSystemRole {
		return systemRoleUseCases[roleCode]
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range grantedModules {
		for _, uc := range moduleUseCases[m] {
			if !seen[uc] {
				seen[uc] = true
				out = append(out, uc)
			}
		}
	}
	return out // nil when every granted module is common (or the role has no grants yet)
}

// RoleMatchesUseCase reports whether a role should be visible for the given outlet use case.
func RoleMatchesUseCase(scopes []string, useCase string) bool {
	if useCase == "" || len(scopes) == 0 {
		return true
	}
	for _, uc := range scopes {
		if uc == useCase {
			return true
		}
	}
	return false
}
