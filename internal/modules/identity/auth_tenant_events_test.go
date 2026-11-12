package identity

import (
	"context"
	"errors"
	"testing"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type fakeRenamer struct {
	calls []string
	err   error
}

func (f *fakeRenamer) RenameTenantSlug(_ context.Context, id uuid.UUID, slug string) (string, error) {
	f.calls = append(f.calls, id.String()+"="+slug)
	return "old", f.err
}

// A rename is passed through by tenant id; a missing slug or id does nothing; a clash is not
// retried, but any other failure is, so the event is redelivered.
func TestHandleTenantUpdated(t *testing.T) {
	ctx := context.Background()
	tid := uuid.New()

	r := &fakeRenamer{}
	h := NewAuthTenantEventHandler(r, zap.NewNop())
	if err := h.handleTenantUpdated(ctx, &sharedevents.Event{TenantID: tid, Payload: map[string]any{"slug": " new-slug "}}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0] != tid.String()+"=new-slug" {
		t.Fatalf("calls = %v", r.calls)
	}

	byPayload := &fakeRenamer{}
	h = NewAuthTenantEventHandler(byPayload, zap.NewNop())
	_ = h.handleTenantUpdated(ctx, &sharedevents.Event{Payload: map[string]any{"slug": "s", "tenant_id": tid.String()}})
	if len(byPayload.calls) != 1 {
		t.Fatalf("tenant id from payload not used: %v", byPayload.calls)
	}

	none := &fakeRenamer{}
	h = NewAuthTenantEventHandler(none, zap.NewNop())
	_ = h.handleTenantUpdated(ctx, &sharedevents.Event{TenantID: tid, Payload: map[string]any{}})
	_ = h.handleTenantUpdated(ctx, &sharedevents.Event{Payload: map[string]any{"slug": "s"}})
	if len(none.calls) != 0 {
		t.Fatalf("no slug or no tenant must not rename: %v", none.calls)
	}

	clash := &fakeRenamer{err: errors.New(`tenant.RenameTenantSlug: slug "s" is held by another local tenant`)}
	if err := NewAuthTenantEventHandler(clash, zap.NewNop()).handleTenantUpdated(ctx, &sharedevents.Event{TenantID: tid, Payload: map[string]any{"slug": "s"}}); err != nil {
		t.Fatalf("a clash must be acknowledged, got %v", err)
	}
	broken := &fakeRenamer{err: errors.New("db down")}
	if err := NewAuthTenantEventHandler(broken, zap.NewNop()).handleTenantUpdated(ctx, &sharedevents.Event{TenantID: tid, Payload: map[string]any{"slug": "s"}}); err == nil {
		t.Fatal("a transient failure must be retried")
	}
}
