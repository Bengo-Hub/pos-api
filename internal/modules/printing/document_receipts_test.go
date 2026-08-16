package printing

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// TestBuildLayawayReceiptViewDeposit covers the opening-deposit slip: no LayawayPayment row
// exists yet at Create, so the builder must source the amount/date off the plan itself.
func TestBuildLayawayReceiptViewDeposit(t *testing.T) {
	created := time.Date(2026, 8, 16, 9, 30, 0, 0, time.UTC)
	plan := &ent.LayawayPlan{
		ID:              uuid.MustParse("1f4c9ab2-0000-4000-8000-000000000001"),
		OutletID:        uuid.New(),
		CustomerName:    "Jane Wanjiru",
		TotalAmount:     decimal.NewFromInt(5000),
		DepositAmount:   decimal.NewFromInt(1500),
		PaidAmount:      decimal.NewFromInt(1500),
		RemainingAmount: decimal.NewFromInt(3500),
		CreatedAt:       created,
	}

	v := BuildLayawayReceiptView(plan, nil, LayawayReceiptOpts{TenantName: "BOI Enterprises", Currency: "KES"})

	if v.ReceiptNumber != "LAY-1f4c9ab2-001" {
		t.Errorf("ReceiptNumber = %q, want LAY-1f4c9ab2-001", v.ReceiptNumber)
	}
	if v.OrderNumber != "LAY-1f4c9ab2" {
		t.Errorf("OrderNumber = %q, want the plan reference LAY-1f4c9ab2", v.OrderNumber)
	}
	if v.BillTo != "Jane Wanjiru" {
		t.Errorf("BillTo = %q, want the plan's customer", v.BillTo)
	}
	if len(v.Lines) != 1 || v.Lines[0].Name != "Layaway deposit" || v.Lines[0].TotalPrice != 1500 {
		t.Errorf("Lines = %+v, want one 1500 'Layaway deposit' line", v.Lines)
	}
	if v.TotalAmount != 5000 {
		t.Errorf("TotalAmount = %v, want the plan total 5000", v.TotalAmount)
	}
	if v.AmountPaid != 1500 {
		t.Errorf("AmountPaid = %v, want this receipt's 1500", v.AmountPaid)
	}
	if v.BalanceDue != 3500 {
		t.Errorf("BalanceDue = %v, want the plan's remaining 3500", v.BalanceDue)
	}
	if v.CustomerAccountBalance == nil || *v.CustomerAccountBalance != 1500 || v.CustomerAccountBalanceLabel != "Paid to Date" {
		t.Errorf("paid-to-date row = %v/%q, want 1500/'Paid to Date'", v.CustomerAccountBalance, v.CustomerAccountBalanceLabel)
	}
	if v.PaymentMethod != "cash" {
		t.Errorf("PaymentMethod = %q, want the cash default", v.PaymentMethod)
	}
	if v.PaymentDate == nil || !v.PaymentDate.Equal(created) {
		t.Errorf("PaymentDate = %v, want the plan's created_at", v.PaymentDate)
	}
	if v.IsReturn {
		t.Error("a layaway slip must never be flagged as a return")
	}
	if v.DisplayName != "BOI Enterprises" {
		t.Errorf("DisplayName = %q, want the tenant name (shared applyOutletContext rule)", v.DisplayName)
	}
}

