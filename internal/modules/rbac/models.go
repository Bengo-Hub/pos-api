package rbac

import (
	"time"

	"github.com/google/uuid"
)

// POSUser represents a POS service user reference.
type POSUser struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	AuthServiceUserID uuid.UUID
	Email             string
	FullName          string
	Status            string
	SyncStatus        string
	SyncAt            *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// POSRoleV2 represents a POS service role.
type POSRoleV2 struct {
	ID           uuid.UUID  `json:"id"`
	TenantID     uuid.UUID  `json:"tenant_id"`
	RoleCode     string     `json:"role_code"`
	Name         string     `json:"name"`
	Description  *string    `json:"description,omitempty"`
	IsSystemRole bool       `json:"is_system_role"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// POSPermission represents a POS service permission.
type POSPermission struct {
	ID             uuid.UUID  `json:"id"`
	PermissionCode string     `json:"permission_code"`
	Name           string     `json:"name"`
	Module         string     `json:"module"`
	Action         string     `json:"action"`
	Resource       *string    `json:"resource,omitempty"`
	Description    *string    `json:"description,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// UserRoleAssignment represents a user role assignment.
type UserRoleAssignment struct {
	ID         uuid.UUID  `json:"id"`
	TenantID   uuid.UUID  `json:"tenant_id"`
	UserID     uuid.UUID  `json:"user_id"`
	RoleID     uuid.UUID  `json:"role_id"`
	AssignedBy uuid.UUID  `json:"assigned_by"`
	AssignedAt time.Time  `json:"assigned_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}
