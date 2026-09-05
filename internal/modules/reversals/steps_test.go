package reversals

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/orders"
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

// recordingTreasuryServer captures every /refunds call it receives so tests can assert on
// how stepTreasuryGL split the reversal across channels.
type recordingTreasuryServer struct {
	mu    sync.Mutex
	calls []treasury.RefundRequest
}

func newRecordingTreasuryServer(t *testing.T) (*recordingTreasuryServer, *treasury.Client) {
	t.Helper()
	rec := &recordingTreasuryServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req treasury.RefundRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Mirrors treasury-api's real ProcessRefund guard (arpa/balances.go: refID, err :=
		// uuid.Parse(req.ReferenceID)) — a fake server that accepted any string here would miss
		// the exact live bug found 2026-08-05, where stepTreasuryGL's split branch sent
		// "<uuid>-cash"/"<uuid>-ar" reference ids that treasury rejected with 400.
		if _, err := uuid.Parse(req.ReferenceID); err != nil {
			http.Error(w, "reference_id must be a uuid", http.StatusBadRequest)
			return
		}
		rec.mu.Lock()
		rec.calls = append(rec.calls, req)
		rec.mu.Unlock()
		_ = json.NewEncoder(w).Encode(treasury.RefundResponse{ID: uuid.NewString(), Status: "succeeded"})
	}))
	t.Cleanup(srv.Close)
	return rec, treasury.NewClient(srv.URL, "test-key", 0)
}

// setupOrderWithPayment seeds a tenant/order/payment triple. paidTotal is what the order
// currently shows as collected; the payment's own Amount matches it (single-payment orders,
// exactly like the E2E boi-enterprises test order this bug was found on).
func setupOrderWithPayment(t *testing.T, client *ent.Client, tenantID uuid.UUID, paidTotal float64) *ent.POSOrder {
	t.Helper()
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(paidTotal).SetTaxTotal(0).SetTotalAmount(paidTotal).SetPaidTotal(paidTotal).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(paidTotal).SetStatus("completed").
		SetPaymentData(map[string]any{"method": "cash"}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return order
}

func newTestReversalsService(t *testing.T, treasuryClient *treasury.Client) (*Service, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", "file:reversals_steps_"+uuid.NewString()+"?mode=memory&cache=shared")
	t.Cleanup(func() { _ = client.Close() })
	orderSvc := orders.NewService(client, orders.Config{DefaultCurrency: "KES"}, zap.NewNop())
	return NewService(zap.NewNop(), client, orderSvc, treasuryClient, nil), client
}

// TestStepTreasuryGL_FullyCashBacked reproduces the common, pre-existing case (the whole
// reversed amount was actually netted from a real payment) and asserts behavior is unchanged
// from before this fix: exactly one /refunds call, for the full amount, via the resolved
// channel.
func TestStepTreasuryGL_FullyCashBacked(t *testing.T) {
	rec, tc := newRecordingTreasuryServer(t)
	svc, client := newTestReversalsService(t, tc)
	tenantID := uuid.New()
	order := setupOrderWithPayment(t, client, tenantID, 300)

	rev, err := client.POSReversal.Create().
		SetTenantID(tenantID).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-1").SetScope("partial").SetStatus("pending").SetReason("test").
		SetRefundChannel("cash").SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(200).SetTaxAmount(0).SetCostAmount(0).
		SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed reversal: %v", err)
	}

	payments, _ := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).All(context.Background())
	pd := map[string]any{"method": "cash", "reversal": map[string]any{
		"reversal_number": "REV-1", "netted_from": 300.0, "netted_to": 100.0,
	}}
	if _, err := payments[0].Update().SetPaymentData(pd).Save(context.Background()); err != nil {
		t.Fatalf("stamp payment: %v", err)
	}

	_, detail, skip, err := svc.stepTreasuryGL(context.Background(), rev, "tenant-slug")
	if err != nil || skip {
		t.Fatalf("stepTreasuryGL() error=%v skip=%v", err, skip)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 refund call, got %d: %+v", len(rec.calls), rec.calls)
	}
	if rec.calls[0].Amount != 200 || rec.calls[0].RefundChannel != "cash" {
		t.Errorf("call = %+v, want amount=200 channel=cash", rec.calls[0])
	}
	t.Logf("detail: %s", detail)
}

