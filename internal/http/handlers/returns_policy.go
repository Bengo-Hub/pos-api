package handlers

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/posreturn"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	enttender "github.com/bengobox/pos-service/internal/ent/tender"
)

// Refund-method policy: which settlement channels are valid for a return, given WHY the
// goods are coming back and HOW the original sale was settled. Enforced server-side at
// initiate, approve AND complete (each step may override the channel); pos-ui mirrors the
// matrix to guide the cashier, but this is the boundary.

// storeCreditBlockedReasons — goods faults are the SELLER's failure, so the customer must
// be made whole (money back, or their account reduced for an unpaid credit sale) — never
// parked as store credit they are forced to spend with the same seller.
var storeCreditBlockedReasons = map[posreturn.ReasonCode]bool{
	posreturn.ReasonCodeDefective: true,
	posreturn.ReasonCodeDamaged:   true,
	posreturn.ReasonCodeExpired:   true,
	posreturn.ReasonCodeWrongItem: true,
}

// validateRefundChannel rejects a (reason, return type, channel) combination that breaks
// the policy. onAccount marks the original sale as an unpaid credit sale ("on account").
// An empty channel is allowed here — it is defaulted at completion (see
// defaultRefundChannel) — EXCEPT that a store_credit return type is itself a channel choice.
func validateRefundChannel(reasonCode *posreturn.ReasonCode, returnType posreturn.ReturnType, channel string, onAccount bool) error {
	wantsStoreCredit := channel == "store_credit" || (channel == "" && returnType == posreturn.ReturnTypeStoreCredit)

	if wantsStoreCredit && reasonCode != nil && storeCreditBlockedReasons[*reasonCode] {
		return fmt.Errorf("store credit is not allowed for a %s return — refund the customer (cash/mpesa/bank/cheque), or offset their account for a credit sale", *reasonCode)
	}

	// An on-account (credit) sale was never paid — handing out cash would refund money the
	// business never received. The return must reduce what the customer owes instead.
	if onAccount && channel != "" && channel != "offset_invoice" && channel != "store_credit" {
		return fmt.Errorf("this sale was on account (unpaid) — settle the return by offsetting the customer's balance (offset_invoice), not by paying out %s", channel)
	}
	if onAccount && wantsStoreCredit && reasonCode != nil && storeCreditBlockedReasons[*reasonCode] {
		return fmt.Errorf("this %s return is against an unpaid credit sale — use offset_invoice so the customer's balance is reduced", *reasonCode)
	}
	return nil
}

// defaultRefundChannel picks the channel when none was chosen through the lifecycle:
// on-account sales offset the customer's AR balance; everything else defaults to cash.
func defaultRefundChannel(returnType posreturn.ReturnType, onAccount bool) string {
	if onAccount {
		return "offset_invoice"
	}
	if returnType == posreturn.ReturnTypeStoreCredit {
		return "store_credit"
	}
	return "cash"
}

// orderSettledOnAccount reports whether the original sale was settled on account (credit
// sale): any completed payment on an on_account tender. Best-effort — false on errors.
func (h *ReturnHandler) orderSettledOnAccount(ctx context.Context, tenantID, orderID uuid.UUID) bool {
	pays, err := h.client.POSPayment.Query().
		Where(entpospayment.OrderID(orderID), entpospayment.Status("completed")).
		All(ctx)
	if err != nil || len(pays) == 0 {
		return false
	}
	ids := make([]uuid.UUID, 0, len(pays))
	for _, p := range pays {
		ids = append(ids, p.TenderID)
	}
	n, err := h.client.Tender.Query().
		Where(enttender.IDIn(ids...), enttender.TenantID(tenantID), enttender.TypeEQ("on_account")).
		Count(ctx)
	return err == nil && n > 0
}
