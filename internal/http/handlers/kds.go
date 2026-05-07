package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsstation "github.com/bengobox/pos-service/internal/ent/kdsstation"
	entkdsticket "github.com/bengobox/pos-service/internal/ent/kdsticket"
)

// KDSHandler handles Kitchen Display System endpoints.
type KDSHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewKDSHandler(log *zap.Logger, client *ent.Client) *KDSHandler {
	return &KDSHandler{log: log, client: client}
}

// ListStations handles GET /{tenantID}/pos/kds/stations
func (h *KDSHandler) ListStations(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	stations, err := h.client.KDSStation.Query().
		Where(entkdsstation.TenantID(tid), entkdsstation.IsActive(true)).
		Order(ent.Asc(entkdsstation.FieldSortOrder)).
		All(r.Context())
	if err != nil {
		h.log.Error("list kds stations failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": stations, "total": len(stations)})
}

// GetKitchenQueue handles GET /{tenantID}/pos/kds/kitchen
// Returns pending/in_progress/ready tickets for kitchen stations.
func (h *KDSHandler) GetKitchenQueue(w http.ResponseWriter, r *http.Request) {
	h.getQueue(w, r, "kitchen")
}

// GetBarQueue handles GET /{tenantID}/pos/kds/bar
// Returns pending/in_progress/ready tickets for bar stations.
func (h *KDSHandler) GetBarQueue(w http.ResponseWriter, r *http.Request) {
	h.getQueue(w, r, "bar")
}

func (h *KDSHandler) getQueue(w http.ResponseWriter, r *http.Request, stationType string) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	// Find stations matching the type (by name containing the type keyword)
	stations, err := h.client.KDSStation.Query().
		Where(entkdsstation.TenantID(tid), entkdsstation.IsActive(true)).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	var stationIDs []uuid.UUID
	for _, s := range stations {
		// Match stations by name containing the type keyword (kitchen/bar)
		if containsCaseInsensitive(s.Name, stationType) {
			stationIDs = append(stationIDs, s.ID)
		}
	}

	activeStatuses := []entkdsticket.Status{
		entkdsticket.StatusPending,
		entkdsticket.StatusInProgress,
		entkdsticket.StatusReady,
	}

	q := h.client.KDSTicket.Query().
		Where(
			entkdsticket.TenantID(tid),
			entkdsticket.StatusIn(activeStatuses...),
		).
		WithStation().
		Order(ent.Asc(entkdsticket.FieldPriority), ent.Asc(entkdsticket.FieldReceivedAt))

	if len(stationIDs) > 0 {
		q = q.Where(entkdsticket.StationIDIn(stationIDs...))
	}

	tickets, err := q.All(r.Context())
	if err != nil {
		h.log.Error("get queue failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": tickets, "total": len(tickets)})
}

// ListTickets handles GET /{tenantID}/pos/kds/tickets
// Supports query params: station_id, status
func (h *KDSHandler) ListTickets(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.client.KDSTicket.Query().
		Where(entkdsticket.TenantID(tid)).
		WithStation().
		Order(ent.Asc(entkdsticket.FieldPriority), ent.Asc(entkdsticket.FieldReceivedAt))

	if stationParam := r.URL.Query().Get("station_id"); stationParam != "" {
		if stationID, err := uuid.Parse(stationParam); err == nil {
			q = q.Where(entkdsticket.StationID(stationID))
		}
	}
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entkdsticket.StatusEQ(entkdsticket.Status(status)))
	}

	// Default to active tickets only unless status explicitly requested
	if r.URL.Query().Get("status") == "" {
		q = q.Where(entkdsticket.StatusIn(
			entkdsticket.StatusPending,
			entkdsticket.StatusInProgress,
			entkdsticket.StatusReady,
		))
	}

	tickets, err := q.All(r.Context())
	if err != nil {
		h.log.Error("list tickets failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": tickets, "total": len(tickets)})
}

// StartTicket handles POST /{tenantID}/pos/kds/tickets/{id}/start
func (h *KDSHandler) StartTicket(w http.ResponseWriter, r *http.Request) {
	h.transitionTicket(w, r, entkdsticket.StatusInProgress)
}

