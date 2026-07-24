// Package notifications provides a WebSocket hub for real-time push notifications
// to POS terminals (floor staff, waiters). When the kitchen calls a waiter or an
// order moves to pending_payment, this hub delivers the event immediately so the
// terminal can play a sound alert without polling.
package notifications

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// Message is the envelope pushed to notification WebSocket clients.
type Message struct {
	Type    string `json:"type"`    // "order_ready" | "order_ready_for_payment" | "ping"
	Payload any    `json:"payload"`
}

// client is a single connected WebSocket session for a staff terminal.
type client struct {
	conn     *websocket.Conn
	tenantID uuid.UUID
	userID   uuid.UUID
	send     chan Message
}

// Hub manages active notification WebSocket connections, scoped to (tenantID, userID).
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	log     *zap.Logger
}

// NewHub creates a new notification hub.
func NewHub(log *zap.Logger) *Hub {
	return &Hub{
		clients: make(map[*client]struct{}),
		log:     log.Named("notif.hub"),
	}
}

// ServeWS upgrades the HTTP connection and blocks until the client disconnects.
func (h *Hub) ServeWS(ctx context.Context, conn *websocket.Conn, tenantID, userID uuid.UUID) {
	c := &client{
		conn:     conn,
		tenantID: tenantID,
		userID:   userID,
		send:     make(chan Message, 32),
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		// Close send so the writer goroutine's `range c.send` returns instead of
		// leaking forever after the client disconnects. Safe: the client is
		// removed from the map under the same lock, so BroadcastToUser (which
		// holds RLock) can no longer reach c to send on the closed channel.
		close(c.send)
		h.mu.Unlock()
	}()

	// Writer goroutine — flushes queued messages to the WebSocket connection.
	go func() {
		for msg := range c.send {
			if err := wsjson.Write(ctx, conn, msg); err != nil {
				return
			}
		}
	}()

	// Send initial ping to confirm connection.
	c.send <- Message{Type: "ping", Payload: map[string]any{"ts": time.Now().Unix()}}

	// Reader loop — keep alive and handle client pings.
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var m map[string]any
		if jsonErr := json.Unmarshal(raw, &m); jsonErr == nil {
			if t, _ := m["type"].(string); t == "ping" {
				select {
				case c.send <- Message{Type: "pong"}:
				default:
				}
			}
		}
	}
}

// BroadcastToUser delivers a message to every active WebSocket session for the given user.
func (h *Hub) BroadcastToUser(tenantID, userID uuid.UUID, msg Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if c.tenantID == tenantID && c.userID == userID {
			select {
			case c.send <- msg:
			default:
				h.log.Warn("notif.hub: send buffer full, dropping message",
					zap.Stringer("user_id", userID))
			}
		}
	}
}

// BroadcastToTenant delivers a message to EVERY active session for the tenant — used for
// tenant-wide signals like a catalog change, so all connected terminals refresh via push
// instead of waiting on their periodic version poll.
func (h *Hub) BroadcastToTenant(tenantID uuid.UUID, msg Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if c.tenantID == tenantID {
			select {
			case c.send <- msg:
			default:
				h.log.Warn("notif.hub: send buffer full, dropping tenant broadcast",
					zap.Stringer("tenant_id", tenantID))
			}
		}
	}
}

// BroadcastToOutlet delivers a message to every active session in the outlet (e.g. all floor staff).
func (h *Hub) BroadcastToOutlet(tenantID, outletID uuid.UUID, msg Message) {
	// outletID not tracked per-client; kept for future use — use BroadcastToUser per waiter.
	h.log.Debug("notif.hub: BroadcastToOutlet not implemented (use BroadcastToUser)",
		zap.Stringer("outlet_id", outletID))
}
