package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/printing"
)

// PrintAgentAPIHandler is the outbound-poll API the Local Print Agent talks to. The agent sits on
// the shop LAN behind NAT, so it must poll OUT to the cloud pos-api; auth is its pairing key
// (X-Agent-Key header), NOT a user JWT.
type PrintAgentAPIHandler struct {
	log   *zap.Logger
	queue *printing.Queue
}

func NewPrintAgentAPIHandler(log *zap.Logger, queue *printing.Queue) *PrintAgentAPIHandler {
	return &PrintAgentAPIHandler{log: log, queue: queue}
}

// maxAgentWait caps the long-poll so LB/ingress idle timeouts never kill the request mid-flight.
const maxAgentWait = 25 * time.Second

func (h *PrintAgentAPIHandler) authAgent(w http.ResponseWriter, r *http.Request) *ent.PrintAgent {
	agent, err := h.queue.AuthAgent(r.Context(), r.Header.Get("X-Agent-Key"))
	if err != nil {
		jsonError(w, "invalid agent key", http.StatusUnauthorized)
		return nil
	}
	return agent
}

// NextJob handles GET /pos/printing/agent/jobs?wait=25&version=1.2.0 — long-poll claim of the next
// queued job for the agent's outlet. 200 with the job, or 204 when none became available.
func (h *PrintAgentAPIHandler) NextJob(w http.ResponseWriter, r *http.Request) {
	agent := h.authAgent(w, r)
	if agent == nil {
		return
	}
	wait := 20 * time.Second
	if v := r.URL.Query().Get("wait"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			wait = time.Duration(secs) * time.Second
		}
	}
	if wait > maxAgentWait {
		wait = maxAgentWait
	}

	job, err := h.queue.ClaimNext(r.Context(), agent, wait, r.URL.Query().Get("version"))
	if err != nil {
		h.log.Warn("agent job claim failed", zap.Error(err))
		jsonError(w, "claim failed", http.StatusInternalServerError)
		return
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	jsonOK(w, map[string]any{
		"id":           job.ID,
		"job_type":     job.JobType,
		"printer_type": job.PrinterType,
		"printer_ip":   job.PrinterIP,
		"printer_port": job.PrinterPort,
		"printer_name": job.PrinterName,
		"paper":        job.Paper,
		"payload_hex":  job.PayloadHex,
	})
}

type ackJobInput struct {
	Printed bool   `json:"printed"`
	Error   string `json:"error"`
}

// AckJob handles POST /pos/printing/agent/jobs/{jobID}/ack — the agent reports the print outcome.
// Failures requeue the job until its attempt cap; the lease sweeper covers agents that die mid-job.
func (h *PrintAgentAPIHandler) AckJob(w http.ResponseWriter, r *http.Request) {
	agent := h.authAgent(w, r)
	if agent == nil {
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "jobID"))
	if err != nil {
		jsonError(w, "invalid job id", http.StatusBadRequest)
		return
	}
	var in ackJobInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	job, err := h.queue.Ack(r.Context(), agent, jobID, in.Printed, in.Error)
	if err != nil {
		jsonError(w, "ack failed: job not held by this agent", http.StatusConflict)
		return
	}
	jsonOK(w, map[string]any{"id": job.ID, "status": job.Status})
}
