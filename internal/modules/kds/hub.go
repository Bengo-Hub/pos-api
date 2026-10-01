package kds

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

// Message is the envelope pushed to KDS WebSocket clients.
type Message struct {
	Type    string `json:"type"` // "ticket_update" | "queue_snapshot" | "ping"
	Payload any    `json:"payload"`
}

// Hub delivers kitchen ticket updates to KDS screens per outlet, on every replica, via the
// shared events.FanoutHub. The previous Redis relay never set or checked its origin, so every
// screen on the publishing replica received each ticket update twice.
type Hub struct {
	fan *eventslib.FanoutHub
	log *zap.Logger
}

const relayTopic = "kds"

// NewHub creates a hub with local-only delivery until SetRelay wires the cross-replica relay.
func NewHub(log *zap.Logger) *Hub {
	fan, _ := eventslib.NewFanoutHub(nil, relayTopic, 64)
	return &Hub{fan: fan, log: log.Named("kds.hub")}
}

// SetRelay wires the cross-replica relay. Call once at startup, before serving traffic.
func (h *Hub) SetRelay(b *eventslib.Broadcaster) {
	fan, err := eventslib.NewFanoutHub(b, relayTopic, 64)
	if err != nil {
		h.log.Warn("kds.hub: relay subscribe failed, single-pod delivery only", zap.Error(err))
	}
	h.fan = fan
}

func outletScope(outletID uuid.UUID) string { return "outlet:" + outletID.String() }

// BroadcastToOutlet sends msg to every KDS screen of the outlet (and screens watching all
// outlets), on every replica.
func (h *Hub) BroadcastToOutlet(tenantID, outletID uuid.UUID, msg Message) {
	b, err := json.Marshal(msg)
	if err != nil {
		h.log.Warn("kds.hub: marshal failed", zap.Error(err))
		return
	}
	h.fan.Publish(tenantID.String(), outletScope(outletID), b)
}

// ServeWS serves one upgraded KDS connection until it disconnects. A screen opened without an
// outlet (uuid.Nil) watches every outlet of the tenant.
func (h *Hub) ServeWS(ctx context.Context, conn *websocket.Conn, tenantID, outletID uuid.UUID) {
	scope := outletScope(outletID)
	if outletID == uuid.Nil {
		scope = eventslib.WildcardScope
	}
	sub := h.fan.Subscribe(tenantID.String(), scope)
	defer h.fan.Unsubscribe(sub)
	hello, _ := json.Marshal(Message{Type: "ping", Payload: map[string]any{"ts": time.Now().Unix()}})
	eventslib.Pump(ctx, realtime.Socket(conn), sub, hello, func(frame []byte) []byte {
		var in struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(frame, &in) != nil || in.Type != "ping" {
			return nil
		}
		b, _ := json.Marshal(Message{Type: "pong", Payload: map[string]any{"ts": time.Now().Unix()}})
		return b
	})
}
