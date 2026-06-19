package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/backup/destination"
)

// BackupDestinationHandler exposes platform-default and per-tenant management of
// the pluggable backup destination (OneDrive / Google Drive / S3 / WebDAV / SFTP
// / SMB) used to mirror tenant backups off the local PVC. Secret backend params
// are encrypted at rest (via a SECRET_KEY-derived key), masked in all responses,
// and never logged.
type BackupDestinationHandler struct {
	store    *destination.Store
	uploader *destination.Uploader
	log      *zap.Logger
}

// NewBackupDestinationHandler builds the handler over the ent client + a
// SECRET_KEY-derived Cipher. pos-api has no KeyProvider, so the destination
// config is encrypted with sha256(SECRET_KEY) — stable across restarts/pods.
func NewBackupDestinationHandler(client *ent.Client, log *zap.Logger) *BackupDestinationHandler {
	store := destination.NewStore(client, destination.NewSecretKeyCipher(), log)
	return &BackupDestinationHandler{
		store:    store,
		uploader: destination.NewUploader(store, log),
		log:      log.Named("backup-destination-handler"),
	}
}

// Store exposes the underlying destination Store so the backup service can build
// an Uploader from the same instance during app wiring.
func (h *BackupDestinationHandler) Store() *destination.Store { return h.store }

// Uploader exposes the configured Uploader for wiring into the backup service.
func (h *BackupDestinationHandler) Uploader() *destination.Uploader { return h.uploader }

// destinationResponse is the masked, API-safe view of a stored destination.
type destinationResponse struct {
	Configured bool                      `json:"configured"`
	Type       string                    `json:"type"`
	Enabled    bool                      `json:"enabled"`
	RemotePath string                    `json:"remote_path"`
	Params     []destination.MaskedParam `json:"params"`
}

type upsertDestinationRequest struct {
	Type       string            `json:"type"`
	Enabled    bool              `json:"enabled"`
	RemotePath string            `json:"remote_path"`
	Params     map[string]string `json:"params"`
}

// RegisterPlatformRoutes registers the platform-default destination routes under
// the caller's already platform-owner-gated router.
func (h *BackupDestinationHandler) RegisterPlatformRoutes(r chi.Router) {
	r.Route("/backups/destination", func(d chi.Router) {
		d.Get("/", h.GetPlatform)
		d.Put("/", h.PutPlatform)
		d.Post("/test", h.TestPlatform)
	})
}

// RegisterRoutes registers the per-tenant override destination routes under the
// caller's already tenant-scoped + permission-gated router.
func (h *BackupDestinationHandler) RegisterRoutes(r chi.Router) {
	r.Route("/backups/destination", func(d chi.Router) {
		d.Get("/", h.GetTenant)
		d.Put("/", h.PutTenant)
		d.Post("/test", h.TestTenant)
	})
}

// --- platform (tenant_id NULL) ---

// GetPlatform returns the masked platform-default destination.
func (h *BackupDestinationHandler) GetPlatform(w http.ResponseWriter, r *http.Request) {
	h.get(w, r, nil)
}

// PutPlatform validates and stores the platform-default destination.
func (h *BackupDestinationHandler) PutPlatform(w http.ResponseWriter, r *http.Request) {
	h.put(w, r, nil)
}

// TestPlatform runs a connection test against the posted destination body.
func (h *BackupDestinationHandler) TestPlatform(w http.ResponseWriter, r *http.Request) {
	h.test(w, r)
}

// --- tenant override ---

// GetTenant returns the masked per-tenant destination override.
func (h *BackupDestinationHandler) GetTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantUUID(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}
	h.get(w, r, &tenantID)
}

// PutTenant validates and stores the per-tenant destination override.
func (h *BackupDestinationHandler) PutTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantUUID(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}
	h.put(w, r, &tenantID)
}

// TestTenant runs a connection test against the posted destination body.
func (h *BackupDestinationHandler) TestTenant(w http.ResponseWriter, r *http.Request) {
	if _, err := parseTenantUUID(r); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}
	h.test(w, r)
}

// --- shared impl ---

func (h *BackupDestinationHandler) get(w http.ResponseWriter, r *http.Request, tenantID *uuid.UUID) {
	d, found := h.store.Get(r.Context(), tenantID)
	if !found {
		// No row at this scope — report the inert default (PVC-only).
		d = destination.PVCOnly()
	}
	respondJSON(w, http.StatusOK, destinationResponse{
		Configured: found,
		Type:       string(d.Type),
		Enabled:    d.Enabled,
		RemotePath: d.RemotePath,
		Params:     d.MaskedParams(),
	})
}

func (h *BackupDestinationHandler) put(w http.ResponseWriter, r *http.Request, tenantID *uuid.UUID) {
	var req upsertDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	d := destination.Destination{
		Type:       destination.Type(req.Type),
		Enabled:    req.Enabled,
		RemotePath: req.RemotePath,
		Params:     req.Params,
	}
	// Validate type + required params BEFORE persisting (never echoes secrets).
	if err := d.Validate(); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := h.store.Store(r.Context(), tenantID, d); err != nil {
		// store() already avoids logging credential material; do the same here.
		h.log.Error("failed to store backup destination", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save backup destination"})
		return
	}

	// Re-read the persisted+decrypted value so the response is the masked truth.
	h.get(w, r, tenantID)
}

func (h *BackupDestinationHandler) test(w http.ResponseWriter, r *http.Request) {
	var req upsertDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	d := destination.Destination{
		Type:       destination.Type(req.Type),
		Enabled:    req.Enabled,
		RemotePath: req.RemotePath,
		Params:     req.Params,
	}
	res := h.uploader.TestConnection(r.Context(), d)
	status := http.StatusOK
	if !res.OK {
		status = http.StatusBadRequest
	}
	respondJSON(w, status, res)
}
