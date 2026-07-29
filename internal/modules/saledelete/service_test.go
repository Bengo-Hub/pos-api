package saledelete

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entpossaleshred "github.com/bengobox/pos-service/internal/ent/possaleshred"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/reversals"
)

// ── pure-Go sqlite shim (mirrors handlers/held_items_test.go — ent needs a driver registered
// as "sqlite3"; modernc.org/sqlite registers as "sqlite"). Duplicated per-package since Go test
// binaries are per-package and init() in another package's test file isn't visible here.
type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() { sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}}) }

func newTestService(t *testing.T) (*Service, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:saledeletetest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	orderSvc := orders.NewService(client, orders.Config{DefaultCurrency: "KES"}, zap.NewNop())
	revSvc := reversals.NewService(zap.NewNop(), client, orderSvc, nil, nil)
	svc := NewService(zap.NewNop(), client, revSvc, nil, nil)
	return svc, client
}

func seedOrder(t *testing.T, client *ent.Client, tid uuid.UUID, status string) *ent.POSOrder {
	t.Helper()
	o, err := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(uuid.New()).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus(status).
		SetSubtotal(500).
		SetTaxTotal(0).
		SetTotalAmount(500).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return o
}

func seedLineAndPayment(t *testing.T, client *ent.Client, order *ent.POSOrder) {
	t.Helper()
	_, err := client.POSOrderLine.Create().
		SetOrderID(order.ID).
		SetCatalogItemID(uuid.New()).
		SetSku("SKU-1").
		SetName("Sample Item").
		SetQuantity(2).
		SetUnitPrice(250).
		SetTotalPrice(500).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed line: %v", err)
	}
	_, err = client.POSPayment.Create().
		SetOrderID(order.ID).
		SetTenderID(uuid.New()).
		SetAmount(500).
		SetStatus("completed").
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}
}

func TestDelete_RejectsNonFinalizedOrder(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrder(t, client, tid, "draft")

	_, err := svc.Delete(context.Background(), tid, Request{OrderID: order.ID, Reason: "test", RequestedBy: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "finalized") {
		t.Fatalf("expected finalized-status error, got: %v", err)
	}
}

func TestDelete_RejectsMissingReason(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrder(t, client, tid, "completed")

	_, err := svc.Delete(context.Background(), tid, Request{OrderID: order.ID, RequestedBy: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("expected reason-required error, got: %v", err)
	}
}

func TestDelete_RejectsAlreadyDeletedOrder(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrder(t, client, tid, "completed")
	if err := order.Update().SetDeletedAt(time.Now()).Exec(context.Background()); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	_, err := svc.Delete(context.Background(), tid, Request{OrderID: order.ID, Reason: "test", RequestedBy: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "already deleted") {
		t.Fatalf("expected already-deleted error, got: %v", err)
	}
}

func TestDelete_RejectsOrderWithReversalHistory(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrder(t, client, tid, "completed")
	_, err := client.POSReversal.Create().
		SetTenantID(tid).
		SetOrderID(order.ID).
		SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-1").
		SetScope("full").
		SetReason("prior correction").
		SetRequestedBy(uuid.New()).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed reversal: %v", err)
	}

	_, err = svc.Delete(context.Background(), tid, Request{OrderID: order.ID, Reason: "test", RequestedBy: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "already has a return, refund, or reversal") {
		t.Fatalf("expected correction-history error, got: %v", err)
	}
}

// TestDelete_NonFiscalized_HardDeletesAndSnapshots is the core new-logic test: with treasury and
// inventory clients unwired (nil — simulating "no fiscal invoice" and skipped external calls),
// Delete must hard-delete the order/lines/payments and leave a completed POSSaleShred snapshot.
func TestDelete_NonFiscalized_HardDeletesAndSnapshots(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrder(t, client, tid, "completed")
	seedLineAndPayment(t, client, order)
	orderID := order.ID

	result, err := svc.Delete(context.Background(), tid, Request{OrderID: orderID, Reason: "test shred", RequestedBy: uuid.New()})
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if result.Branch != "non_fiscalized" {
		t.Fatalf("expected non_fiscalized branch (nil treasury client), got %q", result.Branch)
	}
	if result.ShredID == nil {
		t.Fatalf("expected a shred record id")
	}

	if ok, _ := client.POSOrder.Query().Where(entposorder.ID(orderID)).Exist(context.Background()); ok {
		t.Fatalf("expected order row to be hard-deleted")
	}

	shred, err := client.POSSaleShred.Get(context.Background(), *result.ShredID)
	if err != nil {
		t.Fatalf("load shred record: %v", err)
	}
	if shred.Status != entpossaleshred.StatusCompleted {
		t.Fatalf("expected shred status completed, got %q", shred.Status)
	}
	if shred.OrderNumber != order.OrderNumber {
		t.Fatalf("expected shred to carry the original order_number")
	}
	snapshotOrderMap, ok := shred.Snapshot["order"].(map[string]any)
	if !ok || snapshotOrderMap["order_number"] != order.OrderNumber {
		t.Fatalf("expected snapshot to carry the order data, got: %+v", shred.Snapshot)
	}
}

// TestDelete_IsIdempotentToRetryAfterSuccess ensures a second Delete call on an order that no
// longer exists fails cleanly (order not found) rather than panicking — the shred is a one-shot,
// terminal operation.
func TestDelete_SecondCallAfterSuccessFailsCleanly(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrder(t, client, tid, "completed")
	seedLineAndPayment(t, client, order)

	if _, err := svc.Delete(context.Background(), tid, Request{OrderID: order.ID, Reason: "first", RequestedBy: uuid.New()}); err != nil {
		t.Fatalf("first Delete failed: %v", err)
	}
	if _, err := svc.Delete(context.Background(), tid, Request{OrderID: order.ID, Reason: "second", RequestedBy: uuid.New()}); err == nil {
		t.Fatalf("expected the second Delete to fail (order no longer exists)")
	}
}
