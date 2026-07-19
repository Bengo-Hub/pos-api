package rbac

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RegistryRole is one role in the catalogue pushed to auth-api's Role registry.
type RegistryRole struct {
	RoleCode    string `json:"role_code"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

// syncServiceRolesRequest mirrors auth-api's expected body for POST /api/v1/s2s/roles/sync.
type syncServiceRolesRequest struct {
	Service string         `json:"service"`
	Roles   []RegistryRole `json:"roles"`
}

// registryHTTPClient is a short-timeout client for the best-effort registry push.
var registryHTTPClient = &http.Client{Timeout: 15 * time.Second}

// PushRolesToAuthRegistry upserts pos's role catalogue into auth-api's shared Role registry so
// that (a) auth-ui can assign these roles to members and (b) global→service role resolution stays
// consistent. Auth reconciles by the UNIQUE `role_code` (update-or-create, never prune), so this
// is fully idempotent and safe to run on every seed/startup. Each role is scope-tagged "pos".
//
// Best-effort by design: a nil/empty authURL or apiKey is a no-op (skips silently, like erp/
// library), and a transport/HTTP error is returned for the caller to log — it must NEVER fail the
// seed or a role-create request, because resolution also works pos-side by role_code regardless.
func PushRolesToAuthRegistry(ctx context.Context, authURL, apiKey string, roles []RegistryRole) error {
	authURL = strings.TrimSpace(authURL)
	apiKey = strings.TrimSpace(apiKey)
	if authURL == "" || apiKey == "" || len(roles) == 0 {
		return nil
	}
	// Default the scope tag to "pos" for any role that didn't set it.
	for i := range roles {
		if roles[i].Scope == "" {
			roles[i].Scope = "pos"
		}
	}
	buf, err := json.Marshal(syncServiceRolesRequest{Service: "pos", Roles: roles})
	if err != nil {
		return err
	}
	url := strings.TrimRight(authURL, "/") + "/api/v1/s2s/roles/sync"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	resp, err := registryHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("role registry sync: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("role registry sync: HTTP %d", resp.StatusCode)
	}
	return nil
}
