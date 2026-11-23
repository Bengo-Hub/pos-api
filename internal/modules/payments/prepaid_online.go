package payments

import (
	"strings"

	"github.com/Bengo-Hub/httpware/money"

	"github.com/bengobox/pos-service/internal/ent"
)

// onlinePrepaidCredit is the money the customer already paid online for this order (ordering took
// it and booked it in treasury). It counts against the bill at the till exactly like a completed
// POS payment, so a prepaid online order never shows its total as due again and the cashier cannot
// take the same money a second time. Only items added at the counter after the online payment are
// left to pay. Zero for till orders and for online orders not yet paid.
func onlinePrepaidCredit(order *ent.POSOrder) float64 {
	if order == nil || order.Metadata == nil {
		return 0
	}
	if id, _ := order.Metadata["online_order_id"].(string); id == "" {
		return 0
	}
	if prepaid, _ := order.Metadata["prepaid"].(bool); !prepaid {
		return 0
	}
	if total, _ := order.Metadata["online_grand_total"].(float64); total > 0 {
		return total
	}
	return order.TotalAmount
}

// onlineTenderPrefix marks the part of an online order's sale that the ordering service collected
// (paid online, or cash on delivery settled through ordering). Treasury books it against Customer
// Deposits, where ordering's payment put it, never as fresh cash. The rest of the name is the
// method ("online_mpesa"), so eTIMS still declares the real mode of payment.
const onlineTenderPrefix = "online_"

// onlineCollectedTender is the online order's money that no till payment covers: ordering took it,
// so the sale must draw it from Customer Deposits. Without it the sale looked like cash at the
// till and every paid online order was booked twice (2026-11-23).
func onlineCollectedTender(order *ent.POSOrder, tenders []saleTender) (saleTender, bool) {
	if order == nil || order.Metadata == nil {
		return saleTender{}, false
	}
	if id, _ := order.Metadata["online_order_id"].(string); id == "" {
		return saleTender{}, false
	}
	atTill := 0.0
	for _, t := range tenders {
		if t.Status == StatusCompleted {
			atTill += t.Amount
		}
	}
	rest := money.Round2(order.TotalAmount - atTill)
	if rest <= 0.009 {
		return saleTender{}, false
	}
	method := "mpesa"
	for _, k := range []string{"collected_payment_method", "payment_method"} {
		if v, _ := order.Metadata[k].(string); strings.TrimSpace(v) != "" {
			method = strings.ToLower(strings.TrimSpace(v))
			break
		}
	}
	return saleTender{Type: onlineTenderPrefix + method, Amount: rest, Status: StatusCompleted}, true
}
