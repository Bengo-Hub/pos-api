package returns

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

func seedCompletedReturn(t *testing.T, svc *Service, tid, outlet uuid.UUID, md map[string]any) uuid.UUID {
	t.Helper()
	order := seedOrder(t, svc.client, tid, outlet, "completed")
	lineID := firstLineID(t, svc.client, order.ID)
	ret, err := svc.CreateReturn(context.Background(), tid, customerReturn(order.ID, outlet, LineInput{OrderLineID: lineID, Quantity: 1}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.client.POSReturn.UpdateOneID(ret.ID).SetStatus(posreturn.StatusCompleted).SetMetadata(md).Save(context.Background()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return ret.ID
}

func TestApplyRestockOutcome_RecordsLocationAndNeverDowngrades(t *testing.T) {
	svc, client := newTestService(t)
	tid, outlet := uuid.New(), uuid.New()
	id := seedCompletedReturn(t, svc, tid, outlet, map[string]any{mdRestockStatus: restockPending, "approval_notes": "ok"})

	// Location means the outlet (branch): the outlet's name wins over the warehouse's, and a
	// shared warehouse with no outlet falls back to its own name.
	if _, err := client.Tenant.Create().SetID(tid).SetName("T").SetSlug("t-" + uuid.NewString()[:6]).Save(context.Background()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	branch, err := client.Outlet.Create().SetTenantID(tid).SetTenantSlug("t").SetCode("JW").SetName("Junior Wholesalers Branch").Save(context.Background())
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	out := RestockOutcome{Status: restockDone, Lines: []RestockOutcomeLine{
		{SKU: "SKU-1", Quantity: 1, WarehouseName: "JW Store", OutletID: branch.ID.String(), Method: "reversal"},
		{SKU: "SKU-2", Quantity: 1, WarehouseName: "HQ Warehouse", Method: "direct"},
	}}
	if err := svc.ApplyRestockOutcome(context.Background(), tid, id, out); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ret, _ := client.POSReturn.Get(context.Background(), id)
	if ret.Metadata[mdRestockStatus] != restockDone || ret.Metadata["approval_notes"] != "ok" {
		t.Fatalf("status/other metadata wrong: %+v", ret.Metadata)
	}
	locs, _ := ret.Metadata[mdRestockLocations].([]any)
	if len(locs) != 2 || locs[0] != "HQ Warehouse" || locs[1] != "Junior Wholesalers Branch" {
		t.Fatalf("locations should be the outlet name, else the shared warehouse: %+v", ret.Metadata[mdRestockLocations])
	}

	// A late failure report must not undo a confirmed restock.
	if err := svc.ApplyRestockOutcome(context.Background(), tid, id, RestockOutcome{Status: restockFailed, Error: "boom"}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	ret, _ = client.POSReturn.Get(context.Background(), id)
	if ret.Metadata[mdRestockStatus] != restockDone {
		t.Fatalf("restocked was downgraded to %v", ret.Metadata[mdRestockStatus])
	}

	// Another tenant's outcome for this id is ignored.
	if err := svc.ApplyRestockOutcome(context.Background(), uuid.New(), id, RestockOutcome{Status: restockFailed}); err != nil {
		t.Fatalf("foreign tenant should be a no-op, got %v", err)
	}
}

// Without explicit ids the resync picks only returns not confirmed restocked, including legacy
// returns with no restock key at all (every return completed before the 2026-10-02 fix).
func TestResyncRestock_DryRunPicksOnlyOutstanding(t *testing.T) {
	svc, _ := newTestService(t)
	tid, outlet := uuid.New(), uuid.New()
	legacy := seedCompletedReturn(t, svc, tid, outlet, map[string]any{"on_account_sale": false})
	pending := seedCompletedReturn(t, svc, tid, outlet, map[string]any{mdRestockStatus: restockPending})
	failed := seedCompletedReturn(t, svc, tid, outlet, map[string]any{mdRestockStatus: restockFailed})
	_ = seedCompletedReturn(t, svc, tid, outlet, map[string]any{mdRestockStatus: restockDone})
	_ = seedCompletedReturn(t, svc, tid, outlet, map[string]any{mdRestockStatus: restockNothing})

	res, err := svc.ResyncRestock(context.Background(), tid, ResyncRestockRequest{DryRun: true})
	if err != nil {
		t.Fatalf("resync: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, it := range res.Items {
		got[it.ReturnID] = true
		if it.Requested {
			t.Errorf("dry run must not request anything")
		}
	}
	if len(got) != 3 || !got[legacy] || !got[pending] || !got[failed] {
		t.Fatalf("want legacy+pending+failed only, got %d items: %+v", len(res.Items), res.Items)
	}
}
