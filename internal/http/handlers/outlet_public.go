package handlers

import (
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
)

// PublicOutletHandler serves outlet info for unauthenticated kiosk pages.
type PublicOutletHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewPublicOutletHandler(log *zap.Logger, client *ent.Client) *PublicOutletHandler {
	return &PublicOutletHandler{log: log, client: client}
}

type outletPublicItem struct {
	ID       string                `json:"id"`
	Code     string                `json:"code"`
	Name     string                `json:"name"`
	UseCase  string                `json:"use_case,omitempty"`
	IsHQ     bool                  `json:"is_hq"`
	Status   string                `json:"status"`
	Settings *outletSettingsPublic `json:"settings,omitempty"`
}

type outletSettingsPublic struct {
	PinLoginMessage           string   `json:"pin_login_message,omitempty"`
	ScreensaverURL            string   `json:"screensaver_url,omitempty"`
	ScreensaverURLs           []string `json:"screensaver_urls,omitempty"`
	ScreensaverTimeoutSeconds int      `json:"screensaver_timeout_seconds,omitempty"`
}

// ListPublicOutlets returns all active outlets for a tenant (public, no auth).
// Used by the kiosk PIN login page to populate the outlet switcher.
// Query params:
//
//	?hq=true   — return only the HQ outlet
//	?id=<uuid> — return a single outlet by ID
func (h *PublicOutletHandler) ListPublicOutlets(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.Outlet.Query().
		Where(entoutlet.TenantID(tid), entoutlet.StatusNEQ("archived")).
		WithSettings()

	if r.URL.Query().Get("hq") == "true" {
		q = q.Where(entoutlet.IsHq(true))
	}
	if idParam := r.URL.Query().Get("id"); idParam != "" {
		if oid, err := uuid.Parse(idParam); err == nil {
			q = q.Where(entoutlet.ID(oid))
		}
	}

	outlets, err := q.All(r.Context())
	if err != nil {
		h.log.Error("list public outlets failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]outletPublicItem, 0, len(outlets))
	for i := range outlets {
		out = append(out, toOutletPublicItem(outlets[i]))
	}
	jsonOK(w, map[string]any{"data": out})
}

// GetCurrentOutlet returns the best-match outlet for the kiosk.
// Prefers ?outlet_id= query param (device-stored preference), then falls back to HQ.
func (h *PublicOutletHandler) GetCurrentOutlet(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	// Resolve the screensaver idle-timeout (tenant override → platform default).
	timeout := resolveScreensaverTimeoutSeconds(r.Context(), h.client, tid)

	if oid := r.URL.Query().Get("outlet_id"); oid != "" {
		if outletUUID, err := uuid.Parse(oid); err == nil {
			o, err := h.client.Outlet.Query().
				Where(entoutlet.ID(outletUUID), entoutlet.TenantID(tid), entoutlet.StatusNEQ("archived")).
				WithSettings().
				Only(r.Context())
			if err == nil {
				jsonOK(w, map[string]any{"data": withScreensaverTimeout(toOutletPublicItem(o), timeout)})
				return
			}
		}
	}

	// Default: HQ outlet → first active outlet
	o, err := h.client.Outlet.Query().
		Where(entoutlet.TenantID(tid), entoutlet.IsHq(true), entoutlet.StatusNEQ("archived")).
		WithSettings().
		First(r.Context())
	if err != nil {
		o, err = h.client.Outlet.Query().
			Where(entoutlet.TenantID(tid), entoutlet.StatusNEQ("archived")).
			WithSettings().
			First(r.Context())
		if err != nil {
			jsonError(w, "no outlets found", http.StatusNotFound)
			return
		}
	}

	jsonOK(w, map[string]any{"data": withScreensaverTimeout(toOutletPublicItem(o), timeout)})
}

// withScreensaverTimeout ensures the screensaver idle-timeout is present on the
// outlet's public settings, creating the settings object when the outlet has no
// OutletSetting row so the value is still returned to the kiosk.
func withScreensaverTimeout(item outletPublicItem, timeoutSeconds int) outletPublicItem {
	if timeoutSeconds <= 0 {
		return item
	}
	if item.Settings == nil {
		item.Settings = &outletSettingsPublic{}
	}
	item.Settings.ScreensaverTimeoutSeconds = timeoutSeconds
	return item
}

func toOutletPublicItem(o *ent.Outlet) outletPublicItem {
	useCase := ""
	if o.UseCase != nil {
		useCase = *o.UseCase
	}
	item := outletPublicItem{
		ID:      o.ID.String(),
		Code:    o.Code,
		Name:    o.Name,
		UseCase: useCase,
		IsHQ:    o.IsHq,
		Status:  o.Status,
	}
	if s := o.Edges.Settings; s != nil {
		settings := &outletSettingsPublic{}
		if s.PinLoginMessage != nil {
			settings.PinLoginMessage = *s.PinLoginMessage
		}
		if s.ScreensaverURL != nil {
			settings.ScreensaverURL = *s.ScreensaverURL
		}
		settings.ScreensaverURLs = metaStringSlice(s.Metadata, "screensaver_urls")
		if settings.PinLoginMessage != "" || settings.ScreensaverURL != "" || len(settings.ScreensaverURLs) > 0 {
			item.Settings = settings
		}
	}
	return item
}
