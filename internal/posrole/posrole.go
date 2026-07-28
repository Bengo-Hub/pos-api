// Package posrole canonicalizes POS staff role names so admin/manager-tier access checks are
// consistent across packages (PIN auth, outlet-switch, outlet-context middleware) regardless of
// whether a role arrives as a current POS role name, a legacy alias, or an SSO/global role.
package posrole

import "strings"

// Canonicalize folds SSO/global role aliases and legacy POS role names onto the current POS
// role vocabulary (admin, manager, cashier, ...). Mirrors mapGlobalRoleToPOSRole's aliasing in
// internal/modules/identity/service.go.
func Canonicalize(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "superuser", "owner", "super_admin", "pos_admin", "tenant_admin", "system_admin", "administrator":
		return "admin"
	case "store_manager", "outlet_manager", "supervisor":
		return "manager"
	case "staff":
		return "cashier"
	default:
		return strings.ToLower(strings.TrimSpace(role))
	}
}

// IsAdminLevel reports whether a raw role (current name, legacy alias, or SSO/global role)
// canonicalizes to admin or manager — the tiers that may access/switch to any outlet in the
// tenant rather than only outlets a StaffOutlet row assigns them to.
func IsAdminLevel(role string) bool {
	c := Canonicalize(role)
	return c == "admin" || c == "manager"
}

// IsAdminLevelAny reports whether any role in the list is admin/manager tier.
func IsAdminLevelAny(roles []string) bool {
	for _, r := range roles {
		if IsAdminLevel(r) {
			return true
		}
	}
	return false
}
