package handlers

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	entprintagent "github.com/bengobox/pos-service/internal/ent/printagent"
	"github.com/bengobox/pos-service/internal/modules/printing"
)

// PrintJobsHandler exposes the background print queue to the till UI: explicit job enqueue
// (Print Bill / Print Receipt / Test print buttons) and Local Print Agent pairing/status.
type PrintJobsHandler struct {
	log    *zap.Logger
	client *ent.Client
	queue  *printing.Queue
}

func NewPrintJobsHandler(log *zap.Logger, client *ent.Client, queue *printing.Queue) *PrintJobsHandler {
	return &PrintJobsHandler{log: log, client: client, queue: queue}
}

type enqueueJobInput struct {
	JobType       string `json:"job_type"` // bill | receipt | test | drawer
	OrderID       string `json:"order_id"`
	OutletID      string `json:"outlet_id"`   // required for test/drawer (no order to derive it from)
	ProfileID     string `json:"profile_id"`  // explicit target; empty = resolved bill printer
	PaymentMethod string `json:"payment_method"`
	Station       string `json:"station"` // label shown on a test ticket
}

// EnqueueJob handles POST /{tenantID}/pos/printing/jobs — enqueue a background print job for the
// outlet's Local Print Agent. When no agent is online it enqueues NOTHING and reports
// agent_online:false so the caller falls back to its client-side transports (QZ / loopback agent).
func (h *PrintJobsHandler) EnqueueJob(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var in enqueueJobInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Resolve order + outlet.
	var order *ent.POSOrder
	outletID := uuid.Nil
	if in.OrderID != "" {
		oid, perr := uuid.Parse(in.OrderID)
		if perr != nil {
			jsonError(w, "invalid order_id", http.StatusBadRequest)
			return
		}
		order, err = h.client.POSOrder.Query().
			Where(entposorder.ID(oid), entposorder.TenantID(tid)).
			Only(ctx)
		if err != nil {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		outletID = order.OutletID
	} else if in.OutletID != "" {
		outletID, err = uuid.Parse(in.OutletID)
		if err != nil {
			jsonError(w, "invalid outlet_id", http.StatusBadRequest)
			return
		}
	} else {
		jsonError(w, "order_id or outlet_id required", http.StatusBadRequest)
		return
	}

	if !h.queue.AgentOnline(ctx, tid, outletID) {
		jsonOK(w, map[string]any{"agent_online": false, "jobs": []any{}})
		return
	}

	setting, _ := h.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Only(ctx)
	var profiles []printing.PrinterProfile
	if setting != nil {
		profiles = printing.ProfilesFromRaw(setting.PrinterProfiles)
	}

	// Resolve the target printer: explicit profile, else the outlet's bill printer.
	var profile *printing.PrinterProfile
	if in.ProfileID != "" {
		profile = printing.FindProfileByID(profiles, in.ProfileID)
		if profile == nil || !profile.HasRealPrinter() {
			jsonError(w, "printer profile has no real printer configured", http.StatusUnprocessableEntity)
			return
		}
	} else {
		profile = printing.ResolveBillProfile(profiles)
		if profile == nil {
			jsonError(w, "no printer configured for this outlet", http.StatusUnprocessableEntity)
			return
		}
	}

	// Build the ESC/POS payload per job type.
	var payload []byte
	var orderID *uuid.UUID
	switch in.JobType {
	case "bill", "receipt":
		if order == nil {
			jsonError(w, "order_id required for bill/receipt jobs", http.StatusBadRequest)
			return
		}
		lines, _ := h.client.POSOrderLine.Query().
			Where(entposorderline.OrderID(order.ID)).
			All(ctx)
		payload = printing.BuildReceipt(printing.OrderReceiptData(order, lines, setting, "customer", in.PaymentMethod, ""))
		orderID = &order.ID
	case "test":
		label := in.Station
		if label == "" {
			label = profile.Label
		}
		payload = printing.BuildTestTicket(label, profile.Paper(), time.Now())
	case "drawer":
		payload = drawerKickBytes(setting)
	default:
		jsonError(w, "invalid job_type: must be bill, receipt, test or drawer", http.StatusBadRequest)
		return
	}

	job, err := h.queue.Enqueue(ctx, printing.EnqueueInput{
		TenantID: tid,
		OutletID: outletID,
		OrderID:  orderID,
		JobType:  in.JobType,
		Target:   printing.TargetFromProfile(profile),
		Payload:  payload,
	})
	if err != nil {
		h.log.Warn("print job enqueue failed", zap.Error(err))
		jsonError(w, "failed to enqueue print job", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{
		"agent_online": true,
		"jobs":         []map[string]any{{"id": job.ID, "job_type": job.JobType, "status": job.Status}},
	})
}

// drawerKickBytes returns the outlet's configured ESC/POS drawer-kick pulse (hex) or the default.
func drawerKickBytes(setting *ent.OutletSetting) []byte {
	if setting != nil && setting.CashDrawerKickCode != "" {
		if b, err := hex.DecodeString(setting.CashDrawerKickCode); err == nil && len(b) > 0 {
			return b
		}
	}
	return []byte{0x1B, 0x70, 0x00, 0x3C, 0x78} // ESC p 0 60 120
}

type pairAgentInput struct {
	OutletID string `json:"outlet_id"`
	Name     string `json:"name"`
}

// PairAgent handles POST /{tenantID}/pos/printing/agents — creates a Local Print Agent pairing and
// returns the plaintext key ONCE. The operator pastes it into the agent (or the UI relays it to
// the loopback agent's /pair endpoint).
func (h *PrintJobsHandler) PairAgent(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var in pairAgentInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(in.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}
	if in.Name == "" {
		in.Name = "Print agent"
	}

	plaintext, hash, err := printing.GenerateAgentKey()
	if err != nil {
		jsonError(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	agent, err := h.client.PrintAgent.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetName(in.Name).
		SetKeyHash(hash).
		Save(r.Context())
	if err != nil {
		h.log.Warn("pair print agent failed", zap.Error(err))
		jsonError(w, "failed to pair agent", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"id": agent.ID, "name": agent.Name, "key": plaintext})
}

// ListAgents handles GET /{tenantID}/pos/printing/agents?outlet_id= — paired agents + liveness.
func (h *PrintJobsHandler) ListAgents(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.client.PrintAgent.Query().
		Where(entprintagent.TenantID(tid), entprintagent.Revoked(false))
	if v := r.URL.Query().Get("outlet_id"); v != "" {
		oid, perr := uuid.Parse(v)
		if perr != nil {
			jsonError(w, "invalid outlet_id", http.StatusBadRequest)
			return
		}
		q = q.Where(entprintagent.OutletID(oid))
	}
	agents, err := q.All(r.Context())
	if err != nil {
		jsonError(w, "failed to list agents", http.StatusInternalServerError)
		return
	}
	online := false
	out := make([]map[string]any, 0, len(agents))
	cutoff := time.Now().Add(-printing.AgentOnlineWindow)
	for _, a := range agents {
		agentOnline := a.LastSeenAt != nil && a.LastSeenAt.After(cutoff)
		online = online || agentOnline
		out = append(out, map[string]any{
			"id":           a.ID,
			"name":         a.Name,
			"outlet_id":    a.OutletID,
			"online":       agentOnline,
			"last_seen_at": a.LastSeenAt,
			"version":      a.Version,
		})
	}
	jsonOK(w, map[string]any{"agent_online": online, "agents": out})
}

// RevokeAgent handles DELETE /{tenantID}/pos/printing/agents/{agentID}.
func (h *PrintJobsHandler) RevokeAgent(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	agentID, err := uuid.Parse(chi.URLParam(r, "agentID"))
	if err != nil {
		jsonError(w, "invalid agent id", http.StatusBadRequest)
		return
	}
	n, err := h.client.PrintAgent.Update().
		Where(entprintagent.ID(agentID), entprintagent.TenantID(tid)).
		SetRevoked(true).
		Save(r.Context())
	if err != nil || n == 0 {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]any{"revoked": true})
}
