package returns

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// completeWithReason creates, approves and completes a one-unit return with the given reason.
func completeWithReason(t *testing.T, svc *Service, tid, outlet uuid.UUID, reason string, restock *bool) map[string]any {
	t.Helper()
	ctx := context.Background()
	order := seedOrder(t, svc.client, tid, outlet, "completed")
	lineID := firstLineID(t, svc.client, order.ID)
	req := customerReturn(order.ID, outlet, LineInput{OrderLineID: lineID, Quantity: 1})
	req.ReasonCode = reason
	ret, err := svc.CreateReturn(ctx, tid, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.ApproveReturn(ctx, tid, ret.ID, ApproveReturnRequest{Action: "approve"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	done, _, err := svc.CompleteReturn(ctx, tid, "t", ret.ID, CompleteReturnRequest{Restock: restock})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.Status != posreturn.StatusCompleted {
		t.Fatalf("not completed: %s", done.Status)
	}
	return done.Metadata
}

func TestRestockPolicy_DamagedWrittenOffByDefault(t *testing.T) {
	svc, _ := newTestService(t)
	tid, outlet := uuid.New(), uuid.New()

	for _, reason := range []string{"damaged", "defective", "expired"} {
		md := completeWithReason(t, svc, tid, outlet, reason, nil)
		if md[mdRestockDecision] != decisionWriteOff || md[mdRestockStatus] != restockNotRestocked {
			t.Errorf("%s: want written off by default, got %+v", reason, md)
		}
	}
	md := completeWithReason(t, svc, tid, outlet, "changed_mind", nil)
	if md[mdRestockDecision] != decisionRestock {
		t.Errorf("changed_mind should restock, got %+v", md)
	}
	md = completeWithReason(t, svc, tid, outlet, "", nil)
	if md[mdRestockDecision] != decisionRestock {
		t.Errorf("no reason (e.g. Edit Sale) should restock, got %+v", md)
	}
}

func TestRestockPolicy_ManagerOverrideAndOutletSetting(t *testing.T) {
	svc, client := newTestService(t)
	tid, outlet := uuid.New(), uuid.New()

	yes := true
	if md := completeWithReason(t, svc, tid, outlet, "damaged", &yes); md[mdRestockDecision] != decisionRestock {
		t.Errorf("manager override should restock a damaged return, got %+v", md)
	}
	no := false
	if md := completeWithReason(t, svc, tid, outlet, "changed_mind", &no); md[mdRestockDecision] != decisionWriteOff {
		t.Errorf("manager override should write off, got %+v", md)
	}

	// An outlet that saved an empty list restocks everything.
	ctx := context.Background()
	if _, err := client.Tenant.Create().SetID(tid).SetName("T").SetSlug("t-" + uuid.NewString()[:6]).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := client.Outlet.Create().SetID(outlet).SetTenantID(tid).SetTenantSlug("t").SetCode("O1").SetName("Main").Save(ctx); err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	if _, err := client.OutletSetting.Create().SetOutletID(outlet).
		SetMetadata(map[string]any{MetaKeyNoRestockReasons: []any{}}).Save(context.Background()); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	if md := completeWithReason(t, svc, tid, outlet, "damaged", nil); md[mdRestockDecision] != decisionRestock {
		t.Errorf("empty no-restock list should restock damaged, got %+v", md)
	}
}

func TestSanitizeNoRestockReasons(t *testing.T) {
	got := SanitizeNoRestockReasons([]string{"expired", "bogus", "damaged", "expired"})
	if len(got) != 2 || got[0] != "damaged" || got[1] != "expired" {
		t.Fatalf("want [damaged expired], got %v", got)
	}
	if def := NoRestockReasons(nil); len(def) != 3 {
		t.Fatalf("default should be damaged/defective/expired, got %v", def)
	}
}

func TestResync_RefusesWrittenOffReturn(t *testing.T) {
	svc, _ := newTestService(t)
	tid, outlet := uuid.New(), uuid.New()
	id := seedCompletedReturn(t, svc, tid, outlet, map[string]any{mdRestockDecision: decisionWriteOff, mdRestockStatus: restockNotRestocked})

	res, err := svc.ResyncRestock(context.Background(), tid, ResyncRestockRequest{DryRun: true})
	if err != nil || len(res.Items) != 0 {
		t.Fatalf("a written-off return is settled; outstanding scan must skip it, got %+v err=%v", res, err)
	}
	ret, _ := svc.client.POSReturn.Get(context.Background(), id)
	ret.Edges.Lines = nil
	svc.publisher = nil
	if err := svc.ApplyRestockOutcome(context.Background(), tid, id, RestockOutcome{Status: restockDone}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ret, _ = svc.client.POSReturn.Get(context.Background(), id)
	if ret.Metadata[mdRestockStatus] != restockNotRestocked {
		t.Fatalf("a stray outcome must not flip a write-off, got %v", ret.Metadata[mdRestockStatus])
	}
}
