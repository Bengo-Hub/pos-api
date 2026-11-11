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
	case "superuser", "superusers", "owner", "super_admin", "pos_admin", "tenant_admin", "system_admin", "administrator",
		"platform_owner", "platform_admin":
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

// Tier ranks a raw role: 2 for the admin tier (admin, owner, superuser, platform roles and their
// aliases), 1 for the manager tier (manager, store manager, supervisor), 0 for everyone else.
func Tier(role string) int {
	switch Canonicalize(role) {
	case "admin":
		return 2
	case "manager":
		return 1
	default:
		return 0
	}
}

// RaisesTier reports whether moving a staff row from one role to another puts it in a higher
// tier. A raise invalidates a PIN chosen while the row was in the lower tier.
func RaisesTier(from, to string) bool {
	return Tier(to) > Tier(from)
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
