package kds

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

// Message is the envelope pushed to KDS WebSocket clients.
type Message struct {
	Type    string `json:"type"`    // "ticket_update" | "queue_snapshot" | "ping"
	Payload any    `json:"payload"`
}

// client is a single connected KDS WebSocket session.
type client struct {
	conn     *websocket.Conn
	tenantID uuid.UUID
	outletID uuid.UUID
	send     chan Message
}

// Hub manages all active KDS WebSocket connections.
// Broadcasts are scoped to (tenantID, outletID) so each outlet's kitchen
// only receives events for its own orders.
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	log     *zap.Logger
}

// NewHub creates a new KDS hub.
func NewHub(log *zap.Logger) *Hub {
	return &Hub{
		clients: make(map[*client]struct{}),
		log:     log.Named("kds.hub"),
	}
}

// Register adds a client to the hub.
func (h *Hub) register(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.log.Debug("kds client registered",
		zap.Stringer("tenant_id", c.tenantID),
		zap.Stringer("outlet_id", c.outletID),
	)
}

// Unregister removes a client from the hub.
func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// BroadcastToOutlet pushes a message to all KDS clients for a specific outlet.
func (h *Hub) BroadcastToOutlet(tenantID, outletID uuid.UUID, msg Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients {
		if c.tenantID == tenantID && (c.outletID == outletID || c.outletID == uuid.Nil) {
			select {
			case c.send <- msg:
			default:
				// Client buffer full — drop message; client will resync on next poll
				h.log.Warn("kds: client send buffer full, dropping message",
					zap.Stringer("outlet_id", outletID))
			}
		}
	}
}

// ServeWS upgrades the HTTP connection to WebSocket and handles the client lifecycle.
// Call from the HTTP handler after auth/tenant middleware have run.
func (h *Hub) ServeWS(ctx context.Context, conn *websocket.Conn, tenantID, outletID uuid.UUID) {
	c := &client{
		conn:     conn,
		tenantID: tenantID,
		outletID: outletID,
		send:     make(chan Message, 64),
	}
	h.register(c)
	defer h.unregister(c)

	// Send initial ping to confirm connection
	_ = wsjson.Write(ctx, conn, Message{Type: "ping", Payload: map[string]any{"ts": time.Now().Unix()}})

	// Writer goroutine: push queued messages to the client
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case msg, ok := <-c.send:
				if !ok {
					return
				}
				writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := wsjson.Write(writeCtx, conn, msg)
				cancel()
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Reader loop: receive messages from client (e.g. bump requests)
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			break
		}

		var inbound struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if jsonErr := json.Unmarshal(raw, &inbound); jsonErr != nil {
			continue
		}

		// Client-side ping keepalive
		if inbound.Type == "ping" {
			select {
			case c.send <- Message{Type: "pong", Payload: map[string]any{"ts": time.Now().Unix()}}:
			default:
			}
		}
		// Other inbound message types (e.g. "bump") would be handled here
	}

	close(c.send)
	<-writerDone
}