// TestStepTreasuryGL_SplitsCashAndARWhenReversalExceedsRealCash is the regression test for
// the live bug found 2026-08-05: an Edit-Sale increase posted KES 1400 straight to AR
// (never collected as cash), then a follow-up edit removed that same line. Only KES 100 was
// ever real cash (stamped by the preceding stepPOSTotals as netted_from-netted_to), so the
// GL reversal must split into a KES 100 cash refund + a KES 1300 AR write-off — never a
// single KES 1400 "cash" refund for money the business never received.
func TestStepTreasuryGL_SplitsCashAndARWhenReversalExceedsRealCash(t *testing.T) {
	rec, tc := newRecordingTreasuryServer(t)
	svc, client := newTestReversalsService(t, tc)
	tenantID := uuid.New()
	order := setupOrderWithPayment(t, client, tenantID, 100)

	rev, err := client.POSReversal.Create().
		SetTenantID(tenantID).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-2").SetScope("partial").SetStatus("pending").SetReason("remove edit-added line").
		SetRefundChannel("cash").SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(1400).SetTaxAmount(140).SetCostAmount(700).
		SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed reversal: %v", err)
	}

	payments, _ := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).All(context.Background())
	pd := map[string]any{"method": "cash", "reversal": map[string]any{
		"reversal_number": "REV-2", "netted_from": 100.0, "netted_to": 0.0,
	}}
	if _, err := payments[0].Update().SetPaymentData(pd).Save(context.Background()); err != nil {
		t.Fatalf("stamp payment: %v", err)
	}

	_, _, skip, err := svc.stepTreasuryGL(context.Background(), rev, "tenant-slug")
	if err != nil || skip {
		t.Fatalf("stepTreasuryGL() error=%v skip=%v", err, skip)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 2 {
		t.Fatalf("expected 2 refund calls (cash + AR write-off), got %d: %+v", len(rec.calls), rec.calls)
	}
	var cashCall, arCall *treasury.RefundRequest
	for i := range rec.calls {
		switch rec.calls[i].RefundChannel {
		case "cash":
			cashCall = &rec.calls[i]
		case "offset_invoice":
			arCall = &rec.calls[i]
		}
	}
	if cashCall == nil || arCall == nil {
		t.Fatalf("expected one cash call and one offset_invoice call, got %+v", rec.calls)
	}
	if cashCall.Amount != 100 {
		t.Errorf("cash portion = %.2f, want 100", cashCall.Amount)
	}
	if arCall.Amount != 1300 {
		t.Errorf("AR write-off portion = %.2f, want 1300", arCall.Amount)
	}
	if cashCall.TaxAmount+arCall.TaxAmount != 140 {
		t.Errorf("tax should split to sum back to 140, got cash=%.2f ar=%.2f", cashCall.TaxAmount, arCall.TaxAmount)
	}
	if cashCall.ReferenceID == arCall.ReferenceID {
		t.Error("cash and AR portions must use distinct reference ids (idempotency)")
	}
}

// TestNetPayments_ExcludesOnAccountFromPaidTotal is the regression test for the live bug found
// on order POS-000155 (BOI Enterprises, invoice 000155, 2026-08-06): netting an on-account
// (credit) payment row down during a partial Edit-Sale reduction must NOT feed its post-netting
// amount into order.PaidTotal — an on-account row is a treasury AR debt, never cash banked, and
// payments.RecomputePaidTotal (the single source of truth for "money actually collected")
// already excludes it everywhere else. Before this fix, netPayments summed EVERY touched
// payment's netted amount regardless of method, so reducing a 5-unit on-account line to 3 units
// (netting its on_account payment from 3500 down to 2100) made the order instantly show
// "paid 2,100" though the till had collected nothing yet.
func TestNetPayments_ExcludesOnAccountFromPaidTotal(t *testing.T) {
	svc, client := newTestReversalsService(t, nil)
	tenantID := uuid.New()
	ctx := context.Background()

	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(3500).SetTaxTotal(0).SetTotalAmount(3500).SetPaidTotal(0).
		SetMetadata(map[string]any{"on_account": true}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(3500).SetStatus("completed").
		SetPaymentData(map[string]any{"method": "on_account"}).
		Save(ctx); err != nil {
		t.Fatalf("seed on-account payment: %v", err)
	}

	rev, err := client.POSReversal.Create().
		SetTenantID(tenantID).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-OA-1").SetScope("partial").SetStatus("pending").SetReason("test").
		SetRefundChannel("offset_invoice").SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(1400).SetTaxAmount(0).SetCostAmount(0).
		SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed reversal: %v", err)
	}

	if err := svc.netPayments(ctx, client, rev, order, order.Metadata); err != nil {
		t.Fatalf("netPayments() error = %v", err)
	}

	updated, err := client.POSOrder.Get(ctx, order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if updated.PaidTotal != 0 {
		t.Errorf("PaidTotal = %.2f, want 0 (the on-account row was never real cash)", updated.PaidTotal)
	}

	payment, err := client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).Only(ctx)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if payment.Amount != 2100 {
		t.Errorf("netted payment amount = %.2f, want 2100 (3500 - 1400)", payment.Amount)
	}
}

