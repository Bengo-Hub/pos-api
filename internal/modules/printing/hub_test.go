package printing

import (
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestParseWakeChannel(t *testing.T) {
	tid := uuid.New()
	oid := uuid.New()
	channel := "printjobs:" + tid.String() + ":" + oid.String()

	gotT, gotO, ok := parseWakeChannel(channel)
	if !ok {
		t.Fatalf("parseWakeChannel(%q) ok=false, want true", channel)
	}
	if gotT != tid {
		t.Errorf("tenant = %s, want %s", gotT, tid)
	}
	if gotO != oid {
		t.Errorf("outlet = %s, want %s", gotO, oid)
	}
}

func TestParseWakeChannel_Invalid(t *testing.T) {
	cases := []string{
		"",
		"printjobs",
		"printjobs:only-one",
		"printjobs:not-a-uuid:" + uuid.New().String(),
		"printjobs:" + uuid.New().String() + ":not-a-uuid",
	}
	for _, c := range cases {
		if _, _, ok := parseWakeChannel(c); ok {
			t.Errorf("parseWakeChannel(%q) ok=true, want false", c)
		}
	}
}

// WakeOutlet with no Redis and no connected agents must be a safe no-op (single-pod / no-agent path).
func TestWakeOutlet_NoRedisNoAgents(t *testing.T) {
	h := NewHub(zap.NewNop())
	// Must not panic or block.
	h.WakeOutlet(uuid.New(), uuid.New())
}

// wakeLocal must only signal agents matching BOTH tenant and outlet, and must never block when a
// connection's buffer is full (wake-ups coalesce).
func TestWakeLocal_ScopingAndCoalescing(t *testing.T) {
	h := NewHub(zap.NewNop())
	tid := uuid.New()
	oid := uuid.New()

	match := &wsConn{tenantID: tid, outletID: oid, send: make(chan Message, 1)}
	otherOutlet := &wsConn{tenantID: tid, outletID: uuid.New(), send: make(chan Message, 1)}
	otherTenant := &wsConn{tenantID: uuid.New(), outletID: oid, send: make(chan Message, 1)}

	h.mu.Lock()
	h.conns[match] = struct{}{}
	h.conns[otherOutlet] = struct{}{}
	h.conns[otherTenant] = struct{}{}
	h.mu.Unlock()

	// Two wakes; the second must NOT block even though the buffer (depth 1) is already full.
	h.wakeLocal(tid, oid)
	h.wakeLocal(tid, oid)

	if len(match.send) != 1 {
		t.Errorf("matching conn got %d queued wakes, want 1 (coalesced)", len(match.send))
	}
	if got := (<-match.send).Type; got != "job_available" {
		t.Errorf("wake type = %q, want job_available", got)
	}
	if len(otherOutlet.send) != 0 {
		t.Errorf("other-outlet conn was signaled, want untouched")
	}
	if len(otherTenant.send) != 0 {
		t.Errorf("other-tenant conn was signaled, want untouched")
	}
}
