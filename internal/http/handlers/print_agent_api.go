package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"nhooyr.io/websocket"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/printing"
)

// PrintAgentAPIHandler is the outbound-poll API the Local Print Agent talks to. The agent sits on
// the shop LAN behind NAT, so it must poll OUT to the cloud pos-api; auth is its pairing key
// (X-Agent-Key header), NOT a user JWT.
type PrintAgentAPIHandler struct {
	log   *zap.Logger
	queue *printing.Queue
	hub   *printing.Hub
}

func NewPrintAgentAPIHandler(log *zap.Logger, queue *printing.Queue) *PrintAgentAPIHandler {
	return &PrintAgentAPIHandler{log: log, queue: queue}
}

// SetHub wires the real-time wake-up hub used by the agent WebSocket endpoint. Optional — without
// it the /ws route reports 503 and agents stay on polling.
func (h *PrintAgentAPIHandler) SetHub(hub *printing.Hub) { h.hub = hub }

// maxAgentWait caps the long-poll so LB/ingress idle timeouts never kill the request mid-flight.
//
// 2026-07-19 live incident: pos-api logs showed "agent job claim failed: context canceled" (500s)
// recurring every 15-90s across every replica, ALWAYS at a ~13-15s request duration — never at the
// full 25s wait. Root cause: the router's global `middleware.Timeout(30*time.Second)` (router.go)
// only left ~5s of margin over the old 25s wait, and something in the path (proxy/GC/scheduling
// jitter under the shared node) was eating into that margin consistently enough to cancel the
// request before it could return its own 204. Every cancellation makes the print-agent's poll loop
// back off exponentially (1s→2s→4s...→30s, spooler.go), during which it claims NOTHING — so a
// freshly enqueued print job can sit unclaimed well past a few seconds, which is exactly what
// produced "printer did not confirm printing" in the till (the till's own confirmation window is
// only ~7s). Shortened to well under half the global timeout for real headroom; the agent
// re-polls immediately on a 204 (spooler.go run()), so shorter waits just mean more frequent
// round trips, not slower job pickup.
const maxAgentWait = 10 * time.Second

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

// StreamAgent handles GET /pos/printing/agent/ws — the real-time wake-up socket. The agent connects
// once and holds it open; the hub pushes a "job_available" nudge the instant a job is enqueued for
// the agent's outlet, so the agent claims it (via the same NextJob/ack HTTP flow) within
// milliseconds instead of on its next poll tick. Auth is the pairing key (X-Agent-Key), same as the
// poll/ack endpoints. This is purely an optimization: the socket carries no job data and the agent
// keeps a slow safety-net poll, so a dropped/absent socket only reverts to the (correct) polling
// behavior — it never loses a job.
func (h *PrintAgentAPIHandler) StreamAgent(w http.ResponseWriter, r *http.Request) {
	if h.hub == nil {
		jsonError(w, "real-time wake-up not available", http.StatusServiceUnavailable)
		return
	}
	agent := h.authAgent(w, r)
	if agent == nil {
		return
	}
	conn, wsErr := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"}, // the agent is a native app, not a browser — no Origin to police
	})
	if wsErr != nil {
		h.log.Warn("print-agent ws: upgrade failed", zap.Error(wsErr))
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	h.log.Info("print-agent ws: connected",
		zap.Stringer("tenant_id", agent.TenantID),
		zap.Stringer("outlet_id", agent.OutletID))
	h.hub.ServeWS(r.Context(), conn, agent.TenantID, agent.OutletID)
	h.log.Debug("print-agent ws: disconnected",
		zap.Stringer("tenant_id", agent.TenantID),
		zap.Stringer("outlet_id", agent.OutletID))
}
