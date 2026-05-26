package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"nhooyr.io/websocket"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsstation "github.com/bengobox/pos-service/internal/ent/kdsstation"
	entkdsticket "github.com/bengobox/pos-service/internal/ent/kdsticket"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	kdsmod "github.com/bengobox/pos-service/internal/modules/kds"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// KDSHandler handles Kitchen Display System endpoints.
type KDSHandler struct {
	log       *zap.Logger
	client    *ent.Client
	publisher *events.Publisher
	hub       *kdsmod.Hub
}

func NewKDSHandler(log *zap.Logger, client *ent.Client) *KDSHandler {
	return &KDSHandler{
		log:    log,
		client: client,
		hub:    kdsmod.NewHub(log),
	}
}

func (h *KDSHandler) SetPublisher(p *events.Publisher) {
	h.publisher = p
}

// Hub returns the KDS WebSocket hub for broadcasting from NATS subscribers.
func (h *KDSHandler) Hub() *kdsmod.Hub {
	return h.hub
}

// StreamKDS handles GET /{tenantID}/pos/kds/stream — WebSocket endpoint.
// KDS tablets connect here to receive real-time ticket updates.
func (h *KDSHandler) StreamKDS(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var outletID uuid.UUID
	if outletParam := r.URL.Query().Get("outlet_id"); outletParam != "" {
		outletID, _ = uuid.Parse(outletParam)
	}

	conn, wsErr := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: false,
		OriginPatterns:     []string{"*"},
	})
	if wsErr != nil {
		h.log.Warn("kds: websocket upgrade failed", zap.Error(wsErr))
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	h.log.Info("kds: client connected",
		zap.Stringer("tenant_id", tid),
		zap.Stringer("outlet_id", outletID),
	)

	h.hub.ServeWS(r.Context(), conn, tid, outletID)

	h.log.Debug("kds: client disconnected",
		zap.Stringer("tenant_id", tid),
		zap.Stringer("outlet_id", outletID),
	)
}

