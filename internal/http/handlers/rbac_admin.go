package handlers

import (
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/rbac"
)

// adminOverrideRoles may create/edit roles and manage role permissions.
var adminOverrideRoles = map[string]bool{
	"admin": true, "manager": true, "pos_admin": true, "store_manager": true,
	"owner": true, "super_admin": true, "superuser": true,
}

// canManageRBAC reports whether the caller may manage roles/permissions (admin OR manager
// level). Resolves the caller's effective role from claims + platform-owner/superuser flags.
// This is the entry gate; system-role protection (managers may NOT edit system roles) is an
// additional guardrail enforced by requireAdminForSystemRole below.
func (h *RBACHandler) canManageRBAC(w http.ResponseWriter, r *http.Request) bool {
	if adminOverrideRoles[requesterRole(r)] {
		return true
	}
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
		if claims.IsPlatformOwner {
			return true
		}
		for _, role := range claims.Roles {
			if adminOverrideRoles[role] {
				return true
			}
		}
	}
	respondJSON(w, http.StatusForbidden, map[string]string{"error": "insufficient permissions"})
	return false
}

// requireAdminForSystemRole enforces the guardrailed-manager policy: only an admin-level
// caller may create/modify/delete a SYSTEM role (admin, manager, cashier, …). A manager may
// only manage tenant-owned custom roles. Returns false (and writes 403) when a non-admin
// caller targets a system role. `isSystem` is the role being acted on.
func (h *RBACHandler) requireAdminForSystemRole(w http.ResponseWriter, r *http.Request, isSystem bool) bool {
	if !isSystem {
		return true
	}
	if requesterIsAdminLevel(r) {
		return true
	}
	respondJSON(w, http.StatusForbidden, map[string]string{
		"error": "only an administrator can modify system roles",
	})
	return false
}

type createPOSRoleRequest struct {
	RoleCode    string `json:"role_code"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CreateRole handles POST /rbac/roles — creates a tenant-scoped custom POS role.
func (h *RBACHandler) CreateRole(w http.ResponseWriter, r *http.Request) {
	if !h.canManageRBAC(w, r) {
		return
	}
	tid, err := uuid.Parse(httpware.GetTenantID(r.Context()))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tenant_id"})
		return
	}
	var req createPOSRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoleCode == "" || req.Name == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "role_code and name are required"})
		return
	}
	role := &rbac.POSRoleV2{
		ID:           uuid.New(),
		TenantID:     &tid,
		RoleCode:     req.RoleCode,
		Name:         req.Name,
		IsSystemRole: false,
	}
	if req.Description != "" {
		role.Description = &req.Description
	}
	if err := h.rbacRepo.CreateRole(r.Context(), tid, role); err != nil {
		h.logger.Error("create role failed", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create role"})
		return
	}
	respondJSON(w, http.StatusCreated, role)
}

// GetRolePermissions handles GET /rbac/roles/{roleID}/permissions.
func (h *RBACHandler) GetRolePermissions(w http.ResponseWriter, r *http.Request) {
	roleID, err := uuid.Parse(chi.URLParam(r, "roleID"))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role id"})
		return
	}
	perms, err := h.rbacRepo.GetRolePermissions(r.Context(), roleID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load role permissions"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"data": perms, "permissions": perms})
}

type setPOSRolePermissionsRequest struct {
	PermissionIDs []uuid.UUID `json:"permission_ids"`
}

// SetRolePermissions handles PUT /rbac/roles/{roleID}/permissions — replaces a
// role's permission set (the permission-matrix save).
func (h *RBACHandler) SetRolePermissions(w http.ResponseWriter, r *http.Request) {
	if !h.canManageRBAC(w, r) {
		return
	}
	roleID, err := uuid.Parse(chi.URLParam(r, "roleID"))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role id"})
		return
	}
	ctx := r.Context()
	// Guardrail: a manager may not rewrite the permission matrix of a SYSTEM role
	// (admin/manager/cashier/…) — only an admin may. This blocks a manager from, e.g.,
	// granting the cashier role admin-level permissions or de-fanging the admin role.
	tid, terr := uuid.Parse(httpware.GetTenantID(ctx))
	if terr == nil {
		if role, gerr := h.rbacRepo.GetRole(ctx, tid, roleID); gerr == nil {
			if !h.requireAdminForSystemRole(w, r, role.IsSystemRole) {
				return
			}
		}
	}
	var req setPOSRolePermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	current, err := h.rbacRepo.GetRolePermissions(ctx, roleID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load role permissions"})
		return
	}
	have := make(map[uuid.UUID]bool, len(current))
	for _, p := range current {
		have[p.ID] = true
	}
	want := make(map[uuid.UUID]bool, len(req.PermissionIDs))
	for _, id := range req.PermissionIDs {
		want[id] = true
	}
	for id := range want {
		if !have[id] {
			if err := h.rbacRepo.AssignPermissionToRole(ctx, roleID, id); err != nil {
				h.logger.Warn("assign permission failed", zap.String("permission", id.String()), zap.Error(err))
			}
		}
	}
	for id := range have {
		if !want[id] {
			if err := h.rbacRepo.RemovePermissionFromRole(ctx, roleID, id); err != nil {
				h.logger.Warn("remove permission failed", zap.String("permission", id.String()), zap.Error(err))
			}
		}
	}
	respondJSON(w, http.StatusOK, map[string]string{"message": "role permissions updated"})
}

type updatePOSRoleRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

// UpdateRole handles PATCH /rbac/roles/{roleID} — edits a custom role's display name /
// description (role_code is immutable). System roles are edit-protected (admin only, and the
// repository refuses to touch system rows anyway).
func (h *RBACHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	if !h.canManageRBAC(w, r) {
		return
	}
	tid, err := uuid.Parse(httpware.GetTenantID(r.Context()))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tenant_id"})
		return
	}
	roleID, err := uuid.Parse(chi.URLParam(r, "roleID"))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role id"})
		return
	}
	role, err := h.rbacRepo.GetRole(r.Context(), tid, roleID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "role not found"})
		return
	}
	if !h.requireAdminForSystemRole(w, r, role.IsSystemRole) {
		return
	}
	var req updatePOSRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := h.rbacRepo.UpdateRole(r.Context(), tid, roleID, req.Name, req.Description); err != nil {
		h.logger.Warn("update role failed", zap.Error(err))
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to update role (system roles are not editable)"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"message": "role updated"})
}

// DeleteRole handles DELETE /rbac/roles/{roleID} — removes a tenant-owned custom role and its
// permission grants + user assignments. System/global roles cannot be deleted.
func (h *RBACHandler) DeleteRole(w http.ResponseWriter, r *http.Request) {
	if !h.canManageRBAC(w, r) {
		return
	}
	tid, err := uuid.Parse(httpware.GetTenantID(r.Context()))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tenant_id"})
		return
	}
	roleID, err := uuid.Parse(chi.URLParam(r, "roleID"))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role id"})
		return
	}
	role, err := h.rbacRepo.GetRole(r.Context(), tid, roleID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "role not found"})
		return
	}
	if role.IsSystemRole {
		respondJSON(w, http.StatusForbidden, map[string]string{"error": "system roles cannot be deleted"})
		return
	}
	// Custom roles are tenant-owned; deleting one is an admin-or-manager action but never a
	// system-role touch, so no extra admin-level gate is required beyond canManageRBAC.
	if err := h.rbacRepo.DeleteRole(r.Context(), tid, roleID); err != nil {
		h.logger.Warn("delete role failed", zap.Error(err))
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to delete role"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"message": "role deleted"})
}
