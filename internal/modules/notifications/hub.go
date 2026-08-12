// Package notifications provides a WebSocket hub for real-time push notifications
// to POS terminals (floor staff, waiters). When the kitchen calls a waiter or an
// order moves to pending_payment, this hub delivers the event immediately so the
// terminal can play a sound alert without polling.
package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// Message is the envelope pushed to notification WebSocket clients.
type Message struct {
	Type    string `json:"type"`    // "order_ready" | "order_ready_for_payment" | "catalog_changed" | "ping"
	Payload any    `json:"payload"`
}

// client is a single connected WebSocket session for a staff terminal.
type client struct {
	conn     *websocket.Conn
	tenantID uuid.UUID
	userID   uuid.UUID
	send     chan Message
}

// tenantWildcard is the Redis channel sentinel meaning "every client in the tenant" (used by
// BroadcastToTenant) rather than one specific user (BroadcastToUser) — a real uuid.UUID string
// can never collide with it.
const tenantWildcard = "*"

// Hub manages active notification WebSocket connections, scoped to (tenantID, userID).
//
// With several pos-api replicas behind one Service, a terminal's WebSocket lands on whichever pod
// handled its upgrade request, but the event/HTTP call that triggers a broadcast (an inventory
// price/stock change, a treasury balance update, an eTIMS fiscal block, a KDS order-ready signal)
// can land on ANY replica via its own NATS queue-group delivery or request routing. Without a
// cross-pod relay, a broadcast only ever reaches the (on average 1-in-N) terminals that happen to
// share the broadcasting pod — the rest silently fall back to their slow poll (catalog) or never
// get the update at all (eTIMS block, treasury balance push have no poll fallback). SetRedis+Start
// wires the same cross-pod pub/sub relay already used by kds.Hub/printing.Hub so every pod relays
// to its own locally-connected clients, restoring "every connected terminal gets it" regardless of
// which pod produced the event.
type Hub struct {
	mu       sync.RWMutex
	clients  map[*client]struct{}
	log      *zap.Logger
	redis    *redis.Client
	originID string // random per-process ID; lets relayFromRedis skip this pod's own publishes
}

// NewHub creates a new notification hub.
func NewHub(log *zap.Logger) *Hub {
	return &Hub{
		clients:  make(map[*client]struct{}),
		log:      log.Named("notif.hub"),
		originID: uuid.NewString(),
	}
}

// SetRedis wires a Redis client for cross-pod pub/sub relay. Call before Start.
func (h *Hub) SetRedis(rdb *redis.Client) { h.redis = rdb }

// Start subscribes to the cross-pod relay channel and relays messages to this pod's local clients.
// Blocks until ctx is cancelled — run in a goroutine. A nil Redis client degrades to single-pod
// mode (still correct when there is only one replica).
func (h *Hub) Start(ctx context.Context) {
	if h.redis == nil {
		h.log.Info("notif.hub: no Redis client — single-pod broadcast only")
		return
	}
	sub := h.redis.PSubscribe(ctx, "notif:*")
	defer func() { _ = sub.Close() }()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			h.relayFromRedis(msg.Channel, msg.Payload)
		}
	}
}

// relayEnvelope is the JSON payload published on the cross-pod Redis channel.
type relayEnvelope struct {
	Msg    Message `json:"msg"`
	Origin string  `json:"origin"`
}

// relayFromRedis decodes a cross-pod relay message and delivers it to this pod's local clients.
// Skips messages this same pod originally published: BroadcastToUser/BroadcastToTenant already
// deliver to local clients synchronously before publishing, so relaying our own publish back would
// double-deliver it to every locally-connected client on the originating pod.
func (h *Hub) relayFromRedis(channel, payload string) {
	tenantID, userPart, ok := parseNotifChannel(channel)
	if !ok {
		return
	}
	var env relayEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		h.log.Warn("notif.hub: failed to decode redis relay message", zap.Error(err))
		return
	}
	if env.Origin == h.originID {
		return
	}
	if userPart == tenantWildcard {
		h.sendLocalTenant(tenantID, env.Msg)
		return
	}
	userID, err := uuid.Parse(userPart)
	if err != nil {
		return
	}
	h.sendLocalUser(tenantID, userID, env.Msg)
}

// parseNotifChannel decodes "notif:<tenantID>:<userID|*>".
func parseNotifChannel(channel string) (tenantID uuid.UUID, userPart string, ok bool) {
	parts := strings.SplitN(channel, ":", 3)
	if len(parts) != 3 || parts[0] != "notif" {
		return uuid.Nil, "", false
	}
	tid, err := uuid.Parse(parts[1])
	if err != nil {
		return uuid.Nil, "", false
	}
	return tid, parts[2], true
}

// publish relays msg to every other pod via Redis. No-op when Redis is not configured (single-pod
// / dev mode) — local delivery already happened in the caller before this runs.
func (h *Hub) publish(tenantID uuid.UUID, userPart string, msg Message) {
	if h.redis == nil {
		return
	}
	payload, err := json.Marshal(relayEnvelope{Msg: msg, Origin: h.originID})
	if err != nil {
		h.log.Warn("notif.hub: failed to marshal redis relay payload", zap.Error(err))
		return
	}
	channel := fmt.Sprintf("notif:%s:%s", tenantID, userPart)
	if err := h.redis.Publish(context.Background(), channel, payload).Err(); err != nil {
		h.log.Warn("notif.hub: redis publish failed", zap.Error(err), zap.String("channel", channel))
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

// BroadcastToUser delivers a message to every active WebSocket session for the given user —
// locally, and (via Redis) on every other replica too.
func (h *Hub) BroadcastToUser(tenantID, userID uuid.UUID, msg Message) {
	h.sendLocalUser(tenantID, userID, msg)
	h.publish(tenantID, userID.String(), msg)
}

// BroadcastToTenant delivers a message to EVERY active session for the tenant — used for
// tenant-wide signals like a catalog change, so all connected terminals refresh via push
// instead of waiting on their periodic version poll. Locally, and (via Redis) on every other
// replica too.
func (h *Hub) BroadcastToTenant(tenantID uuid.UUID, msg Message) {
	h.sendLocalTenant(tenantID, msg)
	h.publish(tenantID, tenantWildcard, msg)
}

// sendLocalUser delivers msg to this pod's locally-connected sessions for (tenantID, userID).
func (h *Hub) sendLocalUser(tenantID, userID uuid.UUID, msg Message) {
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

// sendLocalTenant delivers msg to every one of this pod's locally-connected sessions for tenantID.
func (h *Hub) sendLocalTenant(tenantID uuid.UUID, msg Message) {
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
