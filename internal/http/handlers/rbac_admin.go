package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/rbac"
	"github.com/bengobox/pos-service/internal/posrole"
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

// permissionsCallerLacks returns the codes the caller does not hold in tenant tid, resolved the
// way route permissions are (token permissions, then per-user assignments, then the POS role
// mapped from the caller's global roles). Platform owners and superusers hold everything. Any
// lookup failure counts as not held, so the ceiling fails closed.
func (h *RBACHandler) permissionsCallerLacks(r *http.Request, tid uuid.UUID, codes []string) []string {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil {
		return codes
	}
	if claims.IsPlatformOwner || claims.IsSuperuser() {
		return nil
	}
	uid, _ := uuid.Parse(claims.Subject)
	var missing []string
	for _, code := range codes {
		if claims.HasAnyPermission(code) {
			continue
		}
		if h.rbacService != nil {
			if uid != uuid.Nil {
				if has, err := h.rbacService.HasAnyPermission(r.Context(), tid, uid, code); err == nil && has {
					continue
				}
			}
			if len(claims.Roles) > 0 {
				if has, err := h.rbacService.HasAnyPermissionViaGlobalRoles(r.Context(), tid, claims.Roles, code); err == nil && has {
					continue
				}
			}
		}
		missing = append(missing, code)
	}
	return missing
}

// roleAboveStaff reports whether a role in the tenant's scope is above staff tier: by its code,
// or for a custom role or a tenant copy of a system role by its permissions (rbac.RoleTier).
// A caller below admin may only author, edit, assign, revoke or delete staff-tier roles.
// Returns rbac.ErrRoleNotInTenant for a role the tenant cannot see.
func (h *RBACHandler) roleAboveStaff(r *http.Request, tid, roleID uuid.UUID) (bool, error) {
	tier, err := h.rbacRepo.RoleTier(r.Context(), tid, roleID)
	if err != nil {
		return false, err
	}
	return tier > 0, nil
}

// reservedRoleKeys are the role codes (normalised by roleCodeKey) a tenant may never create as a
// custom role: the system tiers, their aliases and the platform roles. A tenant row with one of
// these codes would win over the shared system role of the same code for every holder of it.
var reservedRoleKeys = map[string]bool{
	"admin": true, "administrator": true, "owner": true, "superuser": true, "superusers": true,
	"superadmin": true, "posadmin": true, "tenantadmin": true, "systemadmin": true,
	"platformadmin": true, "platformowner": true,
	"manager": true, "storemanager": true, "outletmanager": true, "supervisor": true,
	"cashier": true, "staff": true,
	// auth-api's self-service roles (customer, student, parent, driver): anyone may take them at
	// sign-up, so a tenant role with one of these codes would hand its permissions to every
	// customer. Plus "member" (auth's default for an invited member with no roles, which POS maps
	// to cashier) and the other customer-side names in use across the platform.
	"member": true, "customer": true, "student": true, "parent": true, "driver": true,
	"rider": true, "learner": true, "occupant": true, "viewer": true,
}

// roleCodeKey folds a role code for reserved-name comparison: lower case, without spaces,
// hyphens or underscores ("Super-Admin", "super_admin" and "superadmin" are one key).
func roleCodeKey(code string) string {
	return strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(code)))
}

