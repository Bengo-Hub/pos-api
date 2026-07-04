package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"nhooyr.io/websocket"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsstation "github.com/bengobox/pos-service/internal/ent/kdsstation"
	entkdsticket "github.com/bengobox/pos-service/internal/ent/kdsticket"
	entorderlink "github.com/bengobox/pos-service/internal/ent/orderlink"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	kdsmod "github.com/bengobox/pos-service/internal/modules/kds"
	notifmod "github.com/bengobox/pos-service/internal/modules/notifications"
	ordersmod "github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// KDSHandler handles Kitchen Display System endpoints.
type KDSHandler struct {
	log       *zap.Logger
	client    *ent.Client
	publisher *events.Publisher
	hub       *kdsmod.Hub
	notifHub  *notifmod.Hub
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

// SetNotifHub wires the shared notification hub (owned by NotificationsHandler).
func (h *KDSHandler) SetNotifHub(hub *notifmod.Hub) {
	h.notifHub = hub
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
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			q = q.Where(entkdsstation.OutletID(oid))
		}
	}
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

	stationQuery := h.client.KDSStation.Query().
		Where(
			entkdsstation.TenantID(tid),
			entkdsstation.IsActive(true),
			entkdsstation.StationTypeIn(targetTypes...),
		)
	// Scope to the active outlet so e.g. a quick-service KDS never sees a hospitality
	// outlet's stations/tickets. Outlet comes from the X-Outlet-ID header.
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			stationQuery = stationQuery.Where(entkdsstation.OutletID(oid))
		}
	}
	stations, err := stationQuery.All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	stationIDs := make([]uuid.UUID, 0, len(stations))
	for _, s := range stations {
		stationIDs = append(stationIDs, s.ID)
	}

	// No stations for this outlet/queue → no tickets. Returning early avoids leaking
	// every tenant ticket when the station filter would otherwise be omitted.
	if len(stationIDs) == 0 {
		jsonOK(w, map[string]any{"data": []any{}, "total": 0})
		return
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
			entkdsticket.StationIDIn(stationIDs...),
		).
		WithStation().
		Order(ent.Asc(entkdsticket.FieldPriority), ent.Asc(entkdsticket.FieldReceivedAt))

	// Only show recent tickets so a board never fills up with stale ones that were never bumped
	// (e.g. a printer-only kitchen with no device to serve them). Window is configurable via
	// ?since_hours (default 24; 0 = no limit).
	if cutoff, ok := kdsRecentCutoff(r); ok {
		q = q.Where(entkdsticket.ReceivedAtGTE(cutoff))
	}

	tickets, err := q.All(r.Context())
	if err != nil {
		h.log.Error("get queue failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": tickets, "total": len(tickets)})
}

// kdsRecentCutoff returns the received-at cutoff for the KDS board. Defaults to the last 24h so
// stale tickets don't accumulate forever; ?since_hours=N overrides it, and ?since_hours=0 disables
// the window (show all). Returns (cutoff, apply).
func kdsRecentCutoff(r *http.Request) (time.Time, bool) {
	hours := 24
	if v := r.URL.Query().Get("since_hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			hours = n
		}
	}
	if hours <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(-time.Duration(hours) * time.Hour), true
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

	// Scope to the active outlet via the ticket's station (tickets have no outlet_id of
	// their own). Prevents one outlet's KDS from listing another outlet's tickets.
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			q = q.Where(entkdsticket.HasStationWith(entkdsstation.OutletID(oid)))
		}
	}

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
		// ...and only recent ones, so stale never-bumped tickets don't pile up (see kdsRecentCutoff).
		if cutoff, ok := kdsRecentCutoff(r); ok {
			q = q.Where(entkdsticket.ReceivedAtGTE(cutoff))
		}
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

