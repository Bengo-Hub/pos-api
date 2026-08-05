package saleedit

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/returns"
	"github.com/bengobox/pos-service/internal/modules/reversals"
	"github.com/bengobox/pos-service/internal/modules/treasury"
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

// newOrchestratorTestService mirrors newTestService (service_test.go) but wires the full
// dependency set the new orchestrator needs: reversalSvc, orderSvc, returnsSvc. treasury/
// inventory clients are left nil unless a test opts into a fake treasury server (see
// fakeTreasuryServer) — resolvePolicy treats a nil treasury client as "never fiscalized",
// exactly matching a tenant with no eTIMS integration at all. Reuses service_test.go's
// package-level "sqlite3" driver registration (init()).
func newOrchestratorTestService(t *testing.T) (*Service, *ent.Client) {
	t.Helper()
	return newOrchestratorTestServiceWithTreasury(t, nil)
}

// newOrchestratorTestServiceWithTreasury wires BOTH the orchestrator's own treasuryClient
// (used by resolvePolicy) AND the returns.Service it delegates to (used by
// CreateAndAutoComplete's actual settlement calls) — returns.Service takes its treasury
// client at construction time, not via a setter, so a fiscalized-path test must pass the
// same fake client here rather than trying to inject it after the fact.
func newOrchestratorTestServiceWithTreasury(t *testing.T, treasuryClient *treasury.Client) (*Service, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", "file:saleedit_orch_"+uuid.NewString()+"?mode=memory&cache=shared")
	t.Cleanup(func() { _ = client.Close() })
	orderSvc := orders.NewService(client, orders.Config{DefaultCurrency: "KES"}, zap.NewNop())
	revSvc := reversals.NewService(zap.NewNop(), client, orderSvc, treasuryClient, nil)
	returnsSvc := returns.NewService(zap.NewNop(), client, treasuryClient, nil)
	returnsSvc.SetOrderService(orderSvc)

	svc := NewService(zap.NewNop(), client, revSvc)
	svc.SetOrderService(orderSvc)
	svc.SetReturnsService(returnsSvc)
	svc.SetTreasuryClient(treasuryClient)
	return svc, client
}

// fakeTreasuryServer is a minimal httptest double for the treasury S2S endpoints the
// orchestrator/returns engine call for a FISCALIZED order: tax-profile (eTIMS-active),
// etims-fiscal (this order WAS transmitted — the real signal orders.IsFiscalized reads,
// matching treasury-api's actual /etims-fiscal/pos_sale/{id} endpoint since the 2026-08-05
// fix; NOT /invoices/by-reference, which stopped being populated for POS sales on
// 2026-06-09), refunds, and create-credit-note.
func fakeTreasuryServer(t *testing.T, invoiceID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/s2s/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case containsPath(r.URL.Path, "/tax-profile"):
			_ = json.NewEncoder(w).Encode(treasury.TaxProfileResponse{EtimsActivated: true, VATActive: true, VATRegistered: true})
		case containsPath(r.URL.Path, "/etims-fiscal/"):
			_ = json.NewEncoder(w).Encode(treasury.EtimsFiscal{CuInvoiceNo: invoiceID, ReceiptNo: "1", TransmittedAt: "2026-08-05T00:00:00Z"})
		case containsPath(r.URL.Path, "/invoices/by-reference"):
			_ = json.NewEncoder(w).Encode(treasury.InvoiceRef{ID: invoiceID, InvoiceNumber: "INV-1", Status: "sent"})
		case containsPath(r.URL.Path, "/refunds"):
			_ = json.NewEncoder(w).Encode(treasury.RefundResponse{ID: uuid.NewString(), Status: "succeeded", Amount: "0.00", Currency: "KES"})
		case containsPath(r.URL.Path, "/create-credit-note"):
			_ = json.NewEncoder(w).Encode(treasury.CreditNoteResponse{Number: "CN-1"})
		default:
			http.NotFound(w, r)
		}
	})
	return httptest.NewServer(mux)
}

