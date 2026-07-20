package rbac

import (
	"context"

	"github.com/google/uuid"
)

// Repository abstracts persistence for RBAC entities.
type Repository interface {
	// Role operations
	CreateRole(ctx context.Context, tenantID uuid.UUID, role *POSRoleV2) error
	GetRole(ctx context.Context, tenantID uuid.UUID, roleID uuid.UUID) (*POSRoleV2, error)
	GetRoleByCode(ctx context.Context, tenantID uuid.UUID, roleCode string) (*POSRoleV2, error)
	ListRoles(ctx context.Context, tenantID uuid.UUID) ([]*POSRoleV2, error)
	// UpdateRole edits a custom role's display name/description (role_code is immutable).
	UpdateRole(ctx context.Context, tenantID uuid.UUID, roleID uuid.UUID, name string, description *string) error
	// DeleteRole removes a tenant-owned custom role and its permission grants + user
	// assignments. It must never delete a global/system role.
	DeleteRole(ctx context.Context, tenantID uuid.UUID, roleID uuid.UUID) error
	// EnsureTenantRoleOverride returns the ID of the tenant-scoped override for the given role,
	// materializing it (copy-on-write) when the target is a shared/global role. If the role is
	// already tenant-owned it is returned unchanged. The new override is a copy of the global role
	// with its CURRENT permission grants cloned, so a tenant can then customize the matrix without
	// mutating the shared role (which would leak to every other tenant). The read path already
	// prefers the tenant override (ListRoles/GetRoleByCode/resolveRolePermissions).
	EnsureTenantRoleOverride(ctx context.Context, tenantID uuid.UUID, roleID uuid.UUID) (uuid.UUID, error)

	// Permission operations
	CreatePermission(ctx context.Context, permission *POSPermission) error
	GetPermission(ctx context.Context, permissionID uuid.UUID) (*POSPermission, error)
	GetPermissionByCode(ctx context.Context, permissionCode string) (*POSPermission, error)
	ListPermissions(ctx context.Context, filters PermissionFilters) ([]*POSPermission, error)

	// Role-Permission operations
	AssignPermissionToRole(ctx context.Context, roleID uuid.UUID, permissionID uuid.UUID) error
	RemovePermissionFromRole(ctx context.Context, roleID uuid.UUID, permissionID uuid.UUID) error
	GetRolePermissions(ctx context.Context, roleID uuid.UUID) ([]*POSPermission, error)

	// User-Role assignment operations
	AssignRoleToUser(ctx context.Context, tenantID uuid.UUID, assignment *UserRoleAssignment) error
	RevokeRoleFromUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, roleID uuid.UUID) error
	GetUserRoles(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*POSRoleV2, error)
	GetUserPermissions(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*POSPermission, error)
	ListUserAssignments(ctx context.Context, tenantID uuid.UUID, filters AssignmentFilters) ([]*UserRoleAssignment, error)
}

// PermissionFilters for listing permissions.
type PermissionFilters struct {
	Module *string
	Action *string
}

// AssignmentFilters for listing role assignments.
type AssignmentFilters struct {
	UserID *uuid.UUID
	RoleID *uuid.UUID
}