// TestBuildLayawayReceiptViewInstalment covers a recorded instalment: the amount, method and
// date all come off the LayawayPayment row, and the sequence drives number + label.
func TestBuildLayawayReceiptViewInstalment(t *testing.T) {
	paidAt := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	plan := &ent.LayawayPlan{
		ID:              uuid.MustParse("1f4c9ab2-0000-4000-8000-000000000001"),
		CustomerName:    "Jane Wanjiru",
		TotalAmount:     decimal.NewFromInt(5000),
		DepositAmount:   decimal.NewFromInt(1500),
		PaidAmount:      decimal.NewFromInt(2500),
		RemainingAmount: decimal.NewFromInt(2500),
		CreatedAt:       time.Date(2026, 8, 16, 9, 30, 0, 0, time.UTC),
	}
	payment := &ent.LayawayPayment{
		Amount:        decimal.NewFromInt(1000),
		PaymentMethod: "mpesa",
		PaidAt:        paidAt,
	}

	v := BuildLayawayReceiptView(plan, payment, LayawayReceiptOpts{Sequence: 2, Currency: ""})

	if v.ReceiptNumber != "LAY-1f4c9ab2-002" {
		t.Errorf("ReceiptNumber = %q, want LAY-1f4c9ab2-002", v.ReceiptNumber)
	}
	if len(v.Lines) != 1 || v.Lines[0].Name != "Layaway instalment #2" || v.Lines[0].TotalPrice != 1000 {
		t.Errorf("Lines = %+v, want one 1000 'Layaway instalment #2' line", v.Lines)
	}
	if v.AmountPaid != 1000 || v.Subtotal != 1000 {
		t.Errorf("AmountPaid/Subtotal = %v/%v, want this payment's 1000", v.AmountPaid, v.Subtotal)
	}
	if v.PaymentMethod != "mpesa" {
		t.Errorf("PaymentMethod = %q, want the payment's own method", v.PaymentMethod)
	}
	if v.PaymentDate == nil || !v.PaymentDate.Equal(paidAt) {
		t.Errorf("PaymentDate = %v, want the payment's paid_at", v.PaymentDate)
	}
	if v.Currency != "KES" {
		t.Errorf("Currency = %q, want the KES default for a blank currency", v.Currency)
	}
}

// TestBuildReturnReceiptView asserts the refund document is flagged as a return, carries the
// original order it reverses, and totals the returned lines.
func TestBuildReturnReceiptView(t *testing.T) {
	channel := posreturn.RefundChannelMpesa
	ret := &ent.POSReturn{
		ID:            uuid.New(),
		OutletID:      uuid.New(),
		OrderID:       uuid.New(),
		ReturnNumber:  "RET-000123",
		ReturnType:    posreturn.ReturnTypeRefund,
		RefundAmount:  1300,
		RefundChannel: &channel,
		CreatedAt:     time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC),
	}
	lines := []*ent.POSReturnLine{
		{Sku: "DWL750", Name: "Dish Washing Liquid 750ml", Quantity: 1, UnitPrice: 500, TotalPrice: 500},
		{Sku: "BW500", Name: "Body Wash 500ml", Quantity: 2, UnitPrice: 400, TotalPrice: 800},
	}

	v := BuildReturnReceiptView(ret, lines, ReturnReceiptOpts{
		TenantName:          "BOI Enterprises",
		OriginalOrderNumber: "POS-000450",
		Currency:            "KES",
		BillTo:              "Jane Wanjiru",
	})

	if !v.IsReturn {
		t.Error("IsReturn = false, want true — the renderers key their REFUND framing off it")
	}
	if v.OriginalOrderNumber != "POS-000450" {
		t.Errorf("OriginalOrderNumber = %q, want POS-000450", v.OriginalOrderNumber)
	}
	if v.ReceiptNumber != "RET-000123" {
		t.Errorf("ReceiptNumber = %q, want the return number", v.ReceiptNumber)
	}
	if v.OrderNumber != "" {
		t.Errorf("OrderNumber = %q, want empty — a return has no order number of its own", v.OrderNumber)
	}
	if len(v.Lines) != 2 || v.Subtotal != 1300 || v.TotalAmount != 1300 {
		t.Errorf("lines/subtotal/total = %d/%v/%v, want 2/1300/1300", len(v.Lines), v.Subtotal, v.TotalAmount)
	}
	if v.PaymentMethod != "mpesa" {
		t.Errorf("PaymentMethod = %q, want the refund channel", v.PaymentMethod)
	}
	if v.BillTo != "Jane Wanjiru" {
		t.Errorf("BillTo = %q, want the original sale's customer", v.BillTo)
	}
}

// A return with no recorded refund channel still prints a sensible method, and an unnamed
// customer still prints the walk-in fallback rather than a blank customer cell.
func TestBuildReturnReceiptViewFallbacks(t *testing.T) {
	ret := &ent.POSReturn{ReturnNumber: "RET-000124", RefundAmount: 250, CreatedAt: time.Now()}
	v := BuildReturnReceiptView(ret, nil, ReturnReceiptOpts{})
	if v.PaymentMethod != "Refund" {
		t.Errorf("PaymentMethod = %q, want the 'Refund' fallback", v.PaymentMethod)
	}
	if v.BillTo != "Walk-in customer" {
		t.Errorf("BillTo = %q, want the walk-in fallback", v.BillTo)
	}
	if v.Subtotal != 250 {
		t.Errorf("Subtotal = %v, want the refund amount when there are no line rows", v.Subtotal)
	}
}
