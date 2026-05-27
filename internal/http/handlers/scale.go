package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/weighingscalereading"
)

// ScaleHandler handles weighing scale reading endpoints.
type ScaleHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewScaleHandler(log *zap.Logger, db *ent.Client) *ScaleHandler {
	return &ScaleHandler{log: log, db: db}
}

type createScaleReadingInput struct {
	OutletID      string  `json:"outlet_id"`
	SessionID     string  `json:"session_id"`
	DeviceSerial  string  `json:"device_serial"`
	WeightKg      float64 `json:"weight_kg"`
	Unit          string  `json:"unit"`
	CatalogItemID string  `json:"catalog_item_id"`
	Status        string  `json:"status"`
}

// Create handles POST /{tenantID}/pos/scale/readings
func (h *ScaleHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createScaleReadingInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	if input.WeightKg <= 0 {
		jsonError(w, "weight_kg must be positive", http.StatusBadRequest)
		return
	}

	c := h.db.WeighingScaleReading.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetWeightKg(input.WeightKg)

	if input.SessionID != "" {
		sid, err := uuid.Parse(input.SessionID)
		if err != nil {
			jsonError(w, "invalid session_id", http.StatusBadRequest)
			return
		}
		c.SetSessionID(sid)
	}
	if input.DeviceSerial != "" {
		c.SetDeviceSerial(input.DeviceSerial)
	}
	if input.Unit != "" {
		c.SetUnit(input.Unit)
	}
	if input.CatalogItemID != "" {
		cid, err := uuid.Parse(input.CatalogItemID)
		if err != nil {
			jsonError(w, "invalid catalog_item_id", http.StatusBadRequest)
			return
		}
		c.SetCatalogItemID(cid)
	}
	if input.Status != "" {
		c.SetStatus(input.Status)
	}

	reading, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create scale reading failed", zap.Error(err))
		jsonError(w, "failed to save scale reading: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, reading)
}

// List handles GET /{tenantID}/pos/scale/readings
// Optional query param: ?session_id=
func (h *ScaleHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.WeighingScaleReading.Query().
		Where(weighingscalereading.TenantID(tid))

	if sid := r.URL.Query().Get("session_id"); sid != "" {
		sessionID, err := uuid.Parse(sid)
		if err != nil {
			jsonError(w, "invalid session_id", http.StatusBadRequest)
			return
		}
		q = q.Where(weighingscalereading.SessionID(sessionID))
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	readings, err := q.Order(ent.Desc(weighingscalereading.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list scale readings failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(readings, total, p))
}