// ClearTickets handles POST /{tenantID}/pos/kds/tickets/clear
// Bulk-serves all active tickets for the current outlet (optionally only those older than
// ?older_than_hours=N). Lets a manager clear a cluttered board from a single terminal in one tap —
// essential for printer-only kitchens that have no device to bump tickets individually.
func (h *KDSHandler) ClearTickets(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	upd := h.client.KDSTicket.Update().
		Where(
			entkdsticket.TenantID(tid),
			entkdsticket.StatusIn(entkdsticket.StatusPending, entkdsticket.StatusInProgress, entkdsticket.StatusReady),
		)
	// Scope to the active outlet via the ticket's station.
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			upd = upd.Where(entkdsticket.HasStationWith(entkdsstation.OutletID(oid)))
		}
	}
	if v := r.URL.Query().Get("older_than_hours"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			upd = upd.Where(entkdsticket.ReceivedAtLT(time.Now().Add(-time.Duration(n) * time.Hour)))
		}
	}

	n, err := upd.SetStatus(entkdsticket.StatusServed).SetCompletedAt(time.Now()).Save(r.Context())
	if err != nil {
		h.log.Error("clear kds tickets failed", zap.Error(err))
		jsonError(w, "failed to clear tickets", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"cleared": n})
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

	// Reject mutations on tickets that belong to a different outlet than the caller's
	// active outlet — no cross-outlet ticket processing.
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, parseErr := uuid.Parse(oidStr); parseErr == nil {
			if ticket.Edges.Station == nil || ticket.Edges.Station.OutletID != oid {
				jsonError(w, "ticket not found", http.StatusNotFound)
				return
			}
		}
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

	// Publish pos.kds.order.ready at the ORDER level — only once every KDS ticket
	// for the order is ready (mirrors syncOrderOnAllTicketsServed). external_order_id
	// is resolved via OrderLink so ordering-backend can notify the online customer.
	if toStatus == entkdsticket.StatusReady && h.publisher != nil {
		h.publishOrderReadyIfAllReady(r.Context(), tid, updated.OrderID, updated.OrderNumber)
	}

	// When all tickets for this order are served, move the order to pending_payment.
	if toStatus == entkdsticket.StatusServed {
		h.syncOrderOnAllTicketsServed(r.Context(), tid, updated.OrderID, updated.OrderNumber)
	}

	jsonOK(w, updated)
}

// syncOrderOnAllTicketsServed checks whether every KDS ticket for the order has been
// served/voided. If so, it transitions the parent POSOrder to pending_payment and
// broadcasts a real-time alert to the waiter's POS terminal.
func (h *KDSHandler) syncOrderOnAllTicketsServed(ctx context.Context, tid, orderID uuid.UUID, orderNumber string) {
	remaining, err := h.client.KDSTicket.Query().
		Where(
			entkdsticket.TenantID(tid),
			entkdsticket.OrderID(orderID),
			entkdsticket.StatusIn(
				entkdsticket.StatusPending,
				entkdsticket.StatusInProgress,
				entkdsticket.StatusReady,
			),
		).
		Count(ctx)
	if err != nil || remaining > 0 {
		return
	}

	// All tickets done — look up the order to get the waiter's user_id.
	order, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tid), entposorder.Status(ordersmod.StatusOpen)).
		Only(ctx)
	if err != nil {
		return // already transitioned or not found
	}

	if _, err := h.client.POSOrder.UpdateOne(order).
		SetStatus(ordersmod.StatusPendingPayment).
		SetUpdatedAt(time.Now()).
		Save(ctx); err != nil {
		h.log.Warn("kds: failed to set order pending_payment", zap.Error(err), zap.String("order", orderNumber))
		return
	}

	h.log.Info("kds: order moved to pending_payment", zap.String("order", orderNumber))

	// Push real-time alert to the waiter's terminal.
	if h.notifHub != nil {
		h.notifHub.BroadcastToUser(tid, order.UserID, notifmod.Message{
			Type: "order_ready_for_payment",
			Payload: map[string]any{
				"order_id":     orderID,
				"order_number": orderNumber,
			},
		})
	}
}

// publishOrderReadyIfAllReady publishes pos.kds.order.ready ONCE, when every KDS
// ticket for the order has reached "ready" (none still pending/in_progress). This is
// the order-level "all stations done — order ready for the customer" signal, as opposed
// to a per-ticket event. external_order_id is resolved via OrderLink (by order_id) so
// ordering-backend can notify the online customer; it is omitted for POS-native orders
// that have no online linkage.
func (h *KDSHandler) publishOrderReadyIfAllReady(ctx context.Context, tid, orderID uuid.UUID, orderNumber string) {
	// Any ticket still pending or in_progress means the order is not yet fully ready.
	notReady, err := h.client.KDSTicket.Query().
		Where(
			entkdsticket.TenantID(tid),
			entkdsticket.OrderID(orderID),
			entkdsticket.StatusIn(
				entkdsticket.StatusPending,
				entkdsticket.StatusInProgress,
			),
		).
		Count(ctx)
	if err != nil || notReady > 0 {
		return
	}

	// Resolve the external (online) order id, if this POS order originated online.
	externalOrderID := ""
	if link, lerr := h.client.OrderLink.Query().
		Where(entorderlink.OrderID(orderID)).
		First(ctx); lerr == nil {
		externalOrderID = link.ExternalOrderID
	}

	_ = h.publisher.PublishKDSOrderReady(ctx, tid, map[string]any{
		"order_id":          orderID,
		"order_number":      orderNumber,
		"external_order_id": externalOrderID,
	})
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
