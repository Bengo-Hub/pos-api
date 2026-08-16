package printing

import (
	"fmt"
	"strings"

	"github.com/bengobox/pos-service/internal/ent"
)

// LayawayReceiptOpts carries what BuildLayawayReceiptView cannot derive from the plan/payment
// rows alone — the same split of responsibilities ReceiptViewOpts uses for a sale receipt: the
// caller (an HTTP handler) owns every DB/cache lookup, this builder stays pure.
type LayawayReceiptOpts struct {
	// Outlet/Setting drive the printed business identity, VAT/paper settings and the
	// header/footer — resolved through the SAME applyOutletContext a sale receipt uses, so a
	// layaway slip and a sale receipt print an identical business header.
	Outlet  *ent.Outlet
	Setting *ent.OutletSetting
	// TenantName is the resolved tenant/company name (tenant-vs-outlet header rule).
	TenantName string
	// Currency is the outlet's currency ("KES" when unresolved) — a layaway plan stores no
	// currency of its own.
	Currency string
	ServedBy string
	// Sequence is the 1-based ordinal of this payment within the plan (1 = the opening deposit),
	// used for the printed receipt number and the "instalment #N" line label.
	Sequence int
	// ShowProviderFooter overrides the platform-owner advertisement default (nil = show).
	ShowProviderFooter *bool
}

// BuildLayawayReceiptView assembles the canonical receipt snapshot for money collected against a
// LAYAWAY PLAN — the deposit taken at create time and every instalment recorded afterwards. Those
// happen long before any POSOrder exists (one is only created at Complete), so the sale-receipt
// path (BuildReceiptView, keyed on an order) cannot serve them; this is the same struct rendered
// by the same four layouts, just sourced from the plan instead.
//
// payment may be nil, which builds the OPENING DEPOSIT receipt straight off the plan (Create
// records the deposit on the plan itself and writes no LayawayPayment row).
//
// Money fields map as: the single line + Subtotal = what was collected on THIS receipt;
// TotalAmount = the plan's full price (the "TOTAL" the customer committed to); AmountPaid = this
// payment; BalanceDue = what is still owed; and the spare labeled totals row carries "Paid to
// Date" so the plan arithmetic (plan total − paid to date = balance) is readable on the slip.
func BuildLayawayReceiptView(plan *ent.LayawayPlan, payment *ent.LayawayPayment, opts LayawayReceiptOpts) ReceiptView {
	seq := opts.Sequence
	if seq < 1 {
		seq = 1
	}

	amount := plan.DepositAmount.InexactFloat64()
	method := "cash"
	paidAt := plan.CreatedAt
	label := "Layaway deposit"
	if payment != nil {
		amount = payment.Amount.InexactFloat64()
		if m := strings.TrimSpace(payment.PaymentMethod); m != "" {
			method = m
		}
		paidAt = payment.PaidAt
		if seq > 1 {
			label = fmt.Sprintf("Layaway instalment #%d", seq)
		}
	}

	planRef := layawayRef(plan)
	settled := paidAt

	paidToDate := plan.PaidAmount.InexactFloat64()
	v := ReceiptView{
		Type:          "customer",
		ReceiptNumber: fmt.Sprintf("%s-%03d", planRef, seq),
		// A layaway has no order number until it is completed; the plan reference keeps the
		// document-number cell on every layout populated and traceable back to the plan.
		OrderNumber: planRef,
		OutletID:    plan.OutletID,
		IssuedAt:    settled,
		DisplayDate: settled,
		BillTo:      strings.TrimSpace(plan.CustomerName),
		BillToLabel: "Customer",
		ServedBy:    opts.ServedBy,
		Lines: []ReceiptLine{{
			Name:       label,
			Quantity:   1,
			UnitPrice:  amount,
			TotalPrice: amount,
		}},
		Currency:           receiptCurrency(opts.Currency),
		Subtotal:           amount,
		TotalAmount:        plan.TotalAmount.InexactFloat64(),
		AmountPaid:         amount,
		PaymentMethod:      method,
		PaymentDate:        &settled,
		BalanceDue:         plan.RemainingAmount.InexactFloat64(),
		ShowLogo:           true,
		ShowProviderFooter: true,
		// Spare labeled totals row (rendered by all four layouts) — the running plan position,
		// so "plan total − paid to date = balance due" reconciles on the printed slip.
		CustomerAccountBalance:      &paidToDate,
		CustomerAccountBalanceLabel: "Paid to Date",
	}
	if v.BillTo == "" {
		v.BillTo = "Walk-in customer"
	}
	if opts.ShowProviderFooter != nil {
		v.ShowProviderFooter = *opts.ShowProviderFooter
	}
	applyOutletContext(&v, opts.Outlet, opts.Setting, opts.TenantName)
	return v
}

// layawayRef is the short, human-quotable plan reference printed on every layaway slip
// ("LAY-1f4c9ab2"). Matches the LAY- prefix Complete already uses for the generated order number.
func layawayRef(plan *ent.LayawayPlan) string {
	id := plan.ID.String()
	if len(id) > 8 {
		id = id[:8]
	}
	return "LAY-" + id
}

// receiptCurrency defaults a blank currency to KES, mirroring BuildReceiptView.
func receiptCurrency(c string) string {
	if strings.TrimSpace(c) == "" {
		return "KES"
	}
	return c
}
