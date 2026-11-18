package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/serviceconfig"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
)

// Idle sign-out on POS terminals is configurable per role, so a chef on the KDS screen is not
// signed out every five minutes while a cashier's till still locks quickly.
//
// Resolution, most specific first:
//  1. the outlet's own values (outlet_settings.metadata, edited by the tenant admin)
//  2. the tenant row, then the platform row, of service_config pos.idle_timeout_by_role and
//     pos.screensaver_idle_timeout_seconds
//  3. defaultIdleTimeoutByRole, only when no platform row exists yet
//
// Values are seconds. 0 for a role means "never sign out on idle".

// ConfigKeyIdleTimeoutByRole is the service_config key holding a JSON object of role code to
// idle seconds, e.g. {"kitchen":3600,"bar":3600}.
const ConfigKeyIdleTimeoutByRole = "pos.idle_timeout_by_role"

const (
	metaIdleTimeoutByRole      = "idle_timeout_by_role"
	metaIdleTimeoutDefault     = "idle_timeout_default_seconds"
	metaIdleCountdownSeconds   = "idle_countdown_seconds"
	defaultIdleCountdownSecond = 30
	maxIdleSeconds             = 24 * 3600
)

// defaultIdleTimeoutByRole keeps kitchen and bar screens signed in for an hour when the platform
// has not seeded its own row yet.
var defaultIdleTimeoutByRole = map[string]int{"kitchen": 3600, "bar": 3600, "barista": 3600}

// sessionTimeouts is what the terminal needs to run its idle timer.
type sessionTimeouts struct {
	ScreensaverTimeoutSeconds int            `json:"screensaver_timeout_seconds"`
	IdleTimeoutByRole         map[string]int `json:"idle_timeout_by_role"`
	IdleCountdownSeconds      int            `json:"idle_countdown_seconds"`
}

// serviceConfigValue returns the tenant override of a service_config key, else the platform row.
// found is false when neither row exists.
func serviceConfigValue(ctx context.Context, client *ent.Client, tenantID uuid.UUID, key string) (value string, found bool) {
	rows, err := client.ServiceConfig.Query().
		Where(
			serviceconfig.ConfigKey(key),
			serviceconfig.Or(serviceconfig.TenantID(tenantID), serviceconfig.TenantIDIsNil()),
		).
		All(ctx)
	if err != nil || len(rows) == 0 {
		return "", false
	}
	var platform, tenant string
	var hasPlatform, hasTenant bool
	for _, row := range rows {
		if row.TenantID != nil && *row.TenantID == tenantID {
			tenant, hasTenant = row.ConfigValue, true
		} else if row.TenantID == nil {
			platform, hasPlatform = row.ConfigValue, true
		}
	}
	if hasTenant && strings.TrimSpace(tenant) != "" {
		return tenant, true
	}
	return platform, hasPlatform
}

// parseRoleSeconds reads a role to seconds object, dropping blank roles and out-of-range values.
func parseRoleSeconds(raw string) map[string]int {
	out := map[string]int{}
	var in map[string]any
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return out
	}
	return cleanRoleSeconds(in)
}

func cleanRoleSeconds(in map[string]any) map[string]int {
	out := map[string]int{}
	for role, v := range in {
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "" {
			continue
		}
		secs, ok := toSeconds(v)
		if !ok {
			continue
		}
		out[role] = secs
	}
	return out
}

// toSeconds accepts JSON numbers and numeric strings within 0..maxIdleSeconds.
func toSeconds(v any) (int, bool) {
	var n int
	switch t := v.(type) {
	case float64:
		n = int(t)
	case int:
		n = t
	case string:
		p, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		n = p
	default:
		return 0, false
	}
	if n < 0 || n > maxIdleSeconds {
		return 0, false
	}
	return n, true
}