// reservedRoleCode reports whether a custom role may not use code: a reserved key, any code that
// posrole folds onto another role (an alias), any admin or manager tier code, or the code of a
// shared system role (cashier, waiter, kitchen and the rest), compared on the folded key.
func (h *RBACHandler) reservedRoleCode(r *http.Request, tid uuid.UUID, code string) (bool, error) {
	key := roleCodeKey(code)
	if key == "" || reservedRoleKeys[key] || posrole.Tier(code) > 0 ||
		roleCodeKey(posrole.Canonicalize(code)) != key {
		return true, nil
	}
	roles, err := h.rbacRepo.ListRoles(r.Context(), tid)
	if err != nil {
		return false, err
	}
	for _, role := range roles {
		if role.IsSystemRole && roleCodeKey(role.RoleCode) == key {
			return true, nil
		}
	}
	return false, nil
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
	// A custom role may not take a system or platform role's code (or an alias of one): the
	// tenant row would win over the shared role for everyone who holds that code.
	reserved, rerr := h.reservedRoleCode(r, tid, req.RoleCode)
	if rerr != nil {
		h.logger.Error("create role: list roles", zap.Error(rerr))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create role"})
		return
	}
	if reserved {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "role_code is reserved for a system role; choose another code",
		})
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
	// Best-effort: register the new tenant custom role in auth's shared Role registry (scope "pos")
	// so it's assignable via auth-ui (SSO) too. Idempotent upsert by role_code on the auth side.
	// Never blocks role creation — pos resolves custom roles locally by role_code regardless.
	desc := ""
	if role.Description != nil {
		desc = *role.Description
	}
	if perr := rbac.PushRolesToAuthRegistry(r.Context(), h.authURL, h.authAPIKey, []rbac.RegistryRole{
		{RoleCode: role.RoleCode, Name: role.Name, Description: desc, Scope: "pos"},
	}); perr != nil {
		h.logger.Warn("push custom role to auth registry failed (non-fatal)", zap.Error(perr), zap.String("role_code", role.RoleCode))
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
	ctx := r.Context()
	// Resolve to the tenant's EFFECTIVE role: a per-tenant override (copy-on-write) wins over the
	// shared global row of the same code. This keeps the matrix showing what the tenant actually
	// grants even when the client is still holding the global role id (before it refetches the list).
	if tid, terr := uuid.Parse(httpware.GetTenantID(ctx)); terr == nil {
		if role, gerr := h.rbacRepo.GetRole(ctx, tid, roleID); gerr == nil {
			if eff, eerr := h.rbacRepo.GetRoleByCode(ctx, tid, role.RoleCode); eerr == nil {
				roleID = eff.ID
			}
		}
	}
	perms, err := h.rbacRepo.GetRolePermissions(ctx, roleID)
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
	if terr != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tenant_id"})
		return
	}
	// Fail closed: a role this tenant cannot see (another tenant's) is not found, and the system
	// role guard is never skipped.
	role, gerr := h.rbacRepo.GetRole(ctx, tid, roleID)
	if gerr != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "role not found"})
		return
	}
	if !h.requireAdminForSystemRole(w, r, role.IsSystemRole) {
		return
	}
	var req setPOSRolePermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	// A caller below admin edits staff-tier roles only, and must leave them staff tier:
	//  - a role already above staff (by code or permissions) is refused, so a manager cannot
	//    shrink an admin-built role and demote its holders;
	//  - an edit that would lift the role above staff is refused, so a manager cannot turn a
	//    role they already assigned (and whose holders' PINs they may know) into a manager one.
	// Ceiling: and they may not leave the role carrying a permission they do not hold themselves
	// (the same idea as treasury's checkGrant).
	if !requesterIsAdminLevel(r) {
		above, aerr := h.roleAboveStaff(r, tid, role.ID)
		if aerr != nil {
			h.logger.Error("set role permissions: role tier", zap.Error(aerr))
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not check the role"})
			return
		}
		if above {
			respondJSON(w, http.StatusForbidden, map[string]string{"error": "only an admin can edit a manager or admin role"})
			return
		}
		codes := make([]string, 0, len(req.PermissionIDs))
		for _, id := range req.PermissionIDs {
			p, perr := h.rbacRepo.GetPermission(ctx, id)
			if perr != nil {
				respondJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown permission " + id.String()})
				return
			}
			codes = append(codes, p.PermissionCode)
		}
		after, terr := h.rbacRepo.PermissionsTier(ctx, role.RoleCode, codes)
		if terr != nil {
			h.logger.Error("set role permissions: tier after edit", zap.Error(terr))
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not check the role"})
			return
		}
		if after > 0 {
			respondJSON(w, http.StatusForbidden, map[string]string{
				"error": "only an admin can give a role manager or admin permissions",
			})
			return
		}
		if missing := h.permissionsCallerLacks(r, tid, codes); len(missing) > 0 {
			respondJSON(w, http.StatusForbidden, map[string]any{
				"error":   "you cannot give a role permissions you do not hold",
				"missing": missing,
			})
			return
		}
	}
	// Copy-on-write: a shared/global role (tenant_id NULL) is the platform-wide default and must
	// NEVER be mutated by a tenant's matrix edit — that would change every other tenant's users of
	// the same role. Materialize (or reuse) a per-tenant override of the role and apply the edit
	// there. The read path (resolveRolePermissions/GetRoleByCode/ListRoles) already prefers the
	// tenant override, and the seed never touches it — so the edit takes effect immediately and is
	// durable across deploys. A role that is already tenant-owned is edited in place unchanged.
	effectiveRoleID, err := h.rbacRepo.EnsureTenantRoleOverride(ctx, tid, roleID)
	if err != nil {
		h.logger.Error("ensure tenant role override failed", zap.Error(err), zap.String("role_id", roleID.String()))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scope role permissions to tenant"})
		return
	}
	current, err := h.rbacRepo.GetRolePermissions(ctx, effectiveRoleID)
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
			if err := h.rbacRepo.AssignPermissionToRole(ctx, effectiveRoleID, id); err != nil {
				h.logger.Warn("assign permission failed", zap.String("permission", id.String()), zap.Error(err))
			}
		}
	}
	for id := range have {
		if !want[id] {
			if err := h.rbacRepo.RemovePermissionFromRole(ctx, effectiveRoleID, id); err != nil {
				h.logger.Warn("remove permission failed", zap.String("permission", id.String()), zap.Error(err))
			}
		}
	}
	// Return the effective (possibly newly-materialized tenant-override) role id so the UI can
	// re-target its matrix at the override instead of the shared global row.
	respondJSON(w, http.StatusOK, map[string]any{
		"message": "role permissions updated",
		"role_id": effectiveRoleID.String(),
	})
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
	if !requesterIsAdminLevel(r) {
		above, aerr := h.roleAboveStaff(r, tid, roleID)
		if aerr != nil {
			h.logger.Error("update role: role tier", zap.Error(aerr))
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not check the role"})
			return
		}
		if above {
			respondJSON(w, http.StatusForbidden, map[string]string{"error": "only an admin can edit a manager or admin role"})
			return
		}
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
	// Custom roles are tenant-owned; a manager may delete a staff-tier one. Deleting a role above
	// staff tier (by its permissions) demotes everyone holding it, so that is admin only.
	if !requesterIsAdminLevel(r) {
		above, aerr := h.roleAboveStaff(r, tid, roleID)
		if aerr != nil {
			h.logger.Error("delete role: role tier", zap.Error(aerr))
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not check the role"})
			return
		}
		if above {
			respondJSON(w, http.StatusForbidden, map[string]string{"error": "only an admin can delete a manager or admin role"})
			return
		}
	}
	if err := h.rbacRepo.DeleteRole(r.Context(), tid, roleID); err != nil {
		h.logger.Warn("delete role failed", zap.Error(err))
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to delete role"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"message": "role deleted"})
}
