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

	"github.com/bengobox/pos-service/internal/ent"
	entroomdamagereport "github.com/bengobox/pos-service/internal/ent/roomdamagereport"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// Damage-evidence photo storage reuses pos-api's existing local media volume (same
// {MEDIA_ROOT}/{tenant-slug}/{subfolder}/ convention as the screensaver uploader in
// media.go) rather than inventing a second storage mechanism.
const (
	damageEvidenceSubfolder = "damage-evidence"
	maxDamageEvidenceBytes  = 8 << 20 // 8MB per photo, same cap as screensaver uploads
)

var damageEvidenceExtByMIME = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
}

// SetMediaRoot injects the local media volume root (MEDIA_ROOT) so damage-evidence photo
// uploads can be stored/served the same way the platform's other admin-uploaded media is.
func (h *HotelHandler) SetMediaRoot(root string) {
	if root == "" {
		root = "./media"
	}
	h.mediaRoot = root
}

type damageReportDTO struct {
	ID           string     `json:"id"`
	RoomID       string     `json:"room_id"`
	RoomGuestID  string     `json:"room_guest_id,omitempty"`
	Description  string     `json:"description"`
	Amount       float64    `json:"amount"`
	Currency     string     `json:"currency"`
	EvidenceURLs []string   `json:"evidence_urls"`
	Status       string     `json:"status"`
	ReportedBy   string     `json:"reported_by"`
	ReviewedBy   string     `json:"reviewed_by,omitempty"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
	ReviewNotes  string     `json:"review_notes,omitempty"`
	FolioItemID  string     `json:"folio_item_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func toDamageReportDTO(r *ent.RoomDamageReport) damageReportDTO {
	dto := damageReportDTO{
		ID: r.ID.String(), RoomID: r.RoomID.String(), Description: r.Description,
		Amount: r.Amount, Currency: r.Currency, EvidenceURLs: r.EvidenceUrls,
		Status: string(r.Status), ReportedBy: r.ReportedBy.String(),
		ReviewNotes: r.ReviewNotes, ReviewedAt: r.ReviewedAt, CreatedAt: r.CreatedAt,
	}
	if r.RoomGuestID != nil {
		dto.RoomGuestID = r.RoomGuestID.String()
	}
	if r.ReviewedBy != nil {
		dto.ReviewedBy = r.ReviewedBy.String()
	}
	if r.FolioItemID != nil {
		dto.FolioItemID = r.FolioItemID.String()
	}
	return dto
}

type createDamageReportInput struct {
	Description  string   `json:"description"`
	Amount       float64  `json:"amount"`
	Currency     string   `json:"currency"`
	EvidenceURLs []string `json:"evidence_urls"`
	ReportedBy   string   `json:"reported_by"`
}

// CreateDamageReport handles POST /{tenantID}/hotel/rooms/{id}/damage-reports — logs a
// suspected guest-caused damage/fine for manager review. Does NOT post a folio charge itself
// (see ApproveDamageReport) — this is the workflow's pending state, evidence-gathering step.
func (h *HotelHandler) CreateDamageReport(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid room id", http.StatusBadRequest)
		return
	}

	var input createDamageReportInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Description == "" {
		jsonError(w, "description is required", http.StatusBadRequest)
		return
	}
	if input.Amount <= 0 {
		jsonError(w, "amount must be positive", http.StatusBadRequest)
		return
	}
	currency := input.Currency
	if currency == "" {
		if room, rerr := h.client.Room.Get(r.Context(), roomID); rerr == nil && room.Currency != "" {
			currency = room.Currency
		} else {
			currency = "KES"
		}
	}
	reportedBy, _ := uuid.Parse(input.ReportedBy)

	create := h.client.RoomDamageReport.Create().
		SetTenantID(tid).
		SetRoomID(roomID).
		SetDescription(input.Description).
		SetAmount(input.Amount).
		SetCurrency(currency).
		SetReportedBy(reportedBy)
	if len(input.EvidenceURLs) > 0 {
		create = create.SetEvidenceUrls(input.EvidenceURLs)
	}

	// Attach the currently active guest, if any — a damage report can also be logged after
	// checkout (e.g. found during housekeeping's clean), in which case it stays unlinked and
	// approval cannot auto-post a folio charge (there's no active folio to charge it against).
	if guest, gerr := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Only(r.Context()); gerr == nil {
		create = create.SetRoomGuestID(guest.ID)
	}

	report, err := create.Save(r.Context())
	if err != nil {
		h.log.Error("create damage report failed", zap.Error(err))
		jsonError(w, "failed to create damage report", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishHotelDamageReported(r.Context(), tid, map[string]any{
			"damage_report_id": report.ID, "room_id": roomID, "amount": report.Amount, "currency": report.Currency,
		})
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, toDamageReportDTO(report))
}