// resolveSessionTimeouts merges platform, tenant and outlet values for one outlet.
func resolveSessionTimeouts(ctx context.Context, client *ent.Client, tenantID uuid.UUID, setting *ent.OutletSetting) sessionTimeouts {
	out := sessionTimeouts{
		ScreensaverTimeoutSeconds: resolveScreensaverTimeoutSeconds(ctx, client, tenantID),
		IdleTimeoutByRole:         map[string]int{},
		IdleCountdownSeconds:      defaultIdleCountdownSecond,
	}
	if raw, found := serviceConfigValue(ctx, client, tenantID, ConfigKeyIdleTimeoutByRole); found {
		out.IdleTimeoutByRole = parseRoleSeconds(raw)
	} else {
		for role, secs := range defaultIdleTimeoutByRole {
			out.IdleTimeoutByRole[role] = secs
		}
	}
	if setting == nil || setting.Metadata == nil {
		return out
	}
	if m, ok := setting.Metadata[metaIdleTimeoutByRole].(map[string]any); ok {
		for role, secs := range cleanRoleSeconds(m) {
			out.IdleTimeoutByRole[role] = secs
		}
	}
	if secs, ok := toSeconds(setting.Metadata[metaIdleTimeoutDefault]); ok && secs > 0 {
		out.ScreensaverTimeoutSeconds = secs
	}
	if secs, ok := toSeconds(setting.Metadata[metaIdleCountdownSeconds]); ok && secs >= 5 && secs <= 300 {
		out.IdleCountdownSeconds = secs
	}
	return out
}

// sessionTimeoutsInput for PATCH /settings/session-timeouts. A nil field is left unchanged; a
// non-nil role map replaces the outlet's stored map (a role left out falls back to the platform).
type sessionTimeoutsInput struct {
	IdleTimeoutByRole         *map[string]int `json:"idle_timeout_by_role"`
	IdleTimeoutDefaultSeconds *int            `json:"idle_timeout_default_seconds"`
	IdleCountdownSeconds      *int            `json:"idle_countdown_seconds"`
}

// PatchSessionTimeouts handles PATCH /{tenantID}/pos/settings/session-timeouts. Tenant admins
// (pos.config.manage) set how long each role stays signed in while idle at this outlet.
func (h *ServiceSettingsHandler) PatchSessionTimeouts(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, true) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	outlet, err := h.resolveOutlet(r, tid)
	if err != nil {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}
	var input sessionTimeoutsInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.IdleTimeoutDefaultSeconds != nil {
		if v := *input.IdleTimeoutDefaultSeconds; v != 0 && (v < 30 || v > maxIdleSeconds) {
			jsonError(w, "idle_timeout_default_seconds must be 0 or between 30 and 86400", http.StatusBadRequest)
			return
		}
	}
	if input.IdleCountdownSeconds != nil {
		if v := *input.IdleCountdownSeconds; v < 5 || v > 300 {
			jsonError(w, "idle_countdown_seconds must be between 5 and 300", http.StatusBadRequest)
			return
		}
	}
	roles := map[string]any{}
	if input.IdleTimeoutByRole != nil {
		for role, secs := range *input.IdleTimeoutByRole {
			role = strings.ToLower(strings.TrimSpace(role))
			if role == "" {
				continue
			}
			if secs != 0 && (secs < 30 || secs > maxIdleSeconds) {
				jsonError(w, "each role timeout must be 0 (never) or between 30 and 86400 seconds", http.StatusBadRequest)
				return
			}
			roles[role] = secs
		}
	}

	setting, err := h.getOrCreateSetting(r, outlet.ID)
	if err != nil {
		h.log.Error("get settings for session timeouts", zap.Error(err))
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}
	meta := map[string]any{}
	for k, v := range setting.Metadata {
		meta[k] = v
	}
	if input.IdleTimeoutByRole != nil {
		meta[metaIdleTimeoutByRole] = roles
	}
	if input.IdleTimeoutDefaultSeconds != nil {
		if *input.IdleTimeoutDefaultSeconds == 0 {
			delete(meta, metaIdleTimeoutDefault)
		} else {
			meta[metaIdleTimeoutDefault] = *input.IdleTimeoutDefaultSeconds
		}
	}
	if input.IdleCountdownSeconds != nil {
		meta[metaIdleCountdownSeconds] = *input.IdleCountdownSeconds
	}
	updated, err := setting.Update().SetMetadata(meta).Save(r.Context())
	if err != nil {
		h.log.Error("patch session timeouts", zap.Error(err))
		jsonError(w, "failed to save session timeouts", http.StatusInternalServerError)
		return
	}
	outletmw.InvalidateOutletSetting(outlet.ID)
	resp := toSettingsResponse(outlet, updated)
	resp.sessionTimeouts = resolveSessionTimeouts(r.Context(), h.db, tid, updated)
	jsonOK(w, resp)
}
