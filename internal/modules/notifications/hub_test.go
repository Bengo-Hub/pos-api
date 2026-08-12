package notifications

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// BroadcastToUser/BroadcastToTenant with no Redis and no connected clients must be safe no-ops
// (single-pod / no-client path) — the common case in local dev and tests.
func TestBroadcast_NoRedisNoClients(t *testing.T) {
	h := NewHub(zap.NewNop())
	h.BroadcastToUser(uuid.New(), uuid.New(), Message{Type: "ping"})
	h.BroadcastToTenant(uuid.New(), Message{Type: "catalog_changed"})
}

// sendLocalUser must only signal the client matching BOTH tenant and user, and must never block
// when a client's buffer is full (a slow/stuck client can't wedge the broadcaster).
func TestSendLocalUser_ScopingAndNonBlocking(t *testing.T) {
	h := NewHub(zap.NewNop())
	tid := uuid.New()
	uid := uuid.New()

	match := &client{tenantID: tid, userID: uid, send: make(chan Message, 1)}
	otherUser := &client{tenantID: tid, userID: uuid.New(), send: make(chan Message, 1)}
	otherTenant := &client{tenantID: uuid.New(), userID: uid, send: make(chan Message, 1)}

	h.mu.Lock()
	h.clients[match] = struct{}{}
	h.clients[otherUser] = struct{}{}
	h.clients[otherTenant] = struct{}{}
	h.mu.Unlock()

	// Second send must not block even though the buffer (depth 1) is already full.
	h.sendLocalUser(tid, uid, Message{Type: "etims_fiscalized"})
	h.sendLocalUser(tid, uid, Message{Type: "etims_fiscalized"})

	if len(match.send) != 1 {
		t.Errorf("matching client got %d queued messages, want 1", len(match.send))
	}
	if len(otherUser.send) != 0 {
		t.Errorf("other-user client was signaled, want untouched")
	}
	if len(otherTenant.send) != 0 {
		t.Errorf("other-tenant client was signaled, want untouched")
	}
}

// sendLocalTenant must reach every client for the tenant regardless of user, and skip other tenants.
func TestSendLocalTenant_Scoping(t *testing.T) {
	h := NewHub(zap.NewNop())
	tid := uuid.New()

	a := &client{tenantID: tid, userID: uuid.New(), send: make(chan Message, 1)}
	b := &client{tenantID: tid, userID: uuid.New(), send: make(chan Message, 1)}
	other := &client{tenantID: uuid.New(), userID: uuid.New(), send: make(chan Message, 1)}

	h.mu.Lock()
	h.clients[a] = struct{}{}
	h.clients[b] = struct{}{}
	h.clients[other] = struct{}{}
	h.mu.Unlock()

	h.sendLocalTenant(tid, Message{Type: "catalog_changed"})

	if len(a.send) != 1 || len(b.send) != 1 {
		t.Errorf("both same-tenant clients should have received the broadcast")
	}
	if len(other.send) != 0 {
		t.Errorf("other-tenant client was signaled, want untouched")
	}
}

// parseNotifChannel must decode "notif:<tenantID>:<userID|*>" and reject anything else — this is
// the cross-pod relay's only defense against acting on an unrelated Redis channel (PSubscribe
// "notif:*" could in principle see traffic from a misconfigured/legacy publisher).
func TestParseNotifChannel(t *testing.T) {
	tid := uuid.New()
	uid := uuid.New()

	if gotT, gotU, ok := parseNotifChannel("notif:" + tid.String() + ":" + uid.String()); !ok || gotT != tid || gotU != uid.String() {
		t.Errorf("user-scoped channel: got (%s,%s,%v), want (%s,%s,true)", gotT, gotU, ok, tid, uid)
	}
	if gotT, gotU, ok := parseNotifChannel("notif:" + tid.String() + ":*"); !ok || gotT != tid || gotU != tenantWildcard {
		t.Errorf("tenant-wildcard channel: got (%s,%s,%v), want (%s,*,true)", gotT, gotU, ok, tid)
	}

	invalid := []string{
		"",
		"notif",
		"notif:only-one-part",
		"notpref:" + tid.String() + ":*",
		"notif:not-a-uuid:*",
	}
	for _, c := range invalid {
		if _, _, ok := parseNotifChannel(c); ok {
			t.Errorf("parseNotifChannel(%q) ok=true, want false", c)
		}
	}
}

// relayFromRedis must skip a message this same Hub instance originated (it was already delivered
// to local clients synchronously by the publishing call) — without this, every broadcast would be
// delivered twice to the originating pod's own clients.
func TestRelayFromRedis_SkipsOwnOrigin(t *testing.T) {
	h := NewHub(zap.NewNop())
	tid := uuid.New()
	c := &client{tenantID: tid, userID: uuid.New(), send: make(chan Message, 1)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	payload := `{"msg":{"type":"catalog_changed"},"origin":"` + h.originID + `"}`
	h.relayFromRedis("notif:"+tid.String()+":*", payload)

	if len(c.send) != 0 {
		t.Errorf("relay delivered a self-originated message locally, want skipped (already delivered by the publishing call)")
	}
}

// relayFromRedis must deliver a message from a DIFFERENT pod's origin — this is the actual
// cross-pod delivery path a terminal connected to a different pod than the broadcaster depends on.
func TestRelayFromRedis_DeliversOtherOrigin(t *testing.T) {
	h := NewHub(zap.NewNop())
	tid := uuid.New()
	uid := uuid.New()
	tenantWide := &client{tenantID: tid, userID: uuid.New(), send: make(chan Message, 1)}
	userScoped := &client{tenantID: tid, userID: uid, send: make(chan Message, 1)}
	h.mu.Lock()
	h.clients[tenantWide] = struct{}{}
	h.clients[userScoped] = struct{}{}
	h.mu.Unlock()

	// Tenant-wide relay from another pod reaches every client for the tenant.
	h.relayFromRedis("notif:"+tid.String()+":*", `{"msg":{"type":"catalog_changed"},"origin":"some-other-pod"}`)
	if len(tenantWide.send) != 1 {
		t.Errorf("tenant-wide relay: client did not receive the message")
	}

	// User-scoped relay from another pod reaches only the matching user.
	h.relayFromRedis("notif:"+tid.String()+":"+uid.String(), `{"msg":{"type":"etims_fiscalized"},"origin":"some-other-pod"}`)
	if len(userScoped.send) != 1 {
		t.Errorf("user-scoped relay: matching client did not receive the message")
	}
	if len(tenantWide.send) != 1 {
		t.Errorf("user-scoped relay leaked to a non-matching client")
	}
}

// Start with no Redis client must return immediately (single-pod mode) rather than blocking —
// callers always run it via `go h.Start(ctx)` and never wait on it.
func TestStart_NoRedisReturnsImmediately(t *testing.T) {
	h := NewHub(zap.NewNop())
	done := make(chan struct{})
	go func() {
		h.Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked with no Redis client configured")
	}
}
