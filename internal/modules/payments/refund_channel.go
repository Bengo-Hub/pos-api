package payments

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/tender"
)

// RefundChannelForMethod is the treasury refund channel that sends money back the way it came
// in. Treasury first looks for the tenant's account that defaults to that channel's payment
// method, else books mpesa, mpesa_manual, mtn_momo_manual and airtel_money_manual against the
// mobile money float (1100), bank and cheque against the bank (1300), and cash, other_manual
// and card_manual against cash (1000).
// An unknown method refunds as cash, which is what every refund did before.
func RefundChannelForMethod(method string) string {
	switch canonicalTenderMethod(method) {
	case "mpesa_manual":
		// A sighted M-Pesa code stays a code refund: treasury resolves the refund's account
		// from the channel (a tenant's Till account may default to mpesa_manual and its Paybill
		// account to mpesa), so the refund leaves the account the sale went into.
		return "mpesa_manual"
	case "mpesa", "mpesa_stk", "mpesa_c2b":
		return "mpesa"
	case TenderMTNMoMoManual:
		return TenderMTNMoMoManual
	case TenderAirtelMoneyManual:
		return TenderAirtelMoneyManual
	case TenderOtherManual:
		return TenderOtherManual
	case "card_manual", "pdq", "card_terminal", "card":
		return "card_manual"
	case "room_charge":
		// Credited back to the room folio while the stay is open; see RefundChannelRoomFolio.
		return RefundChannelRoomFolio
	case "bank_transfer", "bank":
		return "bank"
	case "cheque":
		return "cheque"
	default:
		return "cash"
	}
}

// nonMoneyTenders never brought money in, so a refund never goes back through them.
var nonMoneyTenders = map[string]bool{
	TenderOnAccount:      true,
	TenderCustomerCredit: true,
	TenderExchangeCredit: true,
	TenderComplimentary:  true,
	"loyalty_points":     true,
	"loyalty":            true,
}

// IsNonMoneyTender reports whether a tender method never brought money in (on account, store
// credit, exchange credit, complimentary, loyalty), so a refund must never go back through it as
// money.
func IsNonMoneyTender(method string) bool {
	return nonMoneyTenders[canonicalTenderMethod(method)]
}

// IsStoreCreditTender reports whether a tender paid from the customer's store credit (customer
// credit or exchange credit). A refund of that part goes back to the customer as store credit.
func IsStoreCreditTender(method string) bool {
	switch canonicalTenderMethod(method) {
	case TenderCustomerCredit, TenderExchangeCredit:
		return true
	}
	return false
}

// OriginalRefundChannel is the refund channel for a sale's original payment method: the money
// tender that carried the largest completed amount (a split sale refunds the way most of the
// money came in). Credit, store credit, exchange credit, loyalty and complimentary tenders are
// skipped. It returns "" when the sale has no completed money payment or cannot be read, so the
// caller keeps its own default.
func OriginalRefundChannel(ctx context.Context, client *ent.Client, tenantID, orderID uuid.UUID) string {
	pays, err := client.POSPayment.Query().
		Where(
			pospayment.OrderID(orderID),
			pospayment.Status(StatusCompleted),
			pospayment.HasOrderWith(posorder.TenantID(tenantID)),
		).
		All(ctx)
	if err != nil || len(pays) == 0 {
		return ""
	}
	tenderType := map[uuid.UUID]string{}
	ids := make([]uuid.UUID, 0, len(pays))
	for _, p := range pays {
		ids = append(ids, p.TenderID)
	}
	if ts, terr := client.Tender.Query().Where(tender.IDIn(ids...)).All(ctx); terr == nil {
		for _, t := range ts {
			tenderType[t.ID] = t.Type
		}
	}
	totals := map[string]float64{}
	var order []string
	for _, p := range pays {
		method, _ := p.PaymentData["method"].(string)
		if strings.TrimSpace(method) == "" {
			method = tenderType[p.TenderID]
		}
		method = canonicalTenderMethod(method)
		if method == "" || nonMoneyTenders[method] {
			continue
		}
		ch := RefundChannelForMethod(method)
		if _, seen := totals[ch]; !seen {
			order = append(order, ch)
		}
		totals[ch] += p.Amount
	}
	best, bestAmt := "", 0.0
	for _, ch := range order {
		if totals[ch] > bestAmt+0.005 {
			best, bestAmt = ch, totals[ch]
		}
	}
	return best
}