// ListDamageReports handles GET /{tenantID}/hotel/damage-reports?status=&room_id=
func (h *HotelHandler) ListDamageReports(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.client.RoomDamageReport.Query().Where(entroomdamagereport.TenantID(tid))
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entroomdamagereport.StatusEQ(entroomdamagereport.Status(status)))
	}
	if roomIDStr := r.URL.Query().Get("room_id"); roomIDStr != "" {
		if rid, perr := uuid.Parse(roomIDStr); perr == nil {
			q = q.Where(entroomdamagereport.RoomID(rid))
		}
	}
	reports, err := q.Order(ent.Desc(entroomdamagereport.FieldCreatedAt)).All(r.Context())
	if err != nil {
		h.log.Error("list damage reports failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]damageReportDTO, 0, len(reports))
	for _, rep := range reports {
		out = append(out, toDamageReportDTO(rep))
	}
	jsonOK(w, map[string]any{"data": out, "total": len(out)})
}

// GetDamageReport handles GET /{tenantID}/hotel/damage-reports/{id}
func (h *HotelHandler) GetDamageReport(w http.ResponseWriter, r *http.Request) {
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
	report, err := h.client.RoomDamageReport.Query().
		Where(entroomdamagereport.ID(id), entroomdamagereport.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "damage report not found", http.StatusNotFound)
		return
	}
	jsonOK(w, toDamageReportDTO(report))
}

// ApproveDamageReport handles POST /{tenantID}/hotel/damage-reports/{id}/approve — manager-only
// (pos.hotel.manage). When the reported stay is still active, posts the amount to the guest's
// folio as charge_type=damage via the same creation path PostFolioCharge uses, and links the
// resulting folio item back onto the report. When the stay has already checked out (or the
// report was never linked to one), the report is still marked approved but folio_posted is
// false — there's no active folio left to charge; the front desk must settle it another way
// (e.g. an off-platform invoice, or contacting the guest directly).
func (h *HotelHandler) ApproveDamageReport(w http.ResponseWriter, r *http.Request) {
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
		ReviewNotes string `json:"review_notes"`
		ReviewedBy  string `json:"reviewed_by"`
	}
	_ = json.NewDecoder(r.Body).Decode(&input)

	report, err := h.client.RoomDamageReport.Query().
		Where(entroomdamagereport.ID(id), entroomdamagereport.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "damage report not found", http.StatusNotFound)
		return
	}
	if report.Status != entroomdamagereport.StatusPending {
		jsonError(w, "damage report has already been reviewed", http.StatusConflict)
		return
	}
	reviewedBy, _ := uuid.Parse(input.ReviewedBy)

	folioPosted := false
	upd := report.Update().
		SetStatus(entroomdamagereport.StatusApproved).
		SetReviewedAt(time.Now())
	if reviewedBy != uuid.Nil {
		upd = upd.SetReviewedBy(reviewedBy)
	}
	if input.ReviewNotes != "" {
		upd = upd.SetReviewNotes(input.ReviewNotes)
	}

	if report.RoomGuestID != nil {
		if guest, gerr := h.client.RoomGuest.Get(r.Context(), *report.RoomGuestID); gerr == nil && guest.Status == entroomguest.StatusActive {
			item, ferr := h.client.RoomFolioItem.Create().
				SetTenantID(tid).
				SetRoomID(report.RoomID).
				SetRoomGuestID(guest.ID).
				SetDescription("Damage: " + report.Description).
				SetAmount(report.Amount).
				SetCurrency(report.Currency).
				SetChargeType(entroomfolioitem.ChargeTypeDamage).
				SetCreatedBy(reviewedBy).
				Save(r.Context())
			if ferr != nil {
				h.log.Error("approve damage report: post folio charge failed", zap.Error(ferr))
				jsonError(w, "failed to post damage charge to folio", http.StatusInternalServerError)
				return
			}
			upd = upd.SetFolioItemID(item.ID)
			folioPosted = true
		}
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("approve damage report failed", zap.Error(err))
		jsonError(w, "failed to approve damage report", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishHotelDamageReviewed(r.Context(), tid, map[string]any{
			"damage_report_id": updated.ID, "status": "approved", "folio_posted": folioPosted,
		})
	}

	jsonOK(w, map[string]any{"report": toDamageReportDTO(updated), "folio_posted": folioPosted})
}