// TestStepTreasuryGL_OnAccountNettedRoutesToOffsetInvoice is the regression test for the
// second half of the same live bug: even after netPayments correctly nets an on-account row
// (see TestNetPayments_ExcludesOnAccountFromPaidTotal above), cashNettedForReversal must not
// count that netted amount as "cash" either — otherwise stepTreasuryGL posts the whole
// reversed amount through the cash refund channel (Dr Revenue / Cr Cash) instead of
// offset_invoice, permanently understating treasury's cash ledger AND leaving the customer's
// AR balance stuck at the pre-reversal figure forever (confirmed live: BOI Enterprises
// customer Miss Sarah Lodwa's treasury customer_balances stayed at balance_due 1,400 for
// stock that had actually been returned/removed from the sale).
func TestStepTreasuryGL_OnAccountNettedRoutesToOffsetInvoice(t *testing.T) {
	rec, tc := newRecordingTreasuryServer(t)
	svc, client := newTestReversalsService(t, tc)
	tenantID := uuid.New()
	ctx := context.Background()

	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(3500).SetTaxTotal(0).SetTotalAmount(3500).SetPaidTotal(0).
		SetMetadata(map[string]any{"on_account": true}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(2100).SetStatus("completed").
		SetPaymentData(map[string]any{"method": "on_account"}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed on-account payment: %v", err)
	}

	rev, err := client.POSReversal.Create().
		SetTenantID(tenantID).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-OA-2").SetScope("partial").SetStatus("pending").SetReason("edit sale reduce").
		SetRefundChannel("offset_invoice").SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(1400).SetTaxAmount(0).SetCostAmount(0).
		SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed reversal: %v", err)
	}

	// Simulate netPayments having already netted this on-account row for THIS reversal (the
	// breadcrumb cashNettedForReversal reads).
	pd := map[string]any{"method": "on_account", "reversal": map[string]any{
		"reversal_number": "REV-OA-2", "netted_from": 3500.0, "netted_to": 2100.0,
	}}
	if _, err := payment.Update().SetPaymentData(pd).Save(ctx); err != nil {
		t.Fatalf("stamp payment: %v", err)
	}

	_, _, skip, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug")
	if err != nil || skip {
		t.Fatalf("stepTreasuryGL() error=%v skip=%v", err, skip)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly 1 refund call (fully AR-backed), got %d: %+v", len(rec.calls), rec.calls)
	}
	if rec.calls[0].RefundChannel != "offset_invoice" || rec.calls[0].Amount != 1400 {
		t.Errorf("call = %+v, want amount=1400 channel=offset_invoice (never cash — no cash was ever collected for an on-account row)", rec.calls[0])
	}
}

// TestStepTreasuryGL_FullyARBacked covers a pure on-account sale (no cash ever collected) —
// the reversal must go through offset_invoice alone, a single call, matching pre-fix
// behavior for that case.
func TestStepTreasuryGL_FullyARBacked(t *testing.T) {
	rec, tc := newRecordingTreasuryServer(t)
	svc, client := newTestReversalsService(t, tc)
	tenantID := uuid.New()
	order := setupOrderWithPayment(t, client, tenantID, 0)

	rev, err := client.POSReversal.Create().
		SetTenantID(tenantID).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-3").SetScope("partial").SetStatus("pending").SetReason("test").
		SetRefundChannel("offset_invoice").SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(500).SetTaxAmount(0).SetCostAmount(0).
		SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed reversal: %v", err)
	}

	_, _, skip, err := svc.stepTreasuryGL(context.Background(), rev, "tenant-slug")
	if err != nil || skip {
		t.Fatalf("stepTreasuryGL() error=%v skip=%v", err, skip)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 || rec.calls[0].RefundChannel != "offset_invoice" || rec.calls[0].Amount != 500 {
		t.Fatalf("expected 1 offset_invoice call for 500, got %+v", rec.calls)
	}
}

