package handlers

import (
	"net/http"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// pos-api's media types on the shared object storage (httpware/objectstore). Keys are
// pos/<tenant_id>/<type>/<group>/<uuid><ext>.
//
//   - screensavers: public display images and videos of the PIN screen (8 MB).
//   - evidence: private photos of room damage and found items, only reachable through short-lived
//     signed links (8 MB).
//   - job-attachments: public reference files of a services job (512 KB), unguessable names.
const (
	mediaTypeScreensavers   = "screensavers"
	mediaTypeEvidence       = "evidence"
	mediaTypeJobAttachments = "job-attachments"
)

// RegisterMediaTypes adds pos-api's media types to the shared registry. Call once at startup.
func RegisterMediaTypes() {
	objectstore.Register(objectstore.MediaType{
		Name: mediaTypeScreensavers, Public: true, MaxBytes: 8 << 20, Image: true,
		Allowed: []string{"image/png", "image/jpeg", "image/webp", "video/mp4"},
	})
	objectstore.Register(objectstore.MediaType{
		Name: mediaTypeEvidence, MaxBytes: 8 << 20, Image: true,
		Allowed: []string{"image/png", "image/jpeg", "image/webp"},
	})
	objectstore.Register(objectstore.MediaType{
		Name: mediaTypeJobAttachments, Public: true, MaxBytes: 512 << 10, Image: true,
		Allowed: []string{"image/png", "image/jpeg", "image/webp", "application/pdf"},
	})
}

// storeTenantMedia reads the multipart "file", checks it against the media type (size and real
// content type, JPEG and PNG re-encoded), stores it under the request's tenant and answers the key.
// On failure it has already written the error response.
func storeTenantMedia(w http.ResponseWriter, r *http.Request, log *zap.Logger, store *objectstore.Store, mediaType, group string) (string, bool) {
	if !store.Enabled() {
		jsonError(w, "media storage not configured", http.StatusServiceUnavailable)
		return "", false
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return "", false
	}
	up, err := objectstore.ReadUpload(w, r, "file", mediaType)
	if err != nil {
		if objectstore.IsUploadError(err) {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return "", false
		}
		jsonError(w, "failed to read upload", http.StatusInternalServerError)
		return "", false
	}
	key, err := store.Key(objectstore.Owner{TenantID: tid.String()}, mediaType, group, uuid.NewString()+up.Ext)
	if err != nil {
		jsonError(w, "failed to name the file", http.StatusInternalServerError)
		return "", false
	}
	if err := store.Put(r.Context(), key, up.Data, up.ContentType); err != nil {
		log.Error("media store", zap.String("type", mediaType), zap.Error(err))
		jsonError(w, "media storage unavailable", http.StatusBadGateway)
		return "", false
	}
	return key, true
}

// mediaLinks turns stored values into links (a private key gets a signed link valid for the
// configured presign time); values that are already links pass through.
func mediaLinks(store *objectstore.Store, refs []string) []string {
	if len(refs) == 0 {
		return refs
	}
	out := make([]string, len(refs))
	for i, ref := range refs {
		out[i] = store.URL(ref)
	}
	return out
}

// mediaKeys turns values a client sends back (links this service handed out) into what is stored.
func mediaKeys(store *objectstore.Store, refs []string) []string {
	if len(refs) == 0 {
		return refs
	}
	out := make([]string, len(refs))
	for i, ref := range refs {
		out[i] = store.Normalize(ref)
	}
	return out
}
