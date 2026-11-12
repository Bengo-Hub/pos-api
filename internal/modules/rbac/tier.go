package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/pospermission"
	"github.com/bengobox/pos-service/internal/ent/posrolev2"
	"github.com/bengobox/pos-service/internal/ent/posuserroleassignment"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/posrole"
)

// ErrRoleNotInTenant is returned when a role id names a role that belongs to another tenant.
// A tenant may only use the shared roles (tenant_id NULL) and its own.
var ErrRoleNotInTenant = errors.New("role does not belong to this tenant")

// RoleInTenantScope restricts a POSRoleV2 query to the roles tenantID may use: the shared
// system roles (tenant_id NULL) and the tenant's own roles. Every reader of role assignments
// applies it, so an assignment row pointing at another tenant's role grants nothing.
func RoleInTenantScope(tenantID uuid.UUID) predicate.POSRoleV2 {
	return posrolev2.Or(posrolev2.TenantID(tenantID), posrolev2.TenantIDIsNil())
}

// RoleTier is the tier a role grants (0 staff, 1 manager, 2 admin; see posrole.Tier). A shared
// system role (no tenant) is tiered by its code: those roles define the ceilings. Every other
// role, a custom role or a tenant's copy of a system role, is tiered by the higher of its code
// and its permissions: it is manager tier when it carries a permission no shared staff-tier
// system role has, and admin tier when it carries one no shared manager-tier system role has. A
// custom "floor_lead" holding pos.staff.manage is not a cashier role, and neither is a tenant's
// "cashier" copy an admin gave admin-grade permissions. The ceilings come from the shared system
// roles, which only the platform seed writes.
func RoleTier(ctx context.Context, client *ent.Client, role *ent.POSRoleV2) (int, error) {
	t := posrole.Tier(role.RoleCode)
	if t == 2 || (role.IsSystemRole && role.TenantID == nil) {
		return t, nil
	}
	perms, err := client.POSRoleV2.QueryPermissions(role).Select(pospermission.FieldPermissionCode).Strings(ctx)
	if err != nil {
		return 0, fmt.Errorf("read role permissions: %w", err)
	}
	if len(perms) == 0 {
		return t, nil
	}
	staffCeiling, managerCeiling, err := systemCeilings(ctx, client)
	if err != nil {
		return 0, err
	}
	for _, p := range perms {
		if !managerCeiling[p] {
			return 2, nil
		}
		if !staffCeiling[p] && t < 1 {
			t = 1
		}
	}
	return t, nil
}

// PermissionsTier is RoleTier for a bare permission set, as a role would have after an edit.
func PermissionsTier(ctx context.Context, client *ent.Client, roleCode string, perms []string) (int, error) {
	t := posrole.Tier(roleCode)
	if t == 2 || len(perms) == 0 {
		return t, nil
	}
	staffCeiling, managerCeiling, err := systemCeilings(ctx, client)
	if err != nil {
		return 0, err
	}
	for _, p := range perms {
		if !managerCeiling[p] {
			return 2, nil
		}
		if !staffCeiling[p] && t < 1 {
			t = 1
		}
	}
	return t, nil
}

// systemCeilings returns the permission codes held by the shared staff-tier system roles, and by
// the shared staff- and manager-tier system roles together.
func systemCeilings(ctx context.Context, client *ent.Client) (staff, manager map[string]bool, err error) {
	roles, err := client.POSRoleV2.Query().
		Where(posrolev2.TenantIDIsNil(), posrolev2.IsSystemRole(true)).
		WithPermissions().
		All(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read system role ceilings: %w", err)
	}
	staff, manager = map[string]bool{}, map[string]bool{}
	for _, r := range roles {
		tier := posrole.Tier(r.RoleCode)
		for _, p := range r.Edges.Permissions {
			if tier == 0 {
				staff[p.PermissionCode] = true
			}
			if tier <= 1 {
				manager[p.PermissionCode] = true
			}
		}
	}
	return staff, manager, nil
}

// RoleTier implements Repository.RoleTier.
func (r *EntRepository) RoleTier(ctx context.Context, tenantID uuid.UUID, roleID uuid.UUID) (int, error) {
	role, err := r.client.POSRoleV2.Query().Where(posrolev2.ID(roleID), RoleInTenantScope(tenantID)).Only(ctx)
	if ent.IsNotFound(err) {
		return 0, ErrRoleNotInTenant
	}
	if err != nil {
		return 0, fmt.Errorf("read role: %w", err)
	}
	return RoleTier(ctx, r.client, role)
}

// PermissionsTier implements Repository.PermissionsTier.
func (r *EntRepository) PermissionsTier(ctx context.Context, roleCode string, permissionCodes []string) (int, error) {
	return PermissionsTier(ctx, r.client, roleCode, permissionCodes)
}

// AssignedTier is the highest tier among the roles userID is assigned in tenantID, counting only
// roles in the tenant's scope (shared or its own), with excludeRole left out (uuid.Nil for none).
func AssignedTier(ctx context.Context, client *ent.Client, tenantID, userID, excludeRole uuid.UUID) (int, error) {
	if userID == uuid.Nil {
		return 0, nil
	}
	roleIDs, err := client.POSUserRoleAssignment.Query().
		Where(posuserroleassignment.TenantID(tenantID), posuserroleassignment.UserID(userID)).
		Select(posuserroleassignment.FieldRoleID).
		Strings(ctx)
	if err != nil {
		return 0, fmt.Errorf("read role assignments: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(roleIDs))
	for _, s := range roleIDs {
		if id, perr := uuid.Parse(s); perr == nil && id != excludeRole {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	roles, err := client.POSRoleV2.Query().Where(posrolev2.IDIn(ids...), RoleInTenantScope(tenantID)).All(ctx)
	if err != nil {
		return 0, fmt.Errorf("read assigned roles: %w", err)
	}
	best := 0
	for _, r := range roles {
		t, terr := RoleTier(ctx, client, r)
		if terr != nil {
			return 0, terr
		}
		if t > best {
			best = t
		}
	}
	return best, nil
}