// TestNetPayments_PartiallyPaidCreditSale_NetsARBeforeRealCash is the regression test for the
// "primary bug" still live after the 2026-08-06 fix above: a credit sale that has since had a
// REAL payment collected against it (a settle-credit/credit-settlement row, always newer than
// the sale-time on_account marker) had its reduction cut from the newest row first — the real
// cash row — because netPayments sorted purely by occurred_at with no AR/cash distinction. A
// reduction on a partially-paid on-account sale must shrink the STILL-OWED (on_account) portion
// first, leaving the real cash collected untouched, so the customer's actual AR debt moves and
// no phantom cash refund gets posted for money never physically returned.
func TestNetPayments_PartiallyPaidCreditSale_NetsARBeforeRealCash(t *testing.T) {
	svc, client := newTestReversalsService(t, nil)
	tenantID := uuid.New()
	ctx := context.Background()

	// 10,000 on-account sale; 4,000 collected later via a real credit-settlement payment
	// (occurred_at newer than the on_account marker — the exact live shape).
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(10000).SetTaxTotal(0).SetTotalAmount(10000).SetPaidTotal(4000).
		SetMetadata(map[string]any{"on_account": true}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	saleTime := time.Now().Add(-24 * time.Hour)
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(10000).SetStatus("completed").
		SetOccurredAt(saleTime).
		SetPaymentData(map[string]any{"method": "on_account"}).
		Save(ctx); err != nil {
		t.Fatalf("seed on-account marker: %v", err)
	}
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(4000).SetStatus("completed").
		SetOccurredAt(time.Now()). // collected AFTER the sale — newer than the marker above
		SetPaymentData(map[string]any{"method": "cash", "credit_settlement": true}).
		Save(ctx); err != nil {
		t.Fatalf("seed real cash settlement: %v", err)
	}

	// Admin reduces a line worth 1,400 (less than the 6,000 still outstanding).
	rev, err := client.POSReversal.Create().
		SetTenantID(tenantID).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-MIXED-1").SetScope("partial").SetStatus("pending").SetReason("edit sale reduce").
		SetRefundChannel("offset_invoice").SetLines([]entschema.ReversalLineJSON{}).
		SetAmount(1400).SetTaxAmount(0).SetCostAmount(0).
		SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed reversal: %v", err)
	}

	if err := svc.netPayments(ctx, client, rev, order, order.Metadata); err != nil {
		t.Fatalf("netPayments() error = %v", err)
	}

	// The real cash row must be UNTOUCHED — the customer really did pay 4,000 and that must
	// stay collected/reported exactly as-is.
	cashRow, err := client.POSPayment.Query().
		Where(entpospayment.OrderID(order.ID), entpospayment.Amount(4000)).Only(ctx)
	if err != nil {
		t.Fatalf("reload cash row: %v", err)
	}
	if _, touched := cashRow.PaymentData["reversal"]; touched {
		t.Errorf("the real 4,000 cash payment was netted — it must be untouched; the reduction should come off the still-owed AR portion instead")
	}
	updated, err := client.POSOrder.Get(ctx, order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if updated.PaidTotal != 4000 {
		t.Errorf("PaidTotal = %.2f, want unchanged 4000 (the real cash collected must not shrink from a reduction that should hit AR)", updated.PaidTotal)
	}

	// stepTreasuryGL must now see ZERO cash netted for this reversal (the on_account row, not
	// the cash row, was the one that shrank) and post the whole 1,400 via offset_invoice.
	rec, tc := newRecordingTreasuryServer(t)
	svc.treasuryClient = tc
	_, _, skip, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug")
	if err != nil || skip {
		t.Fatalf("stepTreasuryGL() error=%v skip=%v", err, skip)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 || rec.calls[0].RefundChannel != "offset_invoice" || rec.calls[0].Amount != 1400 {
		t.Fatalf("expected 1 offset_invoice call for 1400 (never cash — the real cash collected was untouched), got %+v", rec.calls)
	}
}

// TestResolveLines_ExclusiveTaxLine_AmountIncludesTaxOnTop is the regression test for a real bug:
// an EXCLUSIVE-tax line (price_includes_tax=false — tax is additive on top of total_price, see
// POSOrderLine's own schema doc) had its reversal Amount computed as total_price*ratio ALONE,
// omitting the tax portion entirely. Amount must equal the exact GROSS drop this reduction causes
// in order.TotalAmount (subtotal + tax_total, per orders.RecomputeTotalsWithClient), because
// netPayments cuts payment rows by rev.Amount and stepTreasuryGL posts it straight to treasury —
// understating it by the tax on the reduced quantity silently shorted every exclusive-tax
// tenant's partial reversal by its own VAT.
func TestResolveLines_ExclusiveTaxLine_AmountIncludesTaxOnTop(t *testing.T) {
	svc, client := newTestReversalsService(t, nil)
	tenantID := uuid.New()
	ctx := context.Background()

	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(1000).SetTaxTotal(160).SetTotalAmount(1160).SetPaidTotal(1160).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// 10 units @ 100 net each; 16% VAT ADDED ON TOP (price_includes_tax=false) — total_price is
	// the net 1000, tax_amount is the additive 160, matching how orders.CreateOrder persists an
	// exclusive-tax line (service.go: SetTotalPrice(lineTotal); tax_amount computed separately).
	line, err := client.POSOrderLine.Create().
		SetOrderID(order.ID).SetCatalogItemID(uuid.New()).SetSku("SKU-EXCL").SetName("Widget").
		SetQuantity(10).SetUnitPrice(100).SetTotalPrice(1000).
		SetTaxAmount(160).SetTaxRate(16).SetPriceIncludesTax(false).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed line: %v", err)
	}

	req := CreateRequest{Scope: "partial", Lines: []LineSelection{{LineID: line.ID, Quantity: 4}}}
	revLines, amount, tax, err := svc.resolveLines(order, []*ent.POSOrderLine{line}, req)
	if err != nil {
		t.Fatalf("resolveLines: %v", err)
	}
	if len(revLines) != 1 {
		t.Fatalf("expected 1 reversal line, got %d", len(revLines))
	}
	// ratio 4/10 = 0.4 -> net 400, tax 64 -> gross MUST be 464 (400 + 64), never bare 400.
	if revLines[0].Amount != 464 {
		t.Errorf("Amount = %.2f, want 464 (400 net + 64 tax added on top, an exclusive-tax line)", revLines[0].Amount)
	}
	if revLines[0].TaxAmount != 64 {
		t.Errorf("TaxAmount = %.2f, want 64", revLines[0].TaxAmount)
	}
	if amount != 464 || tax != 64 {
		t.Errorf("resolveLines totals = amount=%.2f tax=%.2f, want amount=464 tax=64", amount, tax)
	}
}

// TestResolveLines_InclusiveTaxLine_AmountUnchanged is the companion sanity check: an
// INCLUSIVE-tax line's total_price already carries its tax embedded (gross), so Amount must stay
// exactly total_price*ratio — this fix must NOT double-count tax for the (more common) inclusive
// case, only add it back for the exclusive one.
func TestResolveLines_InclusiveTaxLine_AmountUnchanged(t *testing.T) {
	svc, client := newTestReversalsService(t, nil)
	tenantID := uuid.New()
	ctx := context.Background()

	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(1160).SetTaxTotal(0).SetTotalAmount(1160).SetPaidTotal(1160).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// 10 units @ 116 gross each; 16% VAT EMBEDDED (price_includes_tax=true) — total_price (1160)
	// already carries the tax, tax_amount (160) is just the back-calculated embedded portion.
	line, err := client.POSOrderLine.Create().
		SetOrderID(order.ID).SetCatalogItemID(uuid.New()).SetSku("SKU-INCL").SetName("Widget").
		SetQuantity(10).SetUnitPrice(116).SetTotalPrice(1160).
		SetTaxAmount(160).SetTaxRate(16).SetPriceIncludesTax(true).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed line: %v", err)
	}

	req := CreateRequest{Scope: "partial", Lines: []LineSelection{{LineID: line.ID, Quantity: 4}}}
	revLines, amount, tax, err := svc.resolveLines(order, []*ent.POSOrderLine{line}, req)
	if err != nil {
		t.Fatalf("resolveLines: %v", err)
	}
	if len(revLines) != 1 {
		t.Fatalf("expected 1 reversal line, got %d", len(revLines))
	}
	// ratio 4/10 = 0.4 -> gross 464 (total_price already gross, no addition) — NOT 464+64=528.
	if revLines[0].Amount != 464 {
		t.Errorf("Amount = %.2f, want 464 (total_price already gross for an inclusive-tax line)", revLines[0].Amount)
	}
	if revLines[0].TaxAmount != 64 {
		t.Errorf("TaxAmount = %.2f, want 64 (embedded portion, reported but not added again)", revLines[0].TaxAmount)
	}
	if amount != 464 || tax != 64 {
		t.Errorf("resolveLines totals = amount=%.2f tax=%.2f, want amount=464 tax=64", amount, tax)
	}
}