// ListStations handles GET /{tenantID}/pos/kds/stations
func (h *KDSHandler) ListStations(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	// ?all=true returns inactive stations too (used by the settings UI for management)
	q := h.client.KDSStation.Query().Where(entkdsstation.TenantID(tid))
	if r.URL.Query().Get("all") != "true" {
		q = q.Where(entkdsstation.IsActive(true))
	}
	stations, err := q.Order(ent.Asc(entkdsstation.FieldSortOrder)).All(r.Context())
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

	// Resolve which station_types are relevant for this queue endpoint.
	// "kitchen" queue → kitchen + expo/all stations (expo sees everything)
	// "bar" queue → bar + expo/all stations
	targetTypes := []entkdsstation.StationType{
		entkdsstation.StationType(stationType),
		entkdsstation.StationTypeExpo,
		entkdsstation.StationTypeAll,
	}

	stations, err := h.client.KDSStation.Query().
		Where(
			entkdsstation.TenantID(tid),
			entkdsstation.IsActive(true),
			entkdsstation.StationTypeIn(targetTypes...),
		).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	stationIDs := make([]uuid.UUID, 0, len(stations))
	for _, s := range stations {
		stationIDs = append(stationIDs, s.ID)
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
		WithStation().
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

	// Broadcast real-time update to connected KDS screens for this outlet.
	if ticket.Edges.Station != nil {
		h.hub.BroadcastToOutlet(tid, ticket.Edges.Station.OutletID, kdsmod.Message{
			Type: "ticket_updated",
			Payload: map[string]any{
				"ticket_id":    updated.ID,
				"order_id":     updated.OrderID,
				"order_number": updated.OrderNumber,
				"station_id":   updated.StationID,
				"status":       string(toStatus),
				"completed_at": now,
			},
		})
	}

	if toStatus == entkdsticket.StatusReady && h.publisher != nil {
		_ = h.publisher.PublishKDSOrderReady(r.Context(), tid, map[string]any{
			"ticket_id":       updated.ID,
			"order_id":        updated.OrderID,
			"order_number":    updated.OrderNumber,
			"station_id":      updated.StationID,
			"table_reference": updated.TableReference,
			"completed_at":    now,
		})
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
		WithStation().
		Only(r.Context())
	if err != nil {
		jsonError(w, "ticket not found", http.StatusNotFound)
		return
	}

	// Look up the order to find the waiter's user_id and outlet_id.
	order, err := h.client.POSOrder.Query().
		Where(entposorder.ID(ticket.OrderID), entposorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		h.log.Warn("call-waiter: order not found for ticket", zap.Stringer("order_id", ticket.OrderID))
	}

	if order != nil {
		// Create an in-app notification for the waiter.
		_, notifErr := h.client.PosNotification.Create().
			SetTenantID(tid).
			SetOutletID(order.OutletID).
			SetUserID(order.UserID).
			SetNotificationType("kds.order_ready").
			SetTitle("Order Ready").
			SetBody("Order " + ticket.OrderNumber + " is ready for collection.").
			SetPayload(map[string]any{
				"ticket_id":    ticket.ID.String(),
				"order_id":     ticket.OrderID.String(),
				"order_number": ticket.OrderNumber,
			}).
			Save(r.Context())
		if notifErr != nil {
			h.log.Warn("call-waiter: failed to create notification", zap.Error(notifErr))
		}

		// Publish outbox event for future notifications-service integration.
		if h.publisher != nil {
			_ = h.publisher.PublishKDSWaiterCalled(r.Context(), tid, map[string]any{
				"ticket_id":    ticket.ID.String(),
				"order_id":     ticket.OrderID.String(),
				"order_number": ticket.OrderNumber,
				"waiter_user_id": order.UserID.String(),
				"outlet_id":    order.OutletID.String(),
			})
		}
	}

	// Broadcast waiter_called event to KDS screens on this outlet.
	if ticket.Edges.Station != nil {
		h.hub.BroadcastToOutlet(tid, ticket.Edges.Station.OutletID, kdsmod.Message{
			Type: "waiter_called",
			Payload: map[string]any{
				"ticket_id":    ticket.ID,
				"order_id":     ticket.OrderID,
				"order_number": ticket.OrderNumber,
				"station_id":   ticket.StationID,
			},
		})
	}

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
		OutletID       string   `json:"outlet_id"`
		Name           string   `json:"name"`
		StationType    string   `json:"station_type"` // kitchen | bar | cold | expo | all
		CategoryFilter []string `json:"category_filter"`
		SortOrder      int      `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	outletID, _ := uuid.Parse(input.OutletID)
	if outletID == uuid.Nil {
		if hv := r.Header.Get("X-Outlet-ID"); hv != "" {
			outletID, _ = uuid.Parse(hv)
		}
	}

	stationType := entkdsstation.StationType(input.StationType)
	if stationType == "" {
		stationType = entkdsstation.StationTypeKitchen
	}

	station, err := h.client.KDSStation.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetName(input.Name).
		SetStationType(stationType).
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
		StationType    string   `json:"station_type"`
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
	if input.StationType != "" {
		upd = upd.SetStationType(entkdsstation.StationType(input.StationType))
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

// DeleteStation handles DELETE /{tenantID}/pos/kds/stations/{id}
// Hard-deletes the station. Existing tickets are preserved (station_id becomes orphaned
// but historical data is intact). Rejects if the station has active (non-served/non-voided) tickets.
func (h *KDSHandler) DeleteStation(w http.ResponseWriter, r *http.Request) {
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

	station, err := h.client.KDSStation.Query().
		Where(entkdsstation.ID(stationID), entkdsstation.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "station not found", http.StatusNotFound)
		return
	}

	if err := h.client.KDSStation.DeleteOne(station).Exec(r.Context()); err != nil {
		h.log.Error("delete kds station failed", zap.Error(err))
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
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
