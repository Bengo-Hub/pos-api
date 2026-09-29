package orders

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/kdsticket"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/modules/outletpolicy"
)

// TestServiceJobOrderFlow walks a print job from reception to the production board: the order
// opens (not draft), carries the normalized job header at the printing profile's first stage,
// gets a production ticket carrying the line's spec sheet, and then moves stage and proof status
// through UpdateJob with an audit event.
func TestServiceJobOrderFlow(t *testing.T) {
	client := newFiscalizationTestClient(t)
	ctx := context.Background()
	svc := NewService(client, Config{DefaultCurrency: "KES"}, zap.NewNop())

	tid := uuid.New()
	if _, err := client.Tenant.Create().SetID(tid).SetName("Demo").SetSlug("demo").Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	useCase := "services"
	outlet, err := client.Outlet.Create().
		SetTenantID(tid).SetTenantSlug("demo").SetCode("PRT").SetName("Demo Print & Branding").SetUseCase(useCase).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	if _, err := client.OutletSetting.Create().
		SetOutletID(outlet.ID).SetEnableKds(true).
		SetMetadata(map[string]any{outletpolicy.MetaKeyServiceProfile: outletpolicy.ProfilePrintingBranding}).
		Save(ctx); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	if _, err := client.KDSStation.Create().
		SetTenantID(tid).SetOutletID(outlet.ID).SetName("Production").SetStationType("all").
		Save(ctx); err != nil {
		t.Fatalf("seed station: %v", err)
	}

	order, err := svc.CreateOrder(ctx, CreateOrderRequest{
		TenantID:      tid,
		OutletID:      outlet.ID,
		DeviceID:      uuid.New(),
		UserID:        uuid.New(),
		OrderSubtype:  SubtypeServiceJob,
		CustomerPhone: "+254700111222",
		CustomerName:  "Acme Ltd",
		Metadata: map[string]any{"job": map[string]any{
			"due_at": "2026-10-01T09:00:00Z",
			"brief":  "Business cards with the attached logo",
			"attachments": []any{
				map[string]any{"kind": "link", "url": "https://drive.google.com/drive/folders/abc", "label": "Logo"},
			},
		}},
		Lines: []OrderLineInput{{
			CatalogItemID: uuid.New(),
			SKU:           "PRT-BC-500",
			Name:          "Business cards (500)",
			Category:      "Printing",
			Quantity:      1,
			UnitPrice:     2500,
			Metadata: map[string]any{"job_specs": map[string]any{
				"size": "85 x 55 mm", "material": "Card (300gsm)", "sides": "Double sided",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status != StatusOpen {
		t.Fatalf("job order status = %q, want open", order.Status)
	}
	job, _ := order.Metadata["job"].(map[string]any)
	if job == nil || job["stage"] != "design" || job["proof_status"] != ProofNone || job["brief"] == nil {
		t.Fatalf("job header not normalized: %+v", job)
	}

	tickets, err := client.KDSTicket.Query().Where(kdsticket.OrderID(order.ID)).All(ctx)
	if err != nil || len(tickets) != 1 {
		t.Fatalf("want 1 production ticket, got %d (%v)", len(tickets), err)
	}
	if tickets[0].OrderSubtype != SubtypeServiceJob {
		t.Errorf("ticket subtype = %q", tickets[0].OrderSubtype)
	}
	items := tickets[0].Items
	if len(items) != 1 {
		t.Fatalf("ticket items = %d", len(items))
	}
	specs, _ := items[0]["job_specs"].(map[string]any) // JSON round-trip through the ticket column
	if specs["size"] != "85 x 55 mm" {
		t.Errorf("ticket is missing the spec sheet: %+v", items[0])
	}

	stage, proof := "print", ProofApproved
	updated, err := svc.UpdateJob(ctx, tid, order.ID, uuid.New(), JobUpdate{Stage: &stage, ProofStatus: &proof})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if updated["stage"] != "print" || updated["proof_status"] != ProofApproved {
		t.Errorf("job not updated: %+v", updated)
	}
	events, _ := client.POSOrderEvent.Query().All(ctx)
	if len(events) != 1 || events[0].EventType != "job_updated" {
		t.Errorf("want one job_updated audit event, got %d", len(events))
	}

	// The final stage finishes production: tickets leave the board, the order waits for payment.
	final := "ready"
	if _, err := svc.UpdateJob(ctx, tid, order.ID, uuid.Nil, JobUpdate{Stage: &final}); err != nil {
		t.Fatalf("UpdateJob to ready: %v", err)
	}
	reloaded, _ := client.POSOrder.Get(ctx, order.ID)
	if reloaded.Status != StatusPendingPayment {
		t.Errorf("finished job status = %q, want pending_payment", reloaded.Status)
	}
	openTickets, _ := client.KDSTicket.Query().
		Where(kdsticket.OrderID(order.ID), kdsticket.StatusIn(kdsticket.StatusPending, kdsticket.StatusInProgress, kdsticket.StatusReady)).
		Count(ctx)
	if openTickets != 0 {
		t.Errorf("finished job still has %d open production tickets", openTickets)
	}

	// A job paid in full is still editable until collected; after collection it is locked.
	if err := client.POSOrder.UpdateOneID(order.ID).SetStatus(StatusCompleted).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	backToPrint := "print"
	if _, err := svc.UpdateJob(ctx, tid, order.ID, uuid.Nil, JobUpdate{Stage: &backToPrint}); err != nil {
		t.Errorf("a paid, uncollected job must stay editable: %v", err)
	}
	yes := true
	if _, err := svc.UpdateJob(ctx, tid, order.ID, uuid.Nil, JobUpdate{Collected: &yes}); err != nil {
		t.Fatalf("mark collected: %v", err)
	}
	if _, err := svc.UpdateJob(ctx, tid, order.ID, uuid.Nil, JobUpdate{Stage: &final}); err == nil {
		t.Error("a collected job must be locked")
	}

	// A stage from another trade is rejected by the outlet's profile.
	bad := "washing"
	if _, err := svc.UpdateJob(ctx, tid, order.ID, uuid.Nil, JobUpdate{Stage: &bad}); err == nil {
		t.Error("expected a laundry stage to be rejected on a print job")
	}
	// A retail order is not a job.
	retail, err := svc.CreateOrder(ctx, CreateOrderRequest{
		TenantID: tid, OutletID: outlet.ID, DeviceID: uuid.New(), UserID: uuid.New(), OrderSubtype: "retail",
		Lines: []OrderLineInput{{CatalogItemID: uuid.New(), SKU: "PEN", Name: "Pen", Quantity: 1, UnitPrice: 50}},
	})
	if err != nil {
		t.Fatalf("create retail: %v", err)
	}
	if _, err := svc.UpdateJob(ctx, tid, retail.ID, uuid.Nil, JobUpdate{Stage: &stage}); err == nil {
		t.Error("expected UpdateJob to reject a non-job order")
	}
}

// TestJobSummary counts active jobs by where they stand and totals the money on them.
func TestJobSummary(t *testing.T) {
	client := newFiscalizationTestClient(t)
	ctx := context.Background()
	svc := NewService(client, Config{DefaultCurrency: "KES"}, zap.NewNop())
	tid, outletID := uuid.New(), uuid.New()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	mk := func(status string, total, paid float64, job map[string]any, subtype string, created time.Time) {
		t.Helper()
		if _, err := client.POSOrder.Create().
			SetTenantID(tid).SetOutletID(outletID).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
			SetOrderNumber("J-" + uuid.NewString()[:8]).SetStatus(status).
			SetSubtotal(total).SetTaxTotal(0).SetTotalAmount(total).SetPaidTotal(paid).
			SetOrderSubtype(posorder.OrderSubtype(subtype)).SetCreatedAt(created).
			SetMetadata(map[string]any{"job": job}).
			Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// In production, overdue, proof sent, 70% deposit paid.
	mk(StatusOpen, 1000, 700, map[string]any{"due_at": "2026-09-28T09:00:00Z", "proof_status": ProofSent}, SubtypeServiceJob, now)
	// In production, due later today, nothing paid.
	mk(StatusOpen, 500, 0, map[string]any{"due_at": "2026-09-29T17:00:00Z"}, SubtypeServiceJob, now)
	// Finished, waiting for payment at collection.
	mk(StatusPendingPayment, 300, 100, map[string]any{"stage": "ready"}, SubtypeServiceJob, now)
	// Paid in full, not yet collected.
	mk(StatusCompleted, 800, 800, map[string]any{}, SubtypeServiceJob, now)
	// Paid and collected: not counted.
	mk(StatusCompleted, 800, 800, map[string]any{"collected_at": "2026-09-28T10:00:00Z"}, SubtypeServiceJob, now)
	// Paid, uncollected, but older than the window: not counted.
	mk(StatusCompleted, 800, 800, map[string]any{}, SubtypeServiceJob, now.Add(-100*24*time.Hour))
	// A plain retail sale: never counted.
	mk(StatusOpen, 999, 0, map[string]any{}, "retail", now)

	got, err := svc.JobSummary(ctx, tid, outletID, time.UTC, now)
	if err != nil {
		t.Fatal(err)
	}
	want := JobSummary{
		InProduction: 2, ReadyForCollection: 2, DueToday: 1, Overdue: 1, AwaitingProof: 1,
		BalanceOutstanding: 300 + 500 + 200, DepositsHeld: 700 + 100,
	}
	if got != want {
		t.Errorf("JobSummary = %+v, want %+v", got, want)
	}
}
