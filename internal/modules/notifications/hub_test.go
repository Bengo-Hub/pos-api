package notifications

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Broadcasting with no relay and no connected clients must be a safe no-op.
func TestBroadcast_NoRelayNoClients(t *testing.T) {
	h := NewHub(zap.NewNop())
	h.BroadcastToUser(uuid.New(), uuid.New(), Message{Type: "ping"})
	h.BroadcastToTenant(uuid.New(), Message{Type: "catalog_changed"})
}

// BroadcastToUser reaches only that user's sessions in that tenant; BroadcastToTenant reaches
// every session of the tenant and nothing outside it. A full buffer never blocks the sender.
func TestBroadcastScoping(t *testing.T) {
	h := NewHub(zap.NewNop())
	tid, uid := uuid.New(), uuid.New()
	match := h.fan.Subscribe(tid.String(), userScope(uid))
	otherUser := h.fan.Subscribe(tid.String(), userScope(uuid.New()))
	otherTenant := h.fan.Subscribe(uuid.New().String(), userScope(uid))

	for i := 0; i < 40; i++ { // more than the buffer: must not block
		h.BroadcastToUser(tid, uid, Message{Type: "etims_fiscalized"})
	}
	if len(match.C) == 0 || len(otherUser.C) != 0 || len(otherTenant.C) != 0 {
		t.Fatalf("user broadcast scoping wrong: match=%d other=%d otherTenant=%d", len(match.C), len(otherUser.C), len(otherTenant.C))
	}

	for len(match.C) > 0 {
		<-match.C
	}
	h.BroadcastToTenant(tid, Message{Type: "catalog_changed"})
	if len(match.C) != 1 || len(otherUser.C) != 1 || len(otherTenant.C) != 0 {
		t.Fatalf("tenant broadcast scoping wrong")
	}
	var m Message
	if err := json.Unmarshal(<-match.C, &m); err != nil || m.Type != "catalog_changed" {
		t.Fatalf("payload = %+v, %v", m, err)
	}
}

func TestPongFor(t *testing.T) {
	if pongFor([]byte(`{"type":"ping"}`)) == nil {
		t.Fatal("a ping must get a pong")
	}
	if pongFor([]byte(`{"type":"other"}`)) != nil || pongFor([]byte("junk")) != nil {
		t.Fatal("only pings are answered")
	}
}
