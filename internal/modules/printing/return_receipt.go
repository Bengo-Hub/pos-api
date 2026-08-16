package printing

import (
	"strings"

	"github.com/bengobox/pos-service/internal/ent"
)

// ReturnReceiptOpts carries what BuildReturnReceiptView cannot derive from the return rows alone
// (same caller-resolves-the-lookups split as ReceiptViewOpts).
type ReturnReceiptOpts struct {
	Outlet  *ent.Outlet
	Setting *ent.OutletSetting
	// TenantName is the resolved tenant/company name (tenant-vs-outlet header rule).
	TenantName string
	// OriginalOrderNumber is the number of the sale this return reverses — printed as
	// "Return against Order #…" by every layout.
	OriginalOrderNumber string
	// Currency is the original order's (or outlet's) currency; blank defaults to KES.
	Currency string
	// BillTo is the customer this refund goes back to (resolved from the original order).
	BillTo   string
	ServedBy string
	// ShowProviderFooter overrides the platform-owner advertisement default (nil = show).
	ShowProviderFooter *bool
}

// BuildReturnReceiptView assembles the canonical receipt snapshot for a REFUND/return document —
// the customer's proof that goods went back and money (or store credit) came out. It is the same
// Receipt struct rendered by the same four layouts as a sale; IsReturn is what makes those
// layouts print "REFUND TOTAL" instead of "TOTAL" and frame the document against the original
// order rather than as an invoice of its own.
//
// Exchanges deliberately need nothing special here: an exchange's replacement order is an ordinary
// fully-paid POSOrder, printable through the existing /orders/{id}/receipt endpoint. This builder
// simply renders whatever the POSReturn itself holds, which is the right document for the
// refund/store_credit types.
func BuildReturnReceiptView(ret *ent.POSReturn, lines []*ent.POSReturnLine, opts ReturnReceiptOpts) ReceiptView {
	items := make([]ReceiptLine, 0, len(lines))
	var subtotal float64
	for _, l := range lines {
		if l == nil {
			continue
		}
		items = append(items, ReceiptLine{
			SKU:        l.Sku,
			Name:       l.Name,
			Quantity:   l.Quantity,
			UnitPrice:  l.UnitPrice,
			TotalPrice: l.TotalPrice,
		})
		subtotal += l.TotalPrice
	}
	if subtotal == 0 {
		subtotal = ret.RefundAmount
	}

	// How the money went back: the chosen refund channel ("cash", "store_credit", …), falling
	// back to a plain "Refund" when the channel was never recorded (older/auto-completed rows).
	method := "Refund"
	if ret.RefundChannel != nil && strings.TrimSpace(string(*ret.RefundChannel)) != "" {
		method = string(*ret.RefundChannel)
	}

	issued := ret.CreatedAt
	v := ReceiptView{
		Type:          "customer",
		ReceiptNumber: ret.ReturnNumber,
		// A return carries no order number of its own — the sale it reverses is named through
		// OriginalOrderNumber, which the layouts print instead.
		OutletID:            ret.OutletID,
		IssuedAt:            issued,
		DisplayDate:         issued,
		BillTo:              strings.TrimSpace(opts.BillTo),
		BillToLabel:         "Customer",
		ServedBy:            opts.ServedBy,
		Lines:               items,
		Currency:            receiptCurrency(opts.Currency),
		Subtotal:            subtotal,
		TotalAmount:         ret.RefundAmount,
		AmountPaid:          ret.RefundAmount,
		PaymentMethod:       method,
		PaymentDate:         &issued,
		ShowLogo:            true,
		ShowProviderFooter:  true,
		IsReturn:            true,
		OriginalOrderNumber: opts.OriginalOrderNumber,
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
