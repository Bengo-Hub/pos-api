package returns

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
	"github.com/bengobox/pos-service/internal/ent/posreturn"
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
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:returnstest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	svc := NewService(zap.NewNop(), client, nil, nil)
	return svc, client
}

func seedOrder(t *testing.T, client *ent.Client, tid, outletID uuid.UUID, status string) *ent.POSOrder {
	t.Helper()
	o, err := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus(status).
		SetSubtotal(500).
		SetTaxTotal(0).
		SetTotalAmount(500).
		SetPaidTotal(500).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	line, err := client.POSOrderLine.Create().
		SetOrderID(o.ID).
		SetCatalogItemID(uuid.New()).
		SetSku("SKU-1").
		SetName("Sample Item").
		SetQuantity(1).
		SetUnitPrice(500).
		SetTotalPrice(500).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed line: %v", err)
	}
	_ = line
	return o
}

func firstLineID(t *testing.T, client *ent.Client, orderID uuid.UUID) uuid.UUID {
	t.Helper()
	lines, err := client.POSOrderLine.Query().All(context.Background())
	if err != nil || len(lines) == 0 {
		t.Fatalf("expected a seeded line, err=%v", err)
	}
	for _, l := range lines {
		if l.OrderID == orderID {
			return l.ID
		}
	}
	t.Fatalf("no line found for order %s", orderID)
	return uuid.Nil
}

// TestCreateAndAutoComplete_BypassesReturnWindowAndReturnableGuard is the core test for the
// new admin Edit-Sale path: a return-window-expired, non-returnable-flagged item must still
// go through create→approve→complete in one call when invoked via CreateAndAutoComplete,
// even though CreateReturn's own guards would refuse an ordinary customer-initiated return
// for the exact same order/item.
func TestCreateAndAutoComplete_BypassesReturnWindowAndReturnableGuard(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	outletID := uuid.New()
	order := seedOrder(t, client, tid, outletID, "completed")
	lineID := firstLineID(t, client, order.ID)

	// Flag the SKU non-returnable — CreateReturn would normally refuse this outright.
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku("SKU-1").
		SetIsReturnable(false).
		Save(context.Background()); err != nil {
		t.Fatalf("seed non-returnable override: %v", err)
	}

	// Sanity check: the ordinary customer-facing path (BypassGuards=false) DOES refuse it.
	_, err := svc.CreateReturn(context.Background(), tid, CreateReturnRequest{
		OrderID: order.ID, OutletID: outletID, ReturnType: "refund", Reason: "test",
		Lines:       []LineInput{{OrderLineID: lineID, SKU: "SKU-1", Name: "Sample Item", Quantity: 1, TotalPrice: 500}},
		RequestedBy: uuid.New(),
	})
	if err == nil || !strings.Contains(err.Error(), "return not allowed for") {
		t.Fatalf("expected the customer-facing path to refuse a non-returnable item, got: %v", err)
	}

	// The admin Edit-Sale path bypasses it and completes in one shot.
	ret, err := svc.CreateAndAutoComplete(context.Background(), tid, "test-tenant", AutoCompleteRequest{
		OrderID: order.ID, OutletID: outletID, Reason: "edit sale: qty correction",
		Lines:       []LineInput{{OrderLineID: lineID, SKU: "SKU-1", Name: "Sample Item", Quantity: 1, TotalPrice: 500}},
		RequestedBy: uuid.New(), Source: "edit_sale",
	})
	if err != nil {
		t.Fatalf("CreateAndAutoComplete failed: %v", err)
	}
	if ret.Status != posreturn.StatusCompleted {
		t.Fatalf("expected status completed, got %q", ret.Status)
	}
	if src, _ := ret.Metadata["source"].(string); src != "edit_sale" {
		t.Fatalf("expected metadata.source=edit_sale, got: %+v", ret.Metadata)
	}

	// The original order itself must be untouched (confirmed decision: reduction is visible
	// only via the linked return/credit-note, exactly like a customer return today).
	reloadedOrder, err := client.POSOrder.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloadedOrder.TotalAmount != 500 || reloadedOrder.PaidTotal != 500 {
		t.Fatalf("expected original order totals untouched, got total=%.2f paid=%.2f", reloadedOrder.TotalAmount, reloadedOrder.PaidTotal)
	}
	reloadedLine, err := client.POSOrderLine.Get(context.Background(), lineID)
	if err != nil {
		t.Fatalf("reload line: %v", err)
	}
	if reloadedLine.VoidedQty != nil {
		t.Fatalf("expected original order line quantity untouched, got voided_qty=%v", reloadedLine.VoidedQty)
	}
}

// TestCreateAndAutoComplete_SecondCallCreatesASecondIndependentReturn confirms repeat edits
// on the same order are supported (matches the unified correction-history policy): a second
// Edit-Sale reduction creates and completes its OWN new POSReturn rather than colliding with
// or being blocked by the first one.
func TestCreateAndAutoComplete_SecondCallCreatesASecondIndependentReturn(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	outletID := uuid.New()
	order := seedOrder(t, client, tid, outletID, "completed")
	lineID := firstLineID(t, client, order.ID)

	req := AutoCompleteRequest{
		OrderID: order.ID, OutletID: outletID, Reason: "edit sale",
		Lines:       []LineInput{{OrderLineID: lineID, SKU: "SKU-1", Name: "Sample Item", Quantity: 1, TotalPrice: 100}},
		RequestedBy: uuid.New(), Source: "edit_sale",
	}
	first, err := svc.CreateAndAutoComplete(context.Background(), tid, "test-tenant", req)
	if err != nil {
		t.Fatalf("first CreateAndAutoComplete failed: %v", err)
	}
	second, err := svc.CreateAndAutoComplete(context.Background(), tid, "test-tenant", req)
	if err != nil {
		t.Fatalf("second CreateAndAutoComplete failed: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("expected two independent POSReturn rows, got the same id twice")
	}

	count, err := client.POSReturn.Query().Where(posreturn.OrderID(order.ID)).Count(context.Background())
	if err != nil || count != 2 {
		t.Fatalf("expected 2 POSReturn rows against the order, got %d, err=%v", count, err)
	}
}
