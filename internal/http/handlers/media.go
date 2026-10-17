package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
)

// ScreensaverMediaHandler manages the up-to-3 admin-uploaded POS screensaver images/videos
// (Settings → Display). Files live on the shared object storage (public type screensavers) and
// outlet settings metadata `screensaver_urls` keeps their permanent public links (settings are
// returned as stored, to the pre-auth PIN screen too). Replacing media is explicit: the delete
// endpoint removes the link from settings and the file from storage before a new upload is
// accepted, so storage only holds screensavers in use.
type ScreensaverMediaHandler struct {
	log      *zap.Logger
	settings *ServiceSettingsHandler
	store    *objectstore.Store
}

const (
	maxScreensavers    = 3
	screensaverMetaKey = "screensaver_urls"
)

func NewScreensaverMediaHandler(log *zap.Logger, settings *ServiceSettingsHandler, store *objectstore.Store) *ScreensaverMediaHandler {
	return &ScreensaverMediaHandler{log: log, settings: settings, store: store}
}

// RegisterRoutes mounts the tenant-scoped management endpoints on the /pos router.
// Permission: same pos.config.change/manage gate as the rest of outlet settings.
func (h *ScreensaverMediaHandler) RegisterRoutes(r chi.Router) {
	r.Get("/settings/screensavers", h.List)
	r.Post("/settings/screensavers", h.Upload)
	r.Delete("/settings/screensavers", h.Delete)
}

// currentURLs reads the stored screensaver links from outlet-settings metadata.
func (h *ScreensaverMediaHandler) currentURLs(r *http.Request) ([]string, *settingsCtx, error) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid tenant_id")
	}
	outlet, err := h.settings.resolveOutlet(r, tid)
	if err != nil {
		return nil, nil, fmt.Errorf("outlet not found")
	}
	setting, err := h.settings.getOrCreateSetting(r, outlet.ID)
	if err != nil {
		return nil, nil, err
	}
	return metaStringSlice(setting.Metadata, screensaverMetaKey), &settingsCtx{outlet: outlet, setting: setting}, nil
}

type settingsCtx struct {
	outlet  *ent.Outlet
	setting *ent.OutletSetting
}

// List returns the managed screensaver links.
func (h *ScreensaverMediaHandler) List(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	urls, _, err := h.currentURLs(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]any{"screensaver_urls": urls, "max": maxScreensavers})
}

// Upload handles POST /pos/settings/screensavers (multipart field "file": PNG, JPEG, WebP or MP4,
// up to 8 MB). Rejects the upload when 3 screensavers already exist; the admin must delete one
// first (per product decision: replace = delete the existing file and link, then add the new one).
func (h *ScreensaverMediaHandler) Upload(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	urls, sctx, err := h.currentURLs(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	if len(urls) >= maxScreensavers {
		jsonError(w, fmt.Sprintf("maximum of %d screensavers reached — delete one before adding another", maxScreensavers), http.StatusConflict)
		return
	}

	key, ok := storeTenantMedia(w, r, h.log, h.store, mediaTypeScreensavers, "display")
	if !ok {
		return
	}
	link := h.store.URL(key)
	updated := append(urls, link)
	if err := h.saveURLs(r, sctx, updated); err != nil {
		_ = h.store.Delete(r.Context(), key)
		h.log.Error("screensaver save settings", zap.Error(err))
		jsonError(w, "failed to save settings", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"screensaver_urls": updated, "uploaded": link})
}

// Delete handles DELETE /pos/settings/screensavers?url=... and removes the link from settings
// and the file from storage (only this service's own files; an external link is just dropped).
func (h *ScreensaverMediaHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	if target == "" {
		// Also accept {"url": "..."} in the body for clients that can't send DELETE queries.
		var body struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		target = strings.TrimSpace(body.URL)
	}
	if target == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}

	urls, sctx, err := h.currentURLs(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	remaining := make([]string, 0, len(urls))
	found := false
	for _, u := range urls {
		if u == target {
			found = true
			continue
		}
		remaining = append(remaining, u)
	}
	if !found {
		jsonError(w, "screensaver url not found", http.StatusNotFound)
		return
	}
	if err := h.saveURLs(r, sctx, remaining); err != nil {
		h.log.Error("screensaver delete settings", zap.Error(err))
		jsonError(w, "failed to save settings", http.StatusInternalServerError)
		return
	}

	// Only a file under this tenant's own screensaver keys is deleted.
	tid, _ := parseTenantUUID(r)
	if h.store.Owned(target) && strings.HasPrefix(h.store.Normalize(target), "pos/"+tid.String()+"/"+mediaTypeScreensavers+"/") {
		if err := h.store.Delete(r.Context(), target); err != nil {
			h.log.Warn("screensaver file remove", zap.String("url", target), zap.Error(err))
		}
	}
	jsonOK(w, map[string]any{"screensaver_urls": remaining, "deleted": target})
}

// saveURLs merges the list into outlet-settings metadata (preserving other keys).
func (h *ScreensaverMediaHandler) saveURLs(r *http.Request, sctx *settingsCtx, urls []string) error {
	meta := map[string]any{}
	for k, v := range sctx.setting.Metadata {
		meta[k] = v
	}
	meta[screensaverMetaKey] = dedupeStrings(urls)
	_, err := sctx.setting.Update().SetMetadata(meta).Save(r.Context())
	return err
}
