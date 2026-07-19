package printing

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// Hub gives the Local Print Agent a real-time wake-up channel so a freshly enqueued print job is
// claimed within milliseconds instead of on the agent's next poll tick. It is the push half of a
// push-with-poll-fallback design: the agent keeps a (now slow) safety-net poll, but normally waits
// on this socket and claims the instant a WakeOutlet signal arrives.
//
// Modeled on kds.Hub (same nhooyr.io/websocket + Redis pub/sub cross-pod relay): with several
// pos-api replicas behind one Service, the HTTP request that enqueues a job can land on ANY replica,
// but a given agent's live socket is pinned to whichever replica it connected to. So WakeOutlet both
// delivers to this pod's locally-connected agents AND publishes to a Redis channel that every pod
// relays to its own local agents — otherwise a wake-up would be lost whenever the enqueue and the
// socket live on different pods.
//
// The wake-up carries NO job payload — it is purely a "there is work, claim now" nudge. The actual
// claim stays HTTP + Postgres FOR UPDATE SKIP LOCKED (Queue.ClaimNext), so the multi-agent safety
// and idempotency guarantees are unchanged; the socket only removes the polling latency.
type Message struct {
	Type string `json:"type"` // "job_available" | "ping" | "pong"
}

// wsConn is one connected agent socket, scoped to (tenantID, outletID).
type wsConn struct {
	conn     *websocket.Conn
	tenantID uuid.UUID
	outletID uuid.UUID
	send     chan Message
}

// Hub manages all active print-agent sockets.
type Hub struct {
	mu    sync.RWMutex
	conns map[*wsConn]struct{}
	log   *zap.Logger
	redis *redis.Client
}

// NewHub builds the print-agent wake-up hub.
func NewHub(log *zap.Logger) *Hub {
	return &Hub{
		conns: make(map[*wsConn]struct{}),
		log:   log.Named("print.hub"),
	}
}

// SetRedis wires a Redis client for cross-pod wake-up relay. Call before Start.
func (h *Hub) SetRedis(rdb *redis.Client) { h.redis = rdb }

// Start subscribes to the cross-pod wake-up channel and relays to local agents. Blocks until ctx is
// cancelled — run in a goroutine. A nil Redis client degrades to single-pod mode (still correct when
// there is only one replica).
func (h *Hub) Start(ctx context.Context) {
	if h.redis == nil {
		h.log.Info("print.hub: no Redis client — single-pod wake-ups only")
		return
	}
	sub := h.redis.PSubscribe(ctx, "printjobs:*")
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
			tenantID, outletID, ok := parseWakeChannel(msg.Channel)
			if !ok {
				continue
			}
			h.wakeLocal(tenantID, outletID)
		}
	}
}

// parseWakeChannel decodes "printjobs:<tenantID>:<outletID>".
func parseWakeChannel(channel string) (tenantID, outletID uuid.UUID, ok bool) {
	var t, o string
	// Split on the first two ':' — the outletID (a UUID) contains hyphens but no colons.
	first := -1
	second := -1
	for i := 0; i < len(channel); i++ {
		if channel[i] == ':' {
			if first == -1 {
				first = i
			} else {
				second = i
				break
			}
		}
	}
	if first == -1 || second == -1 {
		return uuid.Nil, uuid.Nil, false
	}
	t = channel[first+1 : second]
	o = channel[second+1:]
	tenantID, err1 := uuid.Parse(t)
	outletID, err2 := uuid.Parse(o)
	if err1 != nil || err2 != nil {
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, outletID, true
}

// WakeOutlet nudges every agent for the outlet to claim now — locally, and (via Redis) on every
// other replica too. Called by Queue.Enqueue right after a job is created. Never blocks the enqueue.
func (h *Hub) WakeOutlet(tenantID, outletID uuid.UUID) {
	h.wakeLocal(tenantID, outletID)
	if h.redis == nil {
		return
	}
	channel := fmt.Sprintf("printjobs:%s:%s", tenantID, outletID)
	// Empty payload — the channel name carries the scope and the message is just a nudge.
	if err := h.redis.Publish(context.Background(), channel, "1").Err(); err != nil {
		h.log.Warn("print.hub: redis publish failed", zap.Error(err), zap.String("channel", channel))
	}
}

// wakeLocal signals every locally-connected agent for the outlet.
func (h *Hub) wakeLocal(tenantID, outletID uuid.UUID) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns {
		if c.tenantID == tenantID && c.outletID == outletID {
			select {
			case c.send <- Message{Type: "job_available"}:
			default:
				// Buffer full — the agent already has a pending wake-up it hasn't drained, so it
				// will claim this job on that pass anyway. Dropping is safe (wake-ups coalesce).
			}
		}
	}
}

// ServeWS registers an agent socket and blocks until it disconnects. Call after X-Agent-Key auth has
// resolved the agent's (tenantID, outletID).
func (h *Hub) ServeWS(ctx context.Context, conn *websocket.Conn, tenantID, outletID uuid.UUID) {
	c := &wsConn{
		conn:     conn,
		tenantID: tenantID,
		outletID: outletID,
		send:     make(chan Message, 8),
	}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		// Delete under the write lock BEFORE closing send: once deleted, wakeLocal (which holds only
		// an RLock) can no longer reach c, so closing send here can never race a send-on-closed-chan.
		h.mu.Lock()
		delete(h.conns, c)
		close(c.send)
		h.mu.Unlock()
	}()

	// Tell the agent to drain immediately on connect — anything enqueued while it was reconnecting
	// is claimed at once rather than waiting for the next enqueue or its slow safety-net poll.
	select {
	case c.send <- Message{Type: "job_available"}:
	default:
	}

	// Writer goroutine — exits when send is closed by the defer above (after the reader loop breaks).
	go func() {
		for msg := range c.send {
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := wsjson.Write(writeCtx, conn, msg)
			cancel()
			if err != nil {
				return
			}
		}
	}()

	// Reader loop — keepalive; any inbound message is a client ping, answered with a pong. Exits when
	// the socket drops (or ctx cancels), which unregisters the connection via the defer.
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			break
		}
		select {
		case c.send <- Message{Type: "pong"}:
		default:
		}
	}
}
