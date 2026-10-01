package printing

import (
	"context"
	"encoding/json"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"nhooyr.io/websocket"

	"github.com/bengobox/pos-service/internal/platform/realtime"
)

// Message is the wake signal pushed to print agents.
type Message struct {
	Type string `json:"type"` // "job_available" | "ping" | "pong"
}

// Hub wakes print agents connected for an outlet when a print job is queued. The agent then
// claims jobs over HTTP (queue.go, FOR UPDATE SKIP LOCKED), so a wake is only a nudge and an
// extra one is harmless. Wakes reach agents on every replica via the shared events.FanoutHub.
type Hub struct {
	fan *eventslib.FanoutHub
	log *zap.Logger
}

const relayTopic = "printjobs"

var (
	jobAvailable, _ = json.Marshal(Message{Type: "job_available"})
	pong, _         = json.Marshal(Message{Type: "pong"})
)

// NewHub creates a hub with local-only wakes until SetRelay wires the cross-replica relay.
func NewHub(log *zap.Logger) *Hub {
	fan, _ := eventslib.NewFanoutHub(nil, relayTopic, 8)
	return &Hub{fan: fan, log: log.Named("print.hub")}
}

// SetRelay wires the cross-replica relay. Call once at startup, before serving traffic.
func (h *Hub) SetRelay(b *eventslib.Broadcaster) {
	fan, err := eventslib.NewFanoutHub(b, relayTopic, 8)
	if err != nil {
		h.log.Warn("print.hub: relay subscribe failed, single-pod wake-ups only", zap.Error(err))
	}
	h.fan = fan
}

// WakeOutlet tells every print agent of the outlet, on every replica, that a job is waiting.
func (h *Hub) WakeOutlet(tenantID, outletID uuid.UUID) {
	h.fan.Publish(tenantID.String(), "outlet:"+outletID.String(), jobAvailable)
}

// ServeWS serves one print-agent connection until it disconnects. A wake is sent on connect so
// an agent that was offline picks up jobs queued meanwhile; any client frame gets a pong.
func (h *Hub) ServeWS(ctx context.Context, conn *websocket.Conn, tenantID, outletID uuid.UUID) {
	sub := h.fan.Subscribe(tenantID.String(), "outlet:"+outletID.String())
	defer h.fan.Unsubscribe(sub)
	eventslib.Pump(ctx, realtime.Socket(conn), sub, jobAvailable, func([]byte) []byte { return pong })
}
