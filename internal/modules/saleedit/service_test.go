package saleedit

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/reversals"
)

// ── pure-Go sqlite shim (duplicated per-package, see saledelete/service_test.go) ──
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
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:saleedittest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	orderSvc := orders.NewService(client, orders.Config{DefaultCurrency: "KES"}, zap.NewNop())
	revSvc := reversals.NewService(zap.NewNop(), client, orderSvc, nil, nil)
	svc := NewService(zap.NewNop(), client, revSvc)
	return svc, client
}

func seedOrderWithLine(t *testing.T, client *ent.Client, tid uuid.UUID, status string) *ent.POSOrder {
	t.Helper()
	o, err := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(uuid.New()).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus(status).
		SetSubtotal(300).
		SetTaxTotal(0).
		SetTotalAmount(300).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	_, err = client.POSOrderLine.Create().
		SetOrderID(o.ID).
		SetCatalogItemID(uuid.New()).
		SetSku("SKU-1").
		SetName("Sample Item").
		SetQuantity(1).
		SetUnitPrice(300).
		SetTotalPrice(300).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed line: %v", err)
	}
	return o
}

func TestPrepareEdit_RejectsNonFinalizedOrder(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrderWithLine(t, client, tid, "draft")

	_, err := svc.PrepareEdit(context.Background(), tid, Request{OrderID: order.ID, Reason: "test", RequestedBy: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "finalized") {
		t.Fatalf("expected finalized-status error, got: %v", err)
	}
}

func TestPrepareEdit_RejectsMissingReason(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrderWithLine(t, client, tid, "completed")

	_, err := svc.PrepareEdit(context.Background(), tid, Request{OrderID: order.ID, RequestedBy: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("expected reason-required error, got: %v", err)
	}
}

func TestPrepareEdit_RejectsOrderWithReturnHistory(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrderWithLine(t, client, tid, "completed")
	_, err := client.POSReturn.Create().
		SetTenantID(tid).
		SetOutletID(order.OutletID).
		SetOrderID(order.ID).
		SetReturnNumber("RET-1").
		SetReturnType("refund").
		SetStatus("pending").
		SetReason("prior return").
		SetReasonCode("other").
		SetRequestedBy(uuid.New()).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed return: %v", err)
	}

	_, err = svc.PrepareEdit(context.Background(), tid, Request{OrderID: order.ID, Reason: "test", RequestedBy: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "already has a return, refund, or reversal") {
		t.Fatalf("expected correction-history error, got: %v", err)
	}
}

// TestPrepareEdit_ReversesAndMarksSuperseded is the core new-logic test: with treasury/inventory
// clients unwired (nil), PrepareEdit must still run the reversal (pos_totals step always runs),
// void the order's lines, mark it "refunded", and stamp metadata.edit for lineage.
func TestPrepareEdit_ReversesAndMarksSuperseded(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrderWithLine(t, client, tid, "completed")

	result, err := svc.PrepareEdit(context.Background(), tid, Request{OrderID: order.ID, Reason: "fix qty", RequestedBy: uuid.New()})
	if err != nil {
		t.Fatalf("PrepareEdit failed: %v", err)
	}
	if result.ReversalNumber == "" {
		t.Fatalf("expected a reversal number")
	}

	reloaded, err := client.POSOrder.Query().Where(entposorder.ID(order.ID)).Only(context.Background())
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloaded.Status != "refunded" {
		t.Fatalf("expected order status refunded after full reversal, got %q", reloaded.Status)
	}
	editMeta, ok := reloaded.Metadata["edit"].(map[string]any)
	if !ok {
		t.Fatalf("expected metadata.edit to be stamped, got: %+v", reloaded.Metadata)
	}
	if editMeta["edit_reason"] != "fix qty" {
		t.Fatalf("expected edit reason to be recorded, got: %+v", editMeta)
	}
}

// TestPrepareEdit_RejectsDoubleEdit ensures the SAME order can't be prepared for editing twice.
func TestPrepareEdit_RejectsDoubleEdit(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	order := seedOrderWithLine(t, client, tid, "completed")

	if _, err := svc.PrepareEdit(context.Background(), tid, Request{OrderID: order.ID, Reason: "first", RequestedBy: uuid.New()}); err != nil {
		t.Fatalf("first PrepareEdit failed: %v", err)
	}
	// The order is now "refunded" (not a finalized status), so a second attempt is already
	// caught by the finalized-status guard — verifying it fails is what matters here.
	if _, err := svc.PrepareEdit(context.Background(), tid, Request{OrderID: order.ID, Reason: "second", RequestedBy: uuid.New()}); err == nil {
		t.Fatalf("expected the second PrepareEdit to fail")
	}
}
