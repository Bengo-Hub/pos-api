package orders

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	enttender "github.com/bengobox/pos-service/internal/ent/tender"
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
//
// completedReturns MUST already exclude any completed return whose refund_channel is
// offset_invoice/ar — those settle straight into treasury's CustomerBalance, which
// payments/ar_reconcile.go's ReconcileCustomerOrders (event-driven, reduce-only) folds into THIS
// order's own paid_total via a non-on_account "ar_reconciled" payment row. If a caller included
// those in completedReturns too, this function would double-subtract the same return once ar_reconcile
// catches up — confirmed live 2026-08-06 (order total 116, one 40 offset_invoice return: amount_due
// read 36 instead of the correct 76). Callers get this exclusion automatically via
// returnsRollupFor (handlers/orders.go) and payments.completedReturnsTotal — see their doc comments.
// A store_credit/cash-channel return on an on-account order never reaches treasury AR at all, so it
// is correctly still included and must still be netted here.

// Settlement is the derived owed-state of a single POS order.
type Settlement struct {
	Collected        float64 // = order.PaidTotal (money actually banked; on-account tender excluded)
	CompletedReturns float64 // sum of COMPLETED sell-returns against this sale (settled refunds/offsets)
	AmountDue        float64 // total − collected − completedReturns, clamped ≥0; 0 for non-committed
	PaymentStatus    string  // paid | partial | due | overdue | refunded | voided | cancelled | draft
}

// ComputeSettlement is THE owed-amount function. completedReturns is the settled-return total for
// this order (0 when none); callers batch-resolve it once via CompletedReturnsTotal semantics —
// see this file's own doc comment above for the offset_invoice/ar exclusion callers must apply.
func ComputeSettlement(o *ent.POSOrder, completedReturns float64) Settlement {
	collected := o.PaidTotal
	ps := DerivePaymentStatus(o.Status, o.TotalAmount, collected, completedReturns, IsOnAccount(o.Metadata))
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

// SettledOnAccount is THE authoritative "was this sale a credit sale" check — the DB-querying
// sibling of IsOnAccount, centralized here (rather than duplicated per-caller) after a live bug
// was found 2026-08-05: two independent copies (reversals.Service and handlers.ReturnHandler)
// each queried ONLY the Tender table for a completed payment whose tender_id resolves to a real
// Tender row of type "on_account" — which silently returns false for any tenant that has never
// bothered to configure a Tender catalog (confirmed live on boi-enterprises: GET /pos/tenders
// returns zero rows), since payments.recordCreditSale never requires a real tender_id. That gap
// let deleteNonFiscalized's ar_writeoff step skip ("not an on-account sale") and strand real AR
// debt for a sale that had just been hard-deleted. The primary signal here — order.Metadata via
// IsOnAccount — is stamped unconditionally by recordCreditSale regardless of Tender configuration;
// the Tender-row join is kept only as a fallback for legacy orders that predate the metadata
// stamp. Best-effort — false on any query error.
func SettledOnAccount(ctx context.Context, client *ent.Client, tenantID, orderID uuid.UUID) bool {
	if order, err := client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tenantID)).
		Only(ctx); err == nil && IsOnAccount(order.Metadata) {
		return true
	}

	pays, err := client.POSPayment.Query().
		Where(entpospayment.OrderID(orderID), entpospayment.Status("completed")).
		All(ctx)
	if err != nil || len(pays) == 0 {
		return false
	}
	ids := make([]uuid.UUID, 0, len(pays))
	for _, p := range pays {
		ids = append(ids, p.TenderID)
	}
	n, err := client.Tender.Query().
		Where(enttender.IDIn(ids...), enttender.TenantID(tenantID), enttender.TypeEQ("on_account")).
		Count(ctx)
	return err == nil && n > 0
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
//
// completedReturns is folded into "settled" alongside collected — a return-offset never touches
// paid_total (it settles directly against the customer's treasury AR), so without this a credit
// sale part-settled by a return and part by a real payment could reach AmountDue==0 (see
// ComputeSettlement, which nets the same completedReturns into `due`) while this function still
// read only `collected` and got stuck reporting "partial" forever.
func DerivePaymentStatus(status string, total, collected, completedReturns float64, onAccount bool) string {
	switch status {
	case "refunded", "voided", "cancelled", "draft":
		return status
	}
	settled := collected + completedReturns
	if total > 0 && settled+0.01 >= total {
		return "paid"
	}
	if onAccount {
		if settled > 0 {
			return "partial"
		}
		return "due"
	}
	if status == "completed" {
		return "paid"
	}
	if settled > 0 {
		return "partial"
	}
	return "due"
}
