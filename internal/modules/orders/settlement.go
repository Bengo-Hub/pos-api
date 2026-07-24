package orders

import (
	"time"

	"github.com/bengobox/pos-service/internal/ent"
)

// This file is the SINGLE authoritative definition of "how much a POS order still owes" and the
// payment status derived from it. Every read path — the All-Sales list + summary, the Sell Details
// modal endpoint, the CSV/register/P&L exports, and the treasury→POS AR reconciler — MUST derive
// its owed figure from ComputeSettlement here, so no two surfaces can ever disagree about the same
// sale again (the class of bug behind "list says 4,000, Sell Details says 8,000, treasury says 0").
//
// The money model it encodes:
//   - paid_total counts only money ACTUALLY COLLECTED (on-account/credit tender rows are excluded
//     upstream in RecomputePaidTotal — a credit sale is a treasury AR debt, not cash banked).
//   - a COMPLETED sell-return reduces what is still owed on the sale but never touches the order's
//     own paid_total (the refund/offset moves the customer's treasury AR directly), so the netting
//     has to happen here, at read time.
//   - voided / cancelled / draft orders owe nothing (no committed financial effect).

// Settlement is the derived owed-state of a single POS order.
type Settlement struct {
	Collected        float64 // = order.PaidTotal (money actually banked; on-account tender excluded)
	CompletedReturns float64 // sum of COMPLETED sell-returns against this sale (settled refunds/offsets)
	AmountDue        float64 // total − collected − completedReturns, clamped ≥0; 0 for non-committed
	PaymentStatus    string  // paid | partial | due | overdue | refunded | voided | cancelled | draft
}

// ComputeSettlement is THE owed-amount function. completedReturns is the settled-return total for
// this order (0 when none); callers batch-resolve it once via CompletedReturnsTotal semantics.
func ComputeSettlement(o *ent.POSOrder, completedReturns float64) Settlement {
	collected := o.PaidTotal
	ps := DerivePaymentStatus(o.Status, o.TotalAmount, collected, IsOnAccount(o.Metadata))
	if (ps == "due" || ps == "partial") && IsOrderOverdue(o.Metadata) {
		ps = "overdue"
	}
	var due float64
	if !NonCommittedStatus(ps) {
		due = o.TotalAmount - collected - completedReturns
		if due < 0 {
			due = 0
		}
	}
	return Settlement{
		Collected:        collected,
		CompletedReturns: completedReturns,
		AmountDue:        due,
		PaymentStatus:    ps,
	}
}

// NonCommittedStatus reports whether a payment-status label represents an order with NO real
// financial effect — voided/cancelled were reversed, draft was never finalized. These are excluded
// from headline Total/Paid/Due/Items sums and always owe 0.
func NonCommittedStatus(ps string) bool {
	switch ps {
	case "voided", "cancelled", "draft":
		return true
	}
	return false
}

// IsOnAccount reports whether the order was closed on account (credit sale) — its money is a
// treasury AR debt, so it reads due/partial/overdue until collected, even once the order completes.
func IsOnAccount(meta map[string]any) bool {
	v, ok := meta["on_account"].(bool)
	return ok && v
}

// IsOrderOverdue reports whether an order is past its stamped metadata.payment_due_date (RFC3339).
func IsOrderOverdue(meta map[string]any) bool {
	raw, ok := meta["payment_due_date"].(string)
	if !ok || raw == "" {
		return false
	}
	due, err := time.Parse(time.RFC3339, raw)
	return err == nil && due.Before(time.Now())
}

// DerivePaymentStatus maps an order's status + collected amount to a display payment status.
// onAccount marks a credit sale: completion means the goods left, NOT that cash was banked —
// paid_total excludes the on-account tender, so the sale reads due/partial (and "overdue" past its
// due date, upgraded above) until the money is actually collected.
func DerivePaymentStatus(status string, total, collected float64, onAccount bool) string {
	switch status {
	case "refunded", "voided", "cancelled", "draft":
		return status
	}
	if total > 0 && collected+0.01 >= total {
		return "paid"
	}
	if onAccount {
		if collected > 0 {
			return "partial"
		}
		return "due"
	}
	if status == "completed" {
		return "paid"
	}
	if collected > 0 {
		return "partial"
	}
	return "due"
}
