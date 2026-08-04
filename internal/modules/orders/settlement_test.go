package orders

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// ── pure-Go sqlite shim (mirrors saledelete/service_test.go — ent needs a driver registered as
// "sqlite3"; modernc.org/sqlite registers as "sqlite"). Duplicated per-package since Go test
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

// TestDerivePaymentStatus_CreditSaleSettledByReturnPlusPayment reproduces the reported bug: a
// credit (on-account) sale part-settled by a completed return-offset (never touches paid_total)
// and part-settled by a real payment must read "paid" once the two together cover the total —
// not stay stuck on "partial" forever.
func TestDerivePaymentStatus_CreditSaleSettledByReturnPlusPayment(t *testing.T) {
	got := DerivePaymentStatus("completed", 1000, 600, 400, true)
	if got != "paid" {
		t.Errorf("DerivePaymentStatus(completed, total=1000, collected=600, completedReturns=400, onAccount=true) = %q, want %q", got, "paid")
	}
}

func TestDerivePaymentStatus_CreditSalePartialWithoutReturn(t *testing.T) {
	got := DerivePaymentStatus("completed", 1000, 400, 0, true)
	if got != "partial" {
		t.Errorf("DerivePaymentStatus(collected=400, completedReturns=0, onAccount=true) = %q, want %q", got, "partial")
	}
}

func TestDerivePaymentStatus_CreditSaleDueWithoutAnySettlement(t *testing.T) {
	got := DerivePaymentStatus("completed", 1000, 0, 0, true)
	if got != "due" {
		t.Errorf("DerivePaymentStatus(collected=0, completedReturns=0, onAccount=true) = %q, want %q", got, "due")
	}
}

func TestDerivePaymentStatus_CashSaleUnaffectedByReturns(t *testing.T) {
	// Non-on-account completed orders always read "paid" regardless of collected/returns — the
	// existing masking behavior (status=="completed") must be unchanged.
	got := DerivePaymentStatus("completed", 1000, 0, 0, false)
	if got != "paid" {
		t.Errorf("DerivePaymentStatus(cash, completed) = %q, want %q", got, "paid")
	}
}

func TestComputeSettlement_CreditSaleFullySettledByReturnPlusPayment(t *testing.T) {
	// Mirrors the reported scenario end-to-end via ComputeSettlement (AmountDue and
	// PaymentStatus must agree): total 1000, a completed return offsets 400, a payment
	// collects the remaining 600.
	o := &ent.POSOrder{
		Status:      "completed",
		TotalAmount: 1000,
		PaidTotal:   600,
		Metadata:    map[string]any{"on_account": true},
	}
	st := ComputeSettlement(o, 400)
	if st.PaymentStatus != "paid" {
		t.Errorf("PaymentStatus = %q, want %q", st.PaymentStatus, "paid")
	}
	if st.AmountDue != 0 {
		t.Errorf("AmountDue = %v, want 0", st.AmountDue)
	}
}

// TestSettledOnAccount_MetadataTrue_NoTenderRow is the regression test for a live bug found
// 2026-08-05: a tenant with zero configured Tender rows (confirmed live on boi-enterprises — GET
// /pos/tenders returns an empty list) still runs credit sales fine via payments.recordCreditSale,
// which stamps order.Metadata["on_account"]=true but never requires a real tender_id on the
// payment row. The old detection (Tender-table join only) always returned false here. This test
// seeds exactly that shape — a payment with a random, never-created tender_id, no Tender row
// anywhere — and requires SettledOnAccount to still return true off the metadata alone.
func TestSettledOnAccount_MetadataTrue_NoTenderRow(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:orderssettletest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	tid := uuid.New()

	order, err := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(uuid.New()).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus("completed").
		SetSubtotal(200).
		SetTaxTotal(0).
		SetTotalAmount(200).
		SetPaidTotal(0).
		SetMetadata(map[string]any{"on_account": true}).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(200).SetStatus("completed").
		Save(context.Background()); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	if !SettledOnAccount(context.Background(), client, tid, order.ID) {
		t.Fatal("expected SettledOnAccount to return true from order metadata alone, with no configured Tender row")
	}
}

// TestSettledOnAccount_CashSale confirms a plain cash sale (no on_account metadata, no on_account
// Tender row) correctly reads as NOT settled on account.
func TestSettledOnAccount_CashSale(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:orderssettletest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	tid := uuid.New()

	order, err := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(uuid.New()).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus("completed").
		SetSubtotal(200).
		SetTaxTotal(0).
		SetTotalAmount(200).
		SetPaidTotal(200).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(200).SetStatus("completed").
		Save(context.Background()); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	if SettledOnAccount(context.Background(), client, tid, order.ID) {
		t.Fatal("expected SettledOnAccount to return false for a plain cash sale")
	}
}
