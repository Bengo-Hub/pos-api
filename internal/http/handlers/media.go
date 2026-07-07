package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/pos-service/internal/ent"
)

// ScreensaverMediaHandler manages the up-to-3 admin-uploaded POS screensaver images/videos
// (Settings → Display) following the platform media convention: files live on the service's
// media volume under {MEDIA_ROOT}/{tenant-slug}/screensaver/, the DB stores a RELATIVE
// `/media/...` URL (outlet settings metadata `screensaver_urls`), and the client resolves it
// against the API base at read time. Replacing media is explicit: the delete endpoint removes
// BOTH the URL from settings and the file from disk before a new upload is accepted.
type ScreensaverMediaHandler struct {
	log      *zap.Logger
	settings *ServiceSettingsHandler
	root     string
}

const (
	maxScreensavers      = 3
	maxScreensaverBytes  = 8 << 20 // 8 MB per file (router body cap is 10 MB)
	screensaverMetaKey   = "screensaver_urls"
	mediaURLPrefix       = "/media/"
	screensaverSubfolder = "screensaver"
)

var screensaverExtByMIME = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"video/mp4":  ".mp4",
}

func NewScreensaverMediaHandler(log *zap.Logger, settings *ServiceSettingsHandler, root string) *ScreensaverMediaHandler {
	if root == "" {
		root = "./media"
	}
	return &ScreensaverMediaHandler{log: log, settings: settings, root: root}
}

// RegisterRoutes mounts the tenant-scoped management endpoints on the /pos router.
// Permission: same pos.config.change/manage gate as the rest of outlet settings.
func (h *ScreensaverMediaHandler) RegisterRoutes(r chi.Router) {
	r.Get("/settings/screensavers", h.List)
	r.Post("/settings/screensavers", h.Upload)
	r.Delete("/settings/screensavers", h.Delete)
}

// tenantSlugFrom returns the tenant slug for media paths (context is set by TenantV2).
func tenantSlugFrom(r *http.Request) string {
	if slug := httpware.GetTenantSlug(r.Context()); slug != "" {
		return slug
	}
	return httpware.GetTenantID(r.Context())
}

// currentURLs reads the stored relative screensaver URLs from outlet-settings metadata.
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

// List returns the managed screensaver URLs (relative /media/... paths).
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

// Upload handles POST /pos/settings/screensavers (multipart field "file").
// Rejects the upload when 3 screensavers already exist — the admin must delete one first
// (per product decision: replace = delete existing media file + URL, then add the new one).
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

	if err := r.ParseMultipartForm(maxScreensaverBytes); err != nil {
		jsonError(w, "invalid multipart form (max 8MB)", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if header.Size > maxScreensaverBytes {
		jsonError(w, "file too large (max 8MB)", http.StatusRequestEntityTooLarge)
		return
	}

	// Sniff the real content type — never trust the extension/Content-Type header.
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	mime := http.DetectContentType(head[:n])
	ext, ok := screensaverExtByMIME[mime]
	if !ok {
		jsonError(w, "unsupported media type — use PNG, JPEG, WebP or MP4", http.StatusUnsupportedMediaType)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		jsonError(w, "failed to read upload", http.StatusInternalServerError)
		return
	}

	slug := tenantSlugFrom(r)
	dir := filepath.Join(h.root, slug, screensaverSubfolder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.log.Error("screensaver mkdir", zap.Error(err))
		jsonError(w, "media storage unavailable", http.StatusInternalServerError)
		return
	}
	name := uuid.NewString() + ext
	dst := filepath.Join(dir, name)
	out, err := os.Create(dst)
	if err != nil {
		h.log.Error("screensaver create", zap.Error(err))
		jsonError(w, "media storage unavailable", http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, io.LimitReader(file, maxScreensaverBytes)); err != nil {
		out.Close()
		_ = os.Remove(dst)
		jsonError(w, "failed to store upload", http.StatusInternalServerError)
		return
	}
	out.Close()

	rel := path.Join(mediaURLPrefix, slug, screensaverSubfolder, name)
	updated := append(urls, rel)
	if err := h.saveURLs(r, sctx, updated); err != nil {
		_ = os.Remove(dst)
		h.log.Error("screensaver save settings", zap.Error(err))
		jsonError(w, "failed to save settings", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"screensaver_urls": updated, "uploaded": rel})
}

// Delete handles DELETE /pos/settings/screensavers?url=/media/... — removes the URL from
// settings AND the backing file from the media volume (path-traversal guarded).
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

	// Best-effort file removal — only within THIS tenant's screensaver folder.
	slug := tenantSlugFrom(r)
	prefix := path.Join(mediaURLPrefix, slug, screensaverSubfolder) + "/"
	if strings.HasPrefix(target, prefix) {
		name := filepath.Base(path.Clean(target))
		full := filepath.Join(h.root, slug, screensaverSubfolder, name)
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			h.log.Warn("screensaver file remove", zap.String("path", full), zap.Error(err))
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

// ServeMedia mounts the read-only public file server for GET /media/* at the router root.
// Public by design (screensaver images render on the pre-auth PIN screen); the media tree
// contains only admin-uploaded display assets, never documents.
func ServeMedia(root string) http.HandlerFunc {
	fs := http.StripPrefix(mediaURLPrefix, http.FileServer(http.Dir(root)))
	return func(w http.ResponseWriter, r *http.Request) {
		// Never allow path escapes or directory listings.
		clean := path.Clean(r.URL.Path)
		if strings.Contains(clean, "..") || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fs.ServeHTTP(w, r)
	}
}
