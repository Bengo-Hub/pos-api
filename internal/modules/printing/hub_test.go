package printing

import (
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// WakeOutlet with no relay and no connected agents must be a safe no-op.
func TestWakeOutlet_NoRelayNoAgents(t *testing.T) {
	NewHub(zap.NewNop()).WakeOutlet(uuid.New(), uuid.New())
}

// A wake reaches only agents of that tenant AND outlet, and never blocks when an agent's
// buffer is full (wake-ups coalesce).
func TestWakeOutlet_ScopingAndCoalescing(t *testing.T) {
	h := NewHub(zap.NewNop())
	tid, oid := uuid.New(), uuid.New()
	match := h.fan.Subscribe(tid.String(), "outlet:"+oid.String())
	otherOutlet := h.fan.Subscribe(tid.String(), "outlet:"+uuid.New().String())
	otherTenant := h.fan.Subscribe(uuid.New().String(), "outlet:"+oid.String())

	for i := 0; i < 20; i++ {
		h.WakeOutlet(tid, oid)
	}
	if len(match.C) == 0 || len(otherOutlet.C) != 0 || len(otherTenant.C) != 0 {
		t.Fatalf("wake scoping wrong: match=%d otherOutlet=%d otherTenant=%d", len(match.C), len(otherOutlet.C), len(otherTenant.C))
	}
	if string(<-match.C) != string(jobAvailable) {
		t.Fatal("wake payload must be job_available")
	}
}