// RejectDamageReport handles POST /{tenantID}/hotel/damage-reports/{id}/reject — manager-only.
func (h *HotelHandler) RejectDamageReport(w http.ResponseWriter, r *http.Request) {
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
		ReviewNotes string `json:"review_notes"`
		ReviewedBy  string `json:"reviewed_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.ReviewNotes == "" {
		jsonError(w, "review_notes is required when rejecting a damage report", http.StatusBadRequest)
		return
	}

	report, err := h.client.RoomDamageReport.Query().
		Where(entroomdamagereport.ID(id), entroomdamagereport.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "damage report not found", http.StatusNotFound)
		return
	}
	if report.Status != entroomdamagereport.StatusPending {
		jsonError(w, "damage report has already been reviewed", http.StatusConflict)
		return
	}
	reviewedBy, _ := uuid.Parse(input.ReviewedBy)

	upd := report.Update().
		SetStatus(entroomdamagereport.StatusRejected).
		SetReviewedAt(time.Now()).
		SetReviewNotes(input.ReviewNotes)
	if reviewedBy != uuid.Nil {
		upd = upd.SetReviewedBy(reviewedBy)
	}
	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("reject damage report failed", zap.Error(err))
		jsonError(w, "failed to reject damage report", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishHotelDamageReviewed(r.Context(), tid, map[string]any{
			"damage_report_id": updated.ID, "status": "rejected",
		})
	}

	jsonOK(w, toDamageReportDTO(updated))
}

// UploadDamageEvidence handles POST /{tenantID}/hotel/damage-evidence/upload (multipart field
// "file") — stores one photo on the local media volume and returns its relative /media/... URL,
// mirroring ScreensaverMediaHandler.Upload's storage convention exactly (same MEDIA_ROOT, same
// content-type sniffing). The caller collects one or more returned URLs client-side and passes
// them as evidence_urls on CreateDamageReport — this endpoint does not itself touch the report.
func (h *HotelHandler) UploadDamageEvidence(w http.ResponseWriter, r *http.Request) {
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
	dir := filepath.Join(h.mediaRoot, slug, damageEvidenceSubfolder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.log.Error("damage evidence mkdir", zap.Error(err))
		jsonError(w, "media storage unavailable", http.StatusInternalServerError)
		return
	}
	name := uuid.NewString() + ext
	dst := filepath.Join(dir, name)
	out, err := os.Create(dst)
	if err != nil {
		h.log.Error("damage evidence create", zap.Error(err))
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

	rel := path.Join(mediaURLPrefix, slug, damageEvidenceSubfolder, name)
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"url": rel})
}
