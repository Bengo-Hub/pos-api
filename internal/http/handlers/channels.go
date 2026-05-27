package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entchan "github.com/bengobox/pos-service/internal/ent/channelintegration"
	entjob "github.com/bengobox/pos-service/internal/ent/channelsyncjob"
)

// ChannelHandler handles delivery-channel integration endpoints (Uber Eats, Glovo, etc.).
type ChannelHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewChannelHandler(log *zap.Logger, db *ent.Client) *ChannelHandler {
	return &ChannelHandler{log: log, db: db}
}

type createChannelInput struct {
	ChannelName string         `json:"channel_name"`
	ChannelType string         `json:"channel_type"`
	ConfigJSON  map[string]any `json:"config_json"`
}

type updateChannelInput struct {
	ChannelName *string        `json:"channel_name"`
	ConfigJSON  map[string]any `json:"config_json"`
	Status      *string        `json:"status"`
}

// ListChannels handles GET /{tenantID}/pos/channels
func (h *ChannelHandler) ListChannels(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.ChannelIntegration.Query().Where(entchan.TenantID(tid))
	if ct := r.URL.Query().Get("channel_type"); ct != "" {
		q = q.Where(entchan.ChannelType(ct))
	}
	if st := r.URL.Query().Get("status"); st != "" {
		q = q.Where(entchan.Status(st))
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	channels, err := q.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list channels failed", zap.Error(err))
		jsonError(w, "failed to list channels", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(channels, total, p))
}

// CreateChannel handles POST /{tenantID}/pos/channels
func (h *ChannelHandler) CreateChannel(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var body createChannelInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.ChannelName == "" || body.ChannelType == "" {
		jsonError(w, "channel_name and channel_type are required", http.StatusBadRequest)
		return
	}
	if body.ConfigJSON == nil {
		body.ConfigJSON = map[string]any{}
	}

	ch, err := h.db.ChannelIntegration.Create().
		SetTenantID(tid).
		SetChannelName(body.ChannelName).
		SetChannelType(body.ChannelType).
		SetConfigJSON(body.ConfigJSON).
		Save(r.Context())
	if err != nil {
		h.log.Error("create channel failed", zap.Error(err))
		jsonError(w, "failed to create channel", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, ch)
}

// UpdateChannel handles PUT /{tenantID}/pos/channels/{channelID}
func (h *ChannelHandler) UpdateChannel(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	cid, err := uuid.Parse(chi.URLParam(r, "channelID"))
	if err != nil {
		jsonError(w, "invalid channelID", http.StatusBadRequest)
		return
	}

	ch, err := h.db.ChannelIntegration.Get(r.Context(), cid)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "channel not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if ch.TenantID != tid {
		jsonError(w, "channel not found", http.StatusNotFound)
		return
	}

	var body updateChannelInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	u := h.db.ChannelIntegration.UpdateOneID(cid)
	if body.ChannelName != nil {
		u = u.SetChannelName(*body.ChannelName)
	}
	if body.ConfigJSON != nil {
		u = u.SetConfigJSON(body.ConfigJSON)
	}
	if body.Status != nil {
		u = u.SetStatus(*body.Status)
	}

	updated, err := u.Save(r.Context())
	if err != nil {
		h.log.Error("update channel failed", zap.Error(err))
		jsonError(w, "failed to update channel", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// DeleteChannel handles DELETE /{tenantID}/pos/channels/{channelID}
func (h *ChannelHandler) DeleteChannel(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	cid, err := uuid.Parse(chi.URLParam(r, "channelID"))
	if err != nil {
		jsonError(w, "invalid channelID", http.StatusBadRequest)
		return
	}

	ch, err := h.db.ChannelIntegration.Get(r.Context(), cid)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "channel not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if ch.TenantID != tid {
		jsonError(w, "channel not found", http.StatusNotFound)
		return
	}

	if err := h.db.ChannelIntegration.DeleteOneID(cid).Exec(r.Context()); err != nil {
		h.log.Error("delete channel failed", zap.Error(err))
		jsonError(w, "failed to delete channel", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListSyncJobs handles GET /{tenantID}/pos/channels/{channelID}/sync-jobs
func (h *ChannelHandler) ListSyncJobs(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	cid, err := uuid.Parse(chi.URLParam(r, "channelID"))
	if err != nil {
		jsonError(w, "invalid channelID", http.StatusBadRequest)
		return
	}

	ch, err := h.db.ChannelIntegration.Get(r.Context(), cid)
	if err != nil || ch.TenantID != tid {
		jsonError(w, "channel not found", http.StatusNotFound)
		return
	}

	p := pagination.Parse(r)
	baseQ := h.db.ChannelSyncJob.Query().Where(entjob.IntegrationID(cid))
	total, _ := baseQ.Clone().Count(r.Context())
	jobs, err := baseQ.Order(ent.Desc(entjob.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list sync jobs failed", zap.Error(err))
		jsonError(w, "failed to list sync jobs", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(jobs, total, p))
}

// TriggerSyncJob handles POST /{tenantID}/pos/channels/{channelID}/sync-jobs
func (h *ChannelHandler) TriggerSyncJob(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	cid, err := uuid.Parse(chi.URLParam(r, "channelID"))
	if err != nil {
		jsonError(w, "invalid channelID", http.StatusBadRequest)
		return
	}

	ch, err := h.db.ChannelIntegration.Get(r.Context(), cid)
	if err != nil || ch.TenantID != tid {
		jsonError(w, "channel not found", http.StatusNotFound)
		return
	}

	var body struct {
		JobType string `json:"job_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.JobType == "" {
		body.JobType = "catalog_sync"
	}

	job, err := h.db.ChannelSyncJob.Create().
		SetIntegrationID(cid).
		SetJobType(body.JobType).
		Save(r.Context())
	if err != nil {
		h.log.Error("trigger sync job failed", zap.Error(err))
		jsonError(w, "failed to trigger sync job", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, job)
}
