package handlers

import (
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/backup"
)

// BackupHandler serves tenant-scoped backups (this tenant's data only). The artifact is a
// gzipped-JSON export of the tenant's rows, tracked in the `backups` table. Permission
// gating (pos.config.change / pos.config.manage) is applied by the router.
type BackupHandler struct {
	log           *zap.Logger
	svc           *backup.Service
	retentionDays int
}

// NewBackupHandler builds the handler.
func NewBackupHandler(log *zap.Logger, svc *backup.Service, retentionDays int) *BackupHandler {
	if retentionDays <= 0 {
		retentionDays = backup.DefaultRetentionDays
	}
	return &BackupHandler{log: log.Named("backups"), svc: svc, retentionDays: retentionDays}
}

// RegisterRoutes mounts the backup endpoints on the given (already tenant-scoped) router
// under /backups.
func (h *BackupHandler) RegisterRoutes(r chi.Router) {
	r.Route("/backups", func(br chi.Router) {
		br.Get("/", h.List)
		br.Post("/", h.Create)
		br.Post("/churn", h.Churn)
		br.Get("/{name}/download", h.Download)
		br.Delete("/{name}", h.Delete)
	})
}

func (h *BackupHandler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantUUID(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}
	items, err := h.svc.List(r.Context(), tenantID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list backups"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"backups": items})
}

func (h *BackupHandler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantUUID(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}
	info, err := h.svc.Generate(r.Context(), tenantID)
	if err != nil {
		h.log.Error("generate backup", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate backup"})
		return
	}
	respondJSON(w, http.StatusCreated, info)
}

// Churn manually triggers retention cleanup (deletes files + tracking rows older than the
// configured retention window across the cluster). The daily scheduler runs this too.
func (h *BackupHandler) Churn(w http.ResponseWriter, r *http.Request) {
	removed, err := h.svc.Churn(r.Context(), h.retentionDays)
	if err != nil {
		h.log.Error("churn backups", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to churn backups"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"removed": removed, "retention_days": h.retentionDays})
}

func (h *BackupHandler) Download(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantUUID(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}
	name := chi.URLParam(r, "name")
	rc, err := h.svc.Open(tenantID, name)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "backup not found"})
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (h *BackupHandler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantUUID(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}
	if err := h.svc.Delete(r.Context(), tenantID, chi.URLParam(r, "name")); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
