package handlers

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
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
// restrictOnAccount is the tenant-configurable policy switch (default true, see
// creditSaleRefundRestricted) for the on-account guard below — reason-code store-credit
// blocking is NOT configurable (that one guards against a seller-fault return being parked as
// credit the customer is forced to spend, not a phantom-debt bug, so it always applies).
func validateRefundChannel(reasonCode *posreturn.ReasonCode, returnType posreturn.ReturnType, channel string, onAccount, restrictOnAccount bool) error {
	wantsStoreCredit := channel == "store_credit" || (channel == "" && returnType == posreturn.ReturnTypeStoreCredit)

	if wantsStoreCredit && reasonCode != nil && storeCreditBlockedReasons[*reasonCode] {
		return fmt.Errorf("store credit is not allowed for a %s return — refund the customer (cash/mpesa/bank/cheque), or offset their account for a credit sale", *reasonCode)
	}

	// An on-account (credit) sale was never paid — handing out cash would refund money the
	// business never received, and store credit is not a substitute either: it stacks a NEW
	// credit balance on top of the still-open debt instead of reducing it, leaving the customer
	// simultaneously owing money AND owed store credit for the same transaction (the exact
	// "phantom credit" bug this guard exists to prevent). By default the return must always
	// reduce what the customer owes (offset_invoice) — never store_credit/cash, regardless of
	// reason code. A tenant may opt out of this guard (OutletSetting.metadata
	// restrict_credit_sale_refund_to_offset=false) to allow any channel at their own discretion.
	if onAccount && restrictOnAccount && channel != "" && channel != "offset_invoice" {
		return fmt.Errorf("this sale was on account (unpaid) — settle the return by offsetting the customer's balance (offset_invoice), not %s", channel)
	}
	return nil
}

// creditSaleRefundRestricted reports whether this order's outlet still enforces the default
// on-account refund guard (see validateRefundChannel) or has opted, via OutletSetting.metadata
// "restrict_credit_sale_refund_to_offset", to let cashiers pick any refund channel for a
// credit-sale return. Defaults to true (today's behavior, unchanged) whenever the order/setting
// can't be resolved — a lookup failure must never silently loosen a financial guard.
func (h *ReturnHandler) creditSaleRefundRestricted(ctx context.Context, tenantID, orderID uuid.UUID) bool {
	order, err := h.client.POSOrder.Query().Where(entposorder.ID(orderID), entposorder.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return true
	}
	setting, err := h.client.OutletSetting.Query().Where(entoutletsetting.OutletID(order.OutletID)).Only(ctx)
	if err != nil {
		return true
	}
	return metaBoolDefault(setting.Metadata, "restrict_credit_sale_refund_to_offset", true)
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
