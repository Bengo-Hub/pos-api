package handlers

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// mediaUploadSpec describes one kind of file pos-api stores on its local media volume. Every
// upload follows the same convention: {MEDIA_ROOT}/{tenant-slug}/{Subfolder}/{uuid}{ext} on disk,
// a relative /media/... URL in the database, and ServeMedia serving it back.
type mediaUploadSpec struct {
	Subfolder string
	MaxBytes  int64
	// SizeLabel is the human cap shown in errors ("8MB", "512 KB").
	SizeLabel string
	// ExtByMIME maps each accepted sniffed content type to the stored extension.
	ExtByMIME map[string]string
	// TypesLabel lists the accepted types for the unsupported-type error.
	TypesLabel string
}

// storeTenantMedia reads the multipart "file" field, checks its size and sniffed type against
// spec, and writes it to the tenant's subfolder. On success it returns the relative URL and the
// absolute path (so a caller that fails a later step can remove the file). On failure it has
// already written the error response and returns ok=false.
func storeTenantMedia(w http.ResponseWriter, r *http.Request, log *zap.Logger, root string, spec mediaUploadSpec) (rel, dst string, ok bool) {
	if root == "" {
		jsonError(w, "media storage not configured", http.StatusServiceUnavailable)
		return "", "", false
	}
	// Cap the body before parsing so an oversized upload is cut off early (multipart framing
	// adds a little on top of the file itself).
	r.Body = http.MaxBytesReader(w, r.Body, spec.MaxBytes+64<<10)
	if err := r.ParseMultipartForm(spec.MaxBytes); err != nil {
		jsonError(w, fmt.Sprintf("file too large or invalid form (max %s)", spec.SizeLabel), http.StatusRequestEntityTooLarge)
		return "", "", false
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "missing file field", http.StatusBadRequest)
		return "", "", false
	}
	defer file.Close()
	if header.Size > spec.MaxBytes {
		jsonError(w, fmt.Sprintf("file too large (max %s)", spec.SizeLabel), http.StatusRequestEntityTooLarge)
		return "", "", false
	}

	// Sniff the real content type; never trust the extension or the client's Content-Type.
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	ext, allowed := spec.ExtByMIME[http.DetectContentType(head[:n])]
	if !allowed {
		jsonError(w, "unsupported media type, use "+spec.TypesLabel, http.StatusUnsupportedMediaType)
		return "", "", false
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		jsonError(w, "failed to read upload", http.StatusInternalServerError)
		return "", "", false
	}

	slug := tenantSlugFrom(r)
	dir := filepath.Join(root, slug, spec.Subfolder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Error("media mkdir", zap.String("subfolder", spec.Subfolder), zap.Error(err))
		jsonError(w, "media storage unavailable", http.StatusInternalServerError)
		return "", "", false
	}
	name := uuid.NewString() + ext
	dst = filepath.Join(dir, name)
	out, err := os.Create(dst)
	if err != nil {
		log.Error("media create", zap.String("subfolder", spec.Subfolder), zap.Error(err))
		jsonError(w, "media storage unavailable", http.StatusInternalServerError)
		return "", "", false
	}
	if _, err := io.Copy(out, io.LimitReader(file, spec.MaxBytes)); err != nil {
		out.Close()
		_ = os.Remove(dst)
		jsonError(w, "failed to store upload", http.StatusInternalServerError)
		return "", "", false
	}
	out.Close()
	return path.Join(mediaURLPrefix, slug, spec.Subfolder, name), dst, true
}
