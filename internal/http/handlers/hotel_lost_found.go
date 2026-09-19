package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/pos-service/internal/ent"
	entlostfounditem "github.com/bengobox/pos-service/internal/ent/lostfounditem"
)

// Lost & found photo storage reuses the same local media volume convention as damage-evidence
// uploads (hotel_damage_reports.go) and the screensaver uploader (media.go) — one storage
// mechanism, three subfolders.
const lostFoundPhotoSubfolder = "lost-found"

type lostFoundItemDTO struct {
	ID              string     `json:"id"`
	OutletID        string     `json:"outlet_id"`
	RoomID          string     `json:"room_id,omitempty"`
	RoomGuestID     string     `json:"room_guest_id,omitempty"`
	Description     string     `json:"description"`
	Category        string     `json:"category"`
	LocationFound   string     `json:"location_found,omitempty"`
	StorageLocation string     `json:"storage_location,omitempty"`
	PhotoURLs       []string   `json:"photo_urls"`
	Status          string     `json:"status"`
	FoundBy         string     `json:"found_by"`
	FoundAt         time.Time  `json:"found_at"`
	GuestName       string     `json:"guest_name,omitempty"`
	GuestPhone      string     `json:"guest_phone,omitempty"`
	GuestEmail      string     `json:"guest_email,omitempty"`
	ClaimedByName   string     `json:"claimed_by_name,omitempty"`
	ClaimedNotes    string     `json:"claimed_notes,omitempty"`
	ClaimedAt       *time.Time `json:"claimed_at,omitempty"`
	DisposalReason  string     `json:"disposal_reason,omitempty"`
	DisposedAt      *time.Time `json:"disposed_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

func toLostFoundItemDTO(it *ent.LostFoundItem) lostFoundItemDTO {
	dto := lostFoundItemDTO{
		ID: it.ID.String(), OutletID: it.OutletID.String(), Description: it.Description,
		Category: string(it.Category), LocationFound: it.LocationFound, StorageLocation: it.StorageLocation,
		PhotoURLs: it.PhotoUrls, Status: string(it.Status), FoundBy: it.FoundBy.String(), FoundAt: it.FoundAt,
		GuestName: it.GuestName, GuestPhone: it.GuestPhone, GuestEmail: it.GuestEmail,
		ClaimedByName: it.ClaimedByName, ClaimedNotes: it.ClaimedNotes, ClaimedAt: it.ClaimedAt,
		DisposalReason: it.DisposalReason, DisposedAt: it.DisposedAt, CreatedAt: it.CreatedAt,
	}
	if it.RoomID != nil {
		dto.RoomID = it.RoomID.String()
	}
	if it.RoomGuestID != nil {
		dto.RoomGuestID = it.RoomGuestID.String()
	}
	return dto
}

type createLostFoundItemInput struct {
	OutletID        string   `json:"outlet_id"`
	RoomID          string   `json:"room_id,omitempty"`
	RoomGuestID     string   `json:"room_guest_id,omitempty"`
	Description     string   `json:"description"`
	Category        string   `json:"category"`
	LocationFound   string   `json:"location_found,omitempty"`
	StorageLocation string   `json:"storage_location,omitempty"`
	PhotoURLs       []string `json:"photo_urls,omitempty"`
	GuestName       string   `json:"guest_name,omitempty"`
	GuestPhone      string   `json:"guest_phone,omitempty"`
	GuestEmail      string   `json:"guest_email,omitempty"`
}

// CreateLostFoundItem handles POST /{tenantID}/hotel/lost-found — logs a guest item found on
// the property (a room, or a common area when room_id is omitted).
func (h *HotelHandler) CreateLostFoundItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var input createLostFoundItemInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Description == "" {
		jsonError(w, "description is required", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		// Fall back to the request's resolved outlet context (matches how other hotel
		// create-endpoints resolve outlet_id when the caller's form doesn't supply one).
		if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
			outletID, err = uuid.Parse(oidStr)
		}
		if err != nil {
			jsonError(w, "outlet_id is required", http.StatusBadRequest)
			return
		}
	}
	foundBy, _ := uuid.Parse(r.Header.Get("X-User-ID"))

	create := h.client.LostFoundItem.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDescription(input.Description).
		SetFoundBy(foundBy)
	if input.Category != "" {
		create = create.SetCategory(entlostfounditem.Category(input.Category))
	}
	if input.LocationFound != "" {
		create = create.SetLocationFound(input.LocationFound)
	}
	if input.StorageLocation != "" {
		create = create.SetStorageLocation(input.StorageLocation)
	}
	if len(input.PhotoURLs) > 0 {
		create = create.SetPhotoUrls(input.PhotoURLs)
	}
	if input.GuestName != "" {
		create = create.SetGuestName(input.GuestName)
	}
	if input.GuestPhone != "" {
		create = create.SetGuestPhone(input.GuestPhone)
	}
	if input.GuestEmail != "" {
		create = create.SetGuestEmail(input.GuestEmail)
	}
	if roomID, perr := uuid.Parse(input.RoomID); perr == nil {
		create = create.SetRoomID(roomID)
	}
	if guestID, perr := uuid.Parse(input.RoomGuestID); perr == nil {
		create = create.SetRoomGuestID(guestID)
		// Prefill guest contact details from the stay when not explicitly supplied, so staff
		// don't have to retype what check-in already captured.
		if input.GuestName == "" || input.GuestPhone == "" {
			if guest, gerr := h.client.RoomGuest.Get(r.Context(), guestID); gerr == nil {
				if input.GuestName == "" {
					create = create.SetGuestName(guest.GuestName)
				}
				if input.GuestPhone == "" && guest.Phone != "" {
					create = create.SetGuestPhone(guest.Phone)
				}
				if input.GuestEmail == "" && guest.Email != "" {
					create = create.SetGuestEmail(guest.Email)
				}
			}
		}
	}

	item, err := create.Save(r.Context())
	if err != nil {
		h.log.Error("create lost & found item failed", zap.Error(err))
		jsonError(w, "failed to log item", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishHotelLostFoundLogged(r.Context(), tid, map[string]any{
			"lost_found_item_id": item.ID, "outlet_id": outletID, "description": item.Description,
		})
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, toLostFoundItemDTO(item))
}

// ListLostFoundItems handles GET /{tenantID}/hotel/lost-found?status=&category=&room_id=
func (h *HotelHandler) ListLostFoundItems(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.client.LostFoundItem.Query().Where(entlostfounditem.TenantID(tid))
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			q = q.Where(entlostfounditem.OutletID(oid))
		}
	}
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entlostfounditem.StatusEQ(entlostfounditem.Status(status)))
	}
	if category := r.URL.Query().Get("category"); category != "" {
		q = q.Where(entlostfounditem.CategoryEQ(entlostfounditem.Category(category)))
	}
	if roomIDStr := r.URL.Query().Get("room_id"); roomIDStr != "" {
		if rid, perr := uuid.Parse(roomIDStr); perr == nil {
			q = q.Where(entlostfounditem.RoomID(rid))
		}
	}
	items, err := q.Order(ent.Desc(entlostfounditem.FieldFoundAt)).All(r.Context())
	if err != nil {
		h.log.Error("list lost & found items failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]lostFoundItemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, toLostFoundItemDTO(it))
	}
	jsonOK(w, map[string]any{"data": out, "total": len(out)})
}

// GetLostFoundItem handles GET /{tenantID}/hotel/lost-found/{id}
func (h *HotelHandler) GetLostFoundItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	item, err := h.client.LostFoundItem.Query().
		Where(entlostfounditem.ID(id), entlostfounditem.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "item not found", http.StatusNotFound)
		return
	}
	jsonOK(w, toLostFoundItemDTO(item))
}

// ClaimLostFoundItem handles POST /{tenantID}/hotel/lost-found/{id}/claim
func (h *HotelHandler) ClaimLostFoundItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	var input struct {
		ClaimedByName string `json:"claimed_by_name"`
		ClaimedNotes  string `json:"claimed_notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.ClaimedByName == "" {
		jsonError(w, "claimed_by_name is required", http.StatusBadRequest)
		return
	}

	item, err := h.client.LostFoundItem.Query().
		Where(entlostfounditem.ID(id), entlostfounditem.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "item not found", http.StatusNotFound)
		return
	}
	if item.Status != entlostfounditem.StatusStored {
		jsonError(w, "item is not available to claim", http.StatusConflict)
		return
	}

	upd := item.Update().
		SetStatus(entlostfounditem.StatusClaimed).
		SetClaimedByName(input.ClaimedByName).
		SetClaimedAt(time.Now())
	if input.ClaimedNotes != "" {
		upd = upd.SetClaimedNotes(input.ClaimedNotes)
	}
	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("claim lost & found item failed", zap.Error(err))
		jsonError(w, "failed to record claim", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishHotelLostFoundClaimed(r.Context(), tid, map[string]any{
			"lost_found_item_id": updated.ID, "claimed_by_name": updated.ClaimedByName,
		})
	}
	jsonOK(w, toLostFoundItemDTO(updated))
}