func containsPath(path, sub string) bool {
	for i := 0; i+len(sub) <= len(path); i++ {
		if path[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func seedOrchestratorOrder(t *testing.T, client *ent.Client, tid, outletID uuid.UUID, qty, unitPrice float64) *ent.POSOrder {
	t.Helper()
	total := qty * unitPrice
	o, err := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(outletID).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(total).SetTaxTotal(0).SetTotalAmount(total).SetPaidTotal(total).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSOrderLine.Create().
		SetOrderID(o.ID).SetCatalogItemID(uuid.New()).SetSku("SKU-1").SetName("Sample Item").
		SetQuantity(qty).SetUnitPrice(unitPrice).SetTotalPrice(total).
		Save(context.Background()); err != nil {
		t.Fatalf("seed line: %v", err)
	}
	return o
}

func onlyLine(t *testing.T, client *ent.Client, orderID uuid.UUID) *ent.POSOrderLine {
	t.Helper()
	l, err := client.POSOrderLine.Query().Where(entposorderline.OrderID(orderID)).Only(context.Background())
	if err != nil {
		t.Fatalf("load line: %v", err)
	}
	return l
}

// TestEdit_NonFiscalized_MixedReduceAndIncrease_OneAtomicCall is the core new-architecture
// test: a non-fiscalized order gets a qty reduction on its existing line AND a brand-new
// line added, both applied TRUE in-place (no new order, no new receipt) via ONE Edit() call.
func TestEdit_NonFiscalized_MixedReduceAndIncrease_OneAtomicCall(t *testing.T) {
	svc, client := newOrchestratorTestService(t)
	tid := uuid.New()
	outletID := uuid.New()
	order := seedOrchestratorOrder(t, client, tid, outletID, 5, 100) // total 500
	line := onlyLine(t, client, order.ID)

	result, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "correction", RequestedBy: uuid.New(),
		Lines: []EditLine{
			{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 3, UnitPrice: 100}, // 5 -> 3
			{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "New Item", Quantity: 2, UnitPrice: 50},
		},
	})
	if err != nil {
		t.Fatalf("Edit failed: %v", err)
	}
	if result.Kind != "mixed" {
		t.Fatalf("expected kind=mixed, got %q", result.Kind)
	}
	if result.Fiscalized {
		t.Fatalf("expected non-fiscalized (no treasury client wired)")
	}
	if result.LinkedReversalID == nil {
		t.Fatalf("expected a linked reversal for the in-place reduction")
	}
	if result.LinkedReturnID != nil || result.LinkedAddendumOrderID != nil {
		t.Fatalf("non-fiscalized edit must never create a return or addendum order, got return=%v addendum=%v",
			result.LinkedReturnID, result.LinkedAddendumOrderID)
	}

	lines, err := client.POSOrderLine.Query().Where(entposorderline.OrderID(order.ID)).All(context.Background())
	if err != nil || len(lines) != 2 {
		t.Fatalf("expected exactly 2 lines on the SAME order (no new order created), got %d, err=%v", len(lines), err)
	}
	reloadedOrder, err := client.POSOrder.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	// 3 remaining of SKU-1 (300) + 2 of new SKU-2 (100) = 400.
	if reloadedOrder.TotalAmount != 400 {
		t.Fatalf("expected recomputed total 400, got %.2f", reloadedOrder.TotalAmount)
	}
}

// TestEdit_NonFiscalized_AllowsRepeatEditsOnSameLine confirms Edit() end-to-end (not just the
// underlying reversals engine directly) tolerates a second reduction of the same line — the
// exact reported production bug.
func TestEdit_NonFiscalized_AllowsRepeatEditsOnSameLine(t *testing.T) {
	svc, client := newOrchestratorTestService(t)
	tid := uuid.New()
	outletID := uuid.New()
	order := seedOrchestratorOrder(t, client, tid, outletID, 5, 100)
	line := onlyLine(t, client, order.ID)

	if _, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "first correction", RequestedBy: uuid.New(),
		Lines: []EditLine{{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 3, UnitPrice: 100}},
	}); err != nil {
		t.Fatalf("first Edit failed: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // avoid the test-only REV-<epoch-ms> fallback collision (no sequence service wired)
	if _, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "second correction", RequestedBy: uuid.New(),
		Lines: []EditLine{{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 1, UnitPrice: 100}},
	}); err != nil {
		t.Fatalf("second Edit on the same line failed (this is the reported bug): %v", err)
	}

	reloaded := onlyLine(t, client, order.ID)
	if reloaded.VoidedQty == nil || *reloaded.VoidedQty != 4 {
		t.Fatalf("expected cumulative voided_qty=4 after two reductions, got %v", reloaded.VoidedQty)
	}
}

