package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

type updateJobInput struct {
	DueAt             *string                 `json:"due_at"`
	Brief             *string                 `json:"brief"`
	DesignFromScratch *bool                   `json:"design_from_scratch"`
	Attachments       *[]orders.JobAttachment `json:"attachments"`
	Stage             *string                 `json:"stage"`
	ProofStatus       *string                 `json:"proof_status"`
	Collected         *bool                   `json:"collected"`
}

// UpdateJob handles PATCH /{tenantID}/pos/orders/{orderID}/job.
//
// Moves a services job order through its production stages, records the customer's proof
// decision, edits the brief/due date/attachments, and marks a finished job collected. Payment is
// never touched here: deposits and the balance go through the normal payment flow.
//
// @Summary Update a services job order
// @Tags orders
// @Accept json
// @Produce json
// @Param orderID path string true "Order ID"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Router /{tenantID}/pos/orders/{orderID}/job [patch]
func (h *POSOrderHandler) UpdateJob(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}
	var input updateJobInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	upd := orders.JobUpdate{
		Brief:             input.Brief,
		DesignFromScratch: input.DesignFromScratch,
		Attachments:       input.Attachments,
		Stage:             input.Stage,
		ProofStatus:       input.ProofStatus,
		Collected:         input.Collected,
	}
	if input.DueAt != nil && strings.TrimSpace(*input.DueAt) != "" {
		t, perr := time.Parse(time.RFC3339, strings.TrimSpace(*input.DueAt))
		if perr != nil {
			jsonError(w, "due_at must be an RFC 3339 time", http.StatusBadRequest)
			return
		}
		upd.DueAt = &t
	}
	var actorID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
		actorID, _ = uuid.Parse(claims.Subject)
	}
	job, err := h.orderSvc.UpdateJob(r.Context(), tid, orderID, actorID, upd)
	if err != nil {
		if errors.Is(err, orders.ErrInvalidJob) {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("update job failed", zap.String("order_id", orderID.String()), zap.Error(err))
		jsonError(w, "failed to update job", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"order_id": orderID, "job": job})
}

// Job attachment storage: reference media a customer brings to reception (a logo, a photo of a
// dashboard warning light, a sample design, a PDF brief). Small by design; anything bigger is
// shared as a link (Google Drive, YouTube) on the job instead.
const (
	jobAttachmentSubfolder = "job-attachments"
	maxJobAttachmentBytes  = 512 << 10
)

var jobAttachmentExtByMIME = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

// SetMediaRoot injects the local media volume root (MEDIA_ROOT) for job attachment uploads.
func (h *POSOrderHandler) SetMediaRoot(root string) {
	if root == "" {
		root = "./media"
	}
	h.mediaRoot = root
}

// UploadJobAttachment handles POST /{tenantID}/pos/orders/job-attachments (multipart field
// "file"). Stores one file of at most 512 KB and returns {url, label}; the terminal adds it to
// the job's attachments (at most orders.MaxJobFileAttachments files per job).
//
// @Summary Upload a job attachment (512 KB image or PDF)
// @Tags orders
// @Accept multipart/form-data
// @Produce json
// @Param file formData file true "Image (PNG, JPEG, WebP) or PDF, max 512 KB"
// @Success 201 {object} map[string]string
// @Failure 413 {object} map[string]string
// @Failure 415 {object} map[string]string
// @Router /{tenantID}/pos/orders/job-attachments [post]
func (h *POSOrderHandler) UploadJobAttachment(w http.ResponseWriter, r *http.Request) {
	rel, _, ok := storeTenantMedia(w, r, h.log, h.mediaRoot, mediaUploadSpec{
		Subfolder: jobAttachmentSubfolder, MaxBytes: maxJobAttachmentBytes, SizeLabel: "512 KB",
		ExtByMIME: jobAttachmentExtByMIME, TypesLabel: "PNG, JPEG, WebP or PDF",
	})
	if !ok {
		return
	}
	label := ""
	if fh := r.MultipartForm; fh != nil {
		if files := fh.File["file"]; len(files) > 0 {
			label = strings.TrimSpace(files[0].Filename)
			if len(label) > 120 {
				label = label[:120]
			}
		}
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"url": rel, "label": label})
}

// JobSummary handles GET /{tenantID}/pos/jobs/summary: the services jobs dashboard counts for the
// active outlet (in production, ready for collection, due today, overdue, awaiting proof) and the
// money on unfinished jobs (balance still to collect, deposits held).
//
// @Summary Services jobs dashboard summary for the active outlet
// @Tags orders
// @Produce json
// @Success 200 {object} orders.JobSummary
// @Router /{tenantID}/pos/jobs/summary [get]
func (h *POSOrderHandler) JobSummary(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	oc := outletmw.OutletFromContext(r.Context())
	if oc == nil {
		jsonError(w, "outlet context required", http.StatusBadRequest)
		return
	}
	summary, err := h.orderSvc.JobSummary(r.Context(), tid, oc.ID, tenantLocation(r.Context(), h.client, tid), time.Now())
	if err != nil {
		h.log.Error("job summary failed", zap.Error(err))
		jsonError(w, "failed to load job summary", http.StatusInternalServerError)
		return
	}
	jsonOK(w, summary)
}
