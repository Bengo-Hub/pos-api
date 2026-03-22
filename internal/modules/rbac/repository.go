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
