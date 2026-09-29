package handlers

import (
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/outletpolicy"
)

// ListServiceProfiles handles GET /{tenantID}/pos/service-profiles.
//
// Returns the services sub use case registry (printing, salon, garage, ...) so pos-ui renders the
// profile picker, the job form spec fields and the production stages from the backend's single
// definition instead of a client-side copy.
//
// @Summary List services sub use cases (service profiles)
// @Tags settings
// @Produce json
// @Success 200 {object} map[string]any
// @Router /{tenantID}/pos/service-profiles [get]
func (h *ServiceSettingsHandler) ListServiceProfiles(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]any{"data": outletpolicy.ServiceProfiles()})
}

type serviceProfileInput struct {
	ServiceProfile    *string  `json:"service_profile"`
	JobDepositPercent *float64 `json:"job_deposit_percent"`
}

// PatchServiceProfile handles PATCH /{tenantID}/pos/settings/service-profile.
//
// Sets a services outlet's sub use case and its default job deposit. Applying a profile also
// switches on what its workflow needs: a job profile turns on the production board (enable_kds)
// and creates the profile's default production station when the outlet has none; an appointment
// profile turns on appointments. Existing stations and toggles an admin already configured are
// never removed.
//
// @Summary Set a services outlet's service profile and default job deposit
// @Tags settings
// @Accept json
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Router /{tenantID}/pos/settings/service-profile [patch]
func (h *ServiceSettingsHandler) PatchServiceProfile(w http.ResponseWriter, r *http.Request) {
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
	useCase := ""
	if outlet.UseCase != nil {
		useCase = *outlet.UseCase
	}
	if outletpolicy.NormalizeUseCase(useCase) != outletpolicy.UseCaseServices {
		jsonError(w, "service profiles apply to services outlets only", http.StatusBadRequest)
		return
	}

	var input serviceProfileInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var profile outletpolicy.ServiceProfile
	if input.ServiceProfile != nil {
		p, ok := outletpolicy.LookupServiceProfile(*input.ServiceProfile)
		if !ok {
			jsonError(w, "unknown service_profile", http.StatusBadRequest)
			return
		}
		profile = p
	}
	if input.JobDepositPercent != nil && (*input.JobDepositPercent < 0 || *input.JobDepositPercent > 100) {
		jsonError(w, "job_deposit_percent must be between 0 and 100", http.StatusBadRequest)
		return
	}

	setting, err := h.getOrCreateSetting(r, outlet.ID)
	if err != nil {
		h.log.Error("get settings for service profile", zap.Error(err))
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}

	meta := map[string]any{}
	for k, v := range setting.Metadata {
		meta[k] = v
	}
	upd := setting.Update()
	if input.ServiceProfile != nil {
		meta[outletpolicy.MetaKeyServiceProfile] = profile.Key
		switch profile.Workflow {
		case outletpolicy.WorkflowJob:
			upd = upd.SetEnableKds(true)
		case outletpolicy.WorkflowAppointment:
			upd = upd.SetEnableAppointments(true)
		}
	}
	if input.JobDepositPercent != nil {
		meta[outletpolicy.MetaKeyJobDepositPercent] = *input.JobDepositPercent
	}
	updated, err := upd.SetMetadata(meta).Save(r.Context())
	if err != nil {
		h.log.Error("patch service profile", zap.Error(err))
		jsonError(w, "failed to save service profile", http.StatusInternalServerError)
		return
	}
	if input.ServiceProfile != nil && profile.Workflow == outletpolicy.WorkflowJob {
		if err := orders.EnsureProfileStations(r.Context(), h.db, tid, outlet.ID, profile); err != nil {
			// The profile is saved; stations can still be added by hand on the board settings.
			h.log.Warn("create default production stations", zap.Error(err),
				zap.String("outlet_id", outlet.ID.String()), zap.String("profile", profile.Key))
		}
	}
	outletmw.InvalidateOutletSetting(outlet.ID)
	jsonOK(w, toSettingsResponse(outlet, updated))
}