// TestEdit_Fiscalized_ReductionCreatesReturn_NotReversal confirms a fiscalized order's line
// removal routes through returns.CreateAndAutoComplete (a real POSReturn), NOT the reversals
// engine, and leaves the original order's own lines/total untouched (confirmed decision).
func TestEdit_Fiscalized_ReductionCreatesReturn_NotReversal(t *testing.T) {
	ts := fakeTreasuryServer(t, "inv-123")
	defer ts.Close()
	treasuryClient := treasury.NewClient(ts.URL, "test-key", 5*time.Second)

	svc, client := newOrchestratorTestServiceWithTreasury(t, treasuryClient)

	tid := uuid.New()
	outletID := uuid.New()
	order := seedOrchestratorOrder(t, client, tid, outletID, 5, 100)
	line := onlyLine(t, client, order.ID)

	result, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "correction", RequestedBy: uuid.New(),
		Lines: []EditLine{{LineID: &line.ID, SKU: "SKU-1", Name: "Sample Item", Quantity: 3, UnitPrice: 100}},
	})
	if err != nil {
		t.Fatalf("Edit failed: %v", err)
	}
	if !result.Fiscalized {
		t.Fatalf("expected fiscalized=true (fake treasury server reports an invoice)")
	}
	if result.LinkedReturnID == nil {
		t.Fatalf("expected a linked POSReturn for the fiscalized reduction")
	}
	if result.LinkedReversalID != nil {
		t.Fatalf("fiscalized reduction must NOT go through the reversals engine, got linked_reversal_id=%v", result.LinkedReversalID)
	}

	reloadedLine := onlyLine(t, client, order.ID)
	if reloadedLine.VoidedQty != nil {
		t.Fatalf("expected original order line untouched (reduction visible only via the linked return), got voided_qty=%v", reloadedLine.VoidedQty)
	}
	reloadedOrder, err := client.POSOrder.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if reloadedOrder.TotalAmount != 500 {
		t.Fatalf("expected original order total untouched at 500, got %.2f", reloadedOrder.TotalAmount)
	}
}

// TestEdit_RejectsFullyReversedOrder confirms the terminal-state guard: an order already
// fully reversed cannot be edited again.
func TestEdit_RejectsFullyReversedOrder(t *testing.T) {
	svc, client := newOrchestratorTestService(t)
	tid := uuid.New()
	outletID := uuid.New()
	order := seedOrchestratorOrder(t, client, tid, outletID, 5, 100)

	if _, err := order.Update().SetStatus("refunded").Save(context.Background()); err != nil {
		t.Fatalf("seed refunded status: %v", err)
	}

	_, err := svc.Edit(context.Background(), tid, EditSaleRequest{
		OrderID: order.ID, Reason: "attempt", RequestedBy: uuid.New(),
		Lines: []EditLine{{CatalogItemID: uuid.New(), SKU: "SKU-2", Name: "New", Quantity: 1, UnitPrice: 10}},
	})
	if err == nil {
		t.Fatalf("expected an error editing a fully-reversed order")
	}
}
