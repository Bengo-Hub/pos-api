package payments

import "github.com/bengobox/pos-service/internal/ent"

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