// ReadyTicket handles POST /{tenantID}/pos/kds/tickets/{id}/ready
func (h *KDSHandler) ReadyTicket(w http.ResponseWriter, r *http.Request) {
	h.transitionTicket(w, r, entkdsticket.StatusReady)
}

// ServeTicket handles POST /{tenantID}/pos/kds/tickets/{id}/serve
func (h *KDSHandler) ServeTicket(w http.ResponseWriter, r *http.Request) {
	h.transitionTicket(w, r, entkdsticket.StatusServed)
}

// VoidTicket handles POST /{tenantID}/pos/kds/tickets/{id}/void
func (h *KDSHandler) VoidTicket(w http.ResponseWriter, r *http.Request) {
	h.transitionTicket(w, r, entkdsticket.StatusVoided)
}

func (h *KDSHandler) transitionTicket(w http.ResponseWriter, r *http.Request, toStatus entkdsticket.Status) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ticketID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid ticket id", http.StatusBadRequest)
		return
	}

	ticket, err := h.client.KDSTicket.Query().
		Where(entkdsticket.ID(ticketID), entkdsticket.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "ticket not found", http.StatusNotFound)
		return
	}

	now := time.Now()
	upd := ticket.Update().SetStatus(toStatus)

	switch toStatus {
	case entkdsticket.StatusInProgress:
		upd = upd.SetStartedAt(now)
	case entkdsticket.StatusReady, entkdsticket.StatusServed, entkdsticket.StatusVoided:
		upd = upd.SetCompletedAt(now)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("transition ticket failed", zap.Error(err))
		jsonError(w, "failed to update ticket", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// CallWaiter handles POST /{tenantID}/pos/kds/tickets/{id}/call-waiter
func (h *KDSHandler) CallWaiter(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ticketID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid ticket id", http.StatusBadRequest)
		return
	}

	ticket, err := h.client.KDSTicket.Query().
		Where(entkdsticket.ID(ticketID), entkdsticket.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "ticket not found", http.StatusNotFound)
		return
	}

	// TODO: Publish pos.kds.waiter.called event via outbox for notifications-service
	h.log.Info("waiter called", zap.Stringer("ticket_id", ticket.ID), zap.String("order_number", ticket.OrderNumber))

	jsonOK(w, map[string]any{"status": "waiter_called", "ticket_id": ticket.ID})
}

// CreateStation handles POST /{tenantID}/pos/kds/stations
func (h *KDSHandler) CreateStation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		OutletID       uuid.UUID `json:"outlet_id"`
		Name           string    `json:"name"`
		CategoryFilter []string  `json:"category_filter"`
		SortOrder      int       `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	station, err := h.client.KDSStation.Create().
		SetTenantID(tid).
		SetOutletID(input.OutletID).
		SetName(input.Name).
		SetCategoryFilter(input.CategoryFilter).
		SetSortOrder(input.SortOrder).
		Save(r.Context())
	if err != nil {
		h.log.Error("create kds station failed", zap.Error(err))
		jsonError(w, "failed to create station", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, station)
}

// UpdateStation handles PUT /{tenantID}/pos/kds/stations/{id}
func (h *KDSHandler) UpdateStation(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	stationID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid station id", http.StatusBadRequest)
		return
	}

	var input struct {
		Name           string   `json:"name"`
		CategoryFilter []string `json:"category_filter"`
		SortOrder      int      `json:"sort_order"`
		IsActive       *bool    `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	station, err := h.client.KDSStation.Query().
		Where(entkdsstation.ID(stationID), entkdsstation.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "station not found", http.StatusNotFound)
		return
	}

	upd := station.Update()
	if input.Name != "" {
		upd = upd.SetName(input.Name)
	}
	if input.CategoryFilter != nil {
		upd = upd.SetCategoryFilter(input.CategoryFilter)
	}
	if input.IsActive != nil {
		upd = upd.SetIsActive(*input.IsActive)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		jsonError(w, "failed to update station", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

func containsCaseInsensitive(s, substr string) bool {
	if len(s) < len(substr) {
		return false
	}
	sLower := make([]byte, len(s))
	subLower := make([]byte, len(substr))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		sLower[i] = c
	}
	for i := range substr {
		c := substr[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		subLower[i] = c
	}
	for i := 0; i <= len(sLower)-len(subLower); i++ {
		if string(sLower[i:i+len(subLower)]) == string(subLower) {
			return true
		}
	}
	return false
}
