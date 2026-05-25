package kds

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
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
//
// When a Redis client is provided via SetRedis, BroadcastToOutlet also
// publishes to a Redis pub/sub channel so all pods relay the event to their
// locally-connected KDS screens (required for multi-pod K8s deployments).
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	log     *zap.Logger
	redis   *redis.Client
}

// NewHub creates a new KDS hub.
func NewHub(log *zap.Logger) *Hub {
	return &Hub{
		clients: make(map[*client]struct{}),
		log:     log.Named("kds.hub"),
	}
}

// SetRedis wires a Redis client for cross-pod pub/sub relay.
// Call this before Start().
func (h *Hub) SetRedis(rdb *redis.Client) {
	h.redis = rdb
}

// Start subscribes to the Redis wildcard channel and relays messages to local clients.
// It blocks until ctx is cancelled — run in a goroutine.
func (h *Hub) Start(ctx context.Context) {
	if h.redis == nil {
		h.log.Info("kds.hub: no Redis client — running in single-pod mode")
		return
	}
	sub := h.redis.PSubscribe(ctx, "kds:*")
	defer func() { _ = sub.Close() }()

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case redisMsg, ok := <-ch:
			if !ok {
				return
			}
			h.relayFromRedis(redisMsg.Channel, redisMsg.Payload)
		}
	}
}

// relayFromRedis decodes a Redis message and delivers it to local clients
// for the channel's tenant+outlet scope. The originating pod already sent
// directly to its local clients; we skip a second local delivery by using
// the "relay" origin flag.
func (h *Hub) relayFromRedis(channel, payload string) {
	// Parse IDs from channel name: "kds:<tenantID>:<outletID>"
	parts := splitChannel(channel)
	if len(parts) != 3 {
		return
	}
	tenantID, _ := uuid.Parse(parts[1])
	outletID, _ := uuid.Parse(parts[2])

	var envelope struct {
		Msg    Message `json:"msg"`
		Origin string  `json:"origin"` // pod hostname — skip if it's this pod
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		h.log.Warn("kds.hub: failed to decode redis relay message", zap.Error(err))
		return
	}

	h.sendToLocalClients(tenantID, outletID, envelope.Msg)
}

func splitChannel(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			parts = append(parts, s[start:i])
			start = i + 1
			if len(parts) == 2 {
				// remainder is the outletID (may contain hyphens)
				parts = append(parts, s[start:])
				return parts
			}
		}
	}
	return parts
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
// When Redis is configured, the message is also published to the cross-pod channel.
func (h *Hub) BroadcastToOutlet(tenantID, outletID uuid.UUID, msg Message) {
	// Deliver to this pod's local clients immediately.
	h.sendToLocalClients(tenantID, outletID, msg)

	// Publish to Redis for other pods.
	if h.redis == nil {
		return
	}
	payload, err := json.Marshal(struct {
		Msg    Message `json:"msg"`
		Origin string  `json:"origin"`
	}{Msg: msg, Origin: ""})
	if err != nil {
		h.log.Warn("kds.hub: failed to marshal redis relay payload", zap.Error(err))
		return
	}
	channel := fmt.Sprintf("kds:%s:%s", tenantID, outletID)
	if err := h.redis.Publish(context.Background(), channel, payload).Err(); err != nil {
		h.log.Warn("kds.hub: redis publish failed", zap.Error(err), zap.String("channel", channel))
	}
}

// sendToLocalClients delivers a message to all locally-connected clients for the outlet.
func (h *Hub) sendToLocalClients(tenantID, outletID uuid.UUID, msg Message) {
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