// DisposeLostFoundItem handles POST /{tenantID}/hotel/lost-found/{id}/dispose — status becomes
// "disposed" or "donated" per the body's disposition field (manager-only, see router.go).
func (h *HotelHandler) DisposeLostFoundItem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	var input struct {
		Disposition string `json:"disposition"` // disposed | donated
		Reason      string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Reason == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}
	status := entlostfounditem.StatusDisposed
	if input.Disposition == "donated" {
		status = entlostfounditem.StatusDonated
	}

	item, err := h.client.LostFoundItem.Query().
		Where(entlostfounditem.ID(id), entlostfounditem.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "item not found", http.StatusNotFound)
		return
	}
	if item.Status != entlostfounditem.StatusStored {
		jsonError(w, "item has already been claimed or disposed", http.StatusConflict)
		return
	}

	updated, err := item.Update().
		SetStatus(status).
		SetDisposalReason(input.Reason).
		SetDisposedAt(time.Now()).
		Save(r.Context())
	if err != nil {
		h.log.Error("dispose lost & found item failed", zap.Error(err))
		jsonError(w, "failed to record disposal", http.StatusInternalServerError)
		return
	}
	jsonOK(w, toLostFoundItemDTO(updated))
}

// UploadLostFoundPhoto handles POST /{tenantID}/hotel/lost-found/upload (multipart field
// "file") — same storage convention as UploadDamageEvidence, different subfolder.
func (h *HotelHandler) UploadLostFoundPhoto(w http.ResponseWriter, r *http.Request) {
	if h.mediaRoot == "" {
		jsonError(w, "media storage not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseMultipartForm(maxDamageEvidenceBytes); err != nil {
		jsonError(w, "invalid multipart form (max 8MB)", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if header.Size > maxDamageEvidenceBytes {
		jsonError(w, "file too large (max 8MB)", http.StatusRequestEntityTooLarge)
		return
	}

	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	mime := http.DetectContentType(head[:n])
	ext, ok := damageEvidenceExtByMIME[mime]
	if !ok {
		jsonError(w, "unsupported media type — use PNG, JPEG or WebP", http.StatusUnsupportedMediaType)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		jsonError(w, "failed to read upload", http.StatusInternalServerError)
		return
	}

	slug := tenantSlugFrom(r)
	dir := filepath.Join(h.mediaRoot, slug, lostFoundPhotoSubfolder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.log.Error("lost & found photo mkdir", zap.Error(err))
		jsonError(w, "media storage unavailable", http.StatusInternalServerError)
		return
	}
	name := uuid.NewString() + ext
	dst := filepath.Join(dir, name)
	out, err := os.Create(dst)
	if err != nil {
		h.log.Error("lost & found photo create", zap.Error(err))
		jsonError(w, "media storage unavailable", http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, io.LimitReader(file, maxDamageEvidenceBytes)); err != nil {
		out.Close()
		_ = os.Remove(dst)
		jsonError(w, "failed to store upload", http.StatusInternalServerError)
		return
	}
	out.Close()

	rel := path.Join(mediaURLPrefix, slug, lostFoundPhotoSubfolder, name)
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"url": rel})
}
