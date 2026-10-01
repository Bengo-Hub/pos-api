// Package notifications provides a WebSocket hub for real-time push notifications
// to POS terminals (floor staff, waiters). When the kitchen calls a waiter or an
// order moves to pending_payment, this hub delivers the event immediately so the
// terminal can play a sound alert without polling.
package notifications

import (
	"context"
	"encoding/json"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"nhooyr.io/websocket"

	"github.com/bengobox/pos-service/internal/platform/realtime"
)

// Message is the envelope pushed to notification WebSocket clients.
type Message struct {
	Type    string `json:"type"` // "order_ready" | "order_ready_for_payment" | "catalog_changed" | "ping"
	Payload any    `json:"payload"`
}

// Hub manages notification WebSocket sessions per tenant and user.
//
// A terminal's socket lands on whichever pos-api replica handled its upgrade, while the event
// that triggers a broadcast (price or stock change, treasury balance update, eTIMS sign) can
// happen on any replica. The shared events.FanoutHub relays every message to all replicas and
// each delivers to its own sockets; this type owns only the WebSocket side. (The 2026-08-12
// Redis relay fixed the cross-pod gap first; it is now the one shared mechanism.)
type Hub struct {
	fan *eventslib.FanoutHub
	log *zap.Logger
}

const relayTopic = "notifications"

// NewHub creates a hub with local-only delivery until SetRelay wires the cross-replica relay.
func NewHub(log *zap.Logger) *Hub {
	l := log.Named("notif.hub")
	fan, _ := eventslib.NewFanoutHub(nil, relayTopic, 32)
	return &Hub{fan: fan, log: l}
}

// SetRelay wires the cross-replica relay. Call once at startup, before serving traffic.
func (h *Hub) SetRelay(b *eventslib.Broadcaster) {
	fan, err := eventslib.NewFanoutHub(b, relayTopic, 32)
	if err != nil {
		h.log.Warn("notif.hub: relay subscribe failed, single-pod delivery only", zap.Error(err))
	}
	h.fan = fan
}

func userScope(userID uuid.UUID) string { return "user:" + userID.String() }

// ServeWS serves one upgraded connection and blocks until the client disconnects.
func (h *Hub) ServeWS(ctx context.Context, conn *websocket.Conn, tenantID, userID uuid.UUID) {
	sub := h.fan.Subscribe(tenantID.String(), userScope(userID))
	defer h.fan.Unsubscribe(sub)
	hello, _ := json.Marshal(Message{Type: "ping", Payload: map[string]any{"ts": time.Now().Unix()}})
	eventslib.Pump(ctx, realtime.Socket(conn), sub, hello, pongFor)
}

// pongFor answers the client's JSON {"type":"ping"} keepalive.
func pongFor(frame []byte) []byte {
	var m struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(frame, &m) != nil || m.Type != "ping" {
		return nil
	}
	b, _ := json.Marshal(Message{Type: "pong"})
	return b
}

// BroadcastToUser delivers a message to every active session of the user, on every replica.
func (h *Hub) BroadcastToUser(tenantID, userID uuid.UUID, msg Message) {
	h.publish(tenantID, userScope(userID), msg)
}

// BroadcastToTenant delivers a message to every active session of the tenant (e.g. a catalog
// change, so all terminals refresh by push instead of their periodic version poll).
func (h *Hub) BroadcastToTenant(tenantID uuid.UUID, msg Message) {
	h.publish(tenantID, "", msg)
}

// BroadcastToOutlet is kept for callers; sessions are not tracked per outlet here, so use
// BroadcastToUser per waiter.
func (h *Hub) BroadcastToOutlet(tenantID, outletID uuid.UUID, msg Message) {
	h.log.Debug("notif.hub: BroadcastToOutlet not implemented (use BroadcastToUser)",
		zap.Stringer("outlet_id", outletID))
}

func (h *Hub) publish(tenantID uuid.UUID, scope string, msg Message) {
	b, err := json.Marshal(msg)
	if err != nil {
		h.log.Warn("notif.hub: marshal failed", zap.Error(err))
		return
	}
	h.fan.Publish(tenantID.String(), scope, b)
}
