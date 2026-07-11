package printing

import (
	"context"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// ServedByFromContext resolves a "served by" display value for a receipt from the request's auth
// claims. Claims carry no display name (only email), so this is the same tier of identification
// already used as the pos-ui fallback (`user.fullName || user.email`) — good enough for a receipt
// line, and free of an extra cross-service user lookup in the order-create / payment-finalize hot
// path. Returns "" when the context carries no claims (e.g. a service-to-service call).
func ServedByFromContext(ctx context.Context) string {
	claims, ok := authclient.ClaimsFromContext(ctx)
	if !ok || claims == nil {
		return ""
	}
	return claims.Email
}

// ReceiptLine is a single priced line on a receipt/ticket.
type ReceiptLine struct {
	SKU        string
	Name       string
	Quantity   float64
	UnitPrice  float64
	TotalPrice float64
}

// ReceiptPaymentMethods carries the "HOW TO PAY" payment-display info shown at the bottom of
// customer receipts, sourced from OutletSetting when ShowPaymentInfoOnReceipt is enabled.
type ReceiptPaymentMethods struct {
	MpesaPaybill      string
	MpesaAccountRef   string
	MpesaTill         string
	MpesaPochi        string
	BankName          string
	BankAccountNumber string
	BankAccountName   string
}

// HasAny reports whether any payment-display field is set (nil-safe).
func (m *ReceiptPaymentMethods) HasAny() bool {
	if m == nil {
		return false
	}
	return m.MpesaPaybill != "" || m.MpesaAccountRef != "" || m.MpesaTill != "" ||
		m.MpesaPochi != "" || m.BankAccountNumber != "" || m.BankAccountName != ""
}

// ReceiptView is the single canonical snapshot of "what a receipt/bill says". Every printable
// surface — the JSON API, the server-rendered HTML/PDF, and the ESC/POS thermal bytes — renders
// FROM this, built by BuildReceiptView. Do not hand-populate a ReceiptView; a background-printed
// thermal receipt must always carry the same information as the one a cashier prints from the
// browser, and that only holds if there is exactly one place that assembles it.
type ReceiptView struct {
	Type          string // "customer" | "kitchen_ticket" | "waiter_copy" | "void"
	ReceiptNumber string
	OrderNumber   string
	OutletID      uuid.UUID
	OutletName    string
	OutletAddress string
	Timezone      string // outlet IANA timezone, e.g. "Africa/Nairobi"
	IssuedAt      time.Time
	BillTo        string
	// BillToLabel is "Customer" for a keyed-in/walk-in customer, or "Paid by" when the name was
	// resolved from an identified online payment (M-Pesa / card / Paystack payer).
	BillToLabel string
	ServedBy    string
	TableRef    string

	Lines []ReceiptLine

	Currency       string
	Subtotal       float64
	TaxAmount      float64
	DiscountAmount float64
	ChargesTotal   float64
	RoundOff       float64
	TotalAmount    float64
	AmountPaid     float64
	PaymentMethod  string
	AmountTendered float64
	ChangeDue      float64

	VatEnabled bool
	VatRate    float64

	ReceiptHeader string
	ReceiptFooter string
	PaperWidth    string

	// ProviderFooter is the platform-owner (Codevertex) advertisement printed at the very bottom
	// of customer receipts. Resolved by the handler (which can reach the tenant cache); when left
	// zero the renderers substitute DefaultProviderFooter() so the advertisement always prints.
	ProviderFooter ProviderFooter

	EtimsInvoiceNumber string
	EtimsQRCodeURL     string
	PaymentMethods     *ReceiptPaymentMethods

	VoidReason string
}

// ReceiptViewOpts carries the bits BuildReceiptView cannot derive from the order/outlet/setting
// alone — resolved by the caller (an HTTP handler or a background print-enqueue path), since
// payment-method/amount resolution differs slightly per call site (explicit query param, joined
// tender names, etc.) and pulling it in here would require an ent.Client dependency this function
// deliberately doesn't have.
type ReceiptViewOpts struct {
	Type           string // defaults to "customer" when empty
	PaymentMethod  string
	AmountPaid     float64
	AmountTendered float64
	ChangeDue      float64
	VoidReason     string
	// ServedBy — staff display name/email for the "Served by" line. Background/auto-print paths
	// resolve this from the acting request's auth claims; explicit reprints from the currently
	// logged-in viewer.
	ServedBy string
	// PayerName is the customer/payer name captured from an identified online payment
	// (M-Pesa / card / Paystack) when the order itself carries no customer name — resolved by the
	// caller from the payment record / treasury. Ignored for cash sales.
	PayerName string
	// SplitLineIDs, when non-nil, restricts the receipt to only these POSOrderLine ids (a
	// split-by-item guest bill) and zeroes order-level tax/discount/charges/round-off/payment
	// figures (a split's total is just the sum of its own line totals).
	SplitLineIDs map[string]bool
	SplitLabel   string // BillTo override for a split-by-item receipt, e.g. "Guest 1"
}

// IsCashMethod reports whether a payment method is an anonymous cash tender (no identified
// customer). Everything else — M-Pesa, card, mobile money, bank, online — identifies a payer.
func IsCashMethod(method string) bool {
	m := strings.ToLower(strings.TrimSpace(method))
	return m == "" || m == "cash" || m == "cash_on_delivery" || m == "cod"
}

// BuildReceiptView assembles the canonical receipt view for an order — the single builder shared
// by the JSON receipt API, the server-rendered HTML/PDF, and the ESC/POS thermal-byte builder.
// `outlet` and `setting` may be nil (best-effort: fields they'd populate stay zero-valued).
func BuildReceiptView(order *ent.POSOrder, lines []*ent.POSOrderLine, outlet *ent.Outlet, setting *ent.OutletSetting, opts ReceiptViewOpts) ReceiptView {
	typ := opts.Type
	if typ == "" {
		typ = "customer"
	}

	currency := order.Currency
	if currency == "" {
		currency = "KES"
	}

	tableRef := ""
	if v, ok := order.Metadata["table_number"].(string); ok && v != "" {
		tableRef = v
	} else if v, ok := order.Metadata["table_name"].(string); ok && v != "" {
		tableRef = v
	}

	// Customer / payer shown on the receipt, in priority order:
	//  1. A stored/keyed-in customer name always wins — the cashier or waiter can enter one at
	//     sale time (or it comes from a linked customer record), regardless of tender.
	//  2. Otherwise, for an IDENTIFIED (online) payment — M-Pesa / card / Paystack — use the payer
	//     name captured from the payment (opts.PayerName), else the order's phone. Labelled
	//     "Paid by" since it identifies who settled rather than a named account customer.
	//  3. Otherwise it's an anonymous walk-in.
	billTo := ""
	billToLabel := "Customer"
	if order.CustomerName != nil && strings.TrimSpace(*order.CustomerName) != "" {
		billTo = strings.TrimSpace(*order.CustomerName)
	} else if !IsCashMethod(opts.PaymentMethod) {
		if n := strings.TrimSpace(opts.PayerName); n != "" {
			billTo = n
			billToLabel = "Paid by"
		} else if order.CustomerPhone != nil && strings.TrimSpace(*order.CustomerPhone) != "" {
			billTo = strings.TrimSpace(*order.CustomerPhone)
			billToLabel = "Paid by"
		}
	}
	if billTo == "" {
		billTo = "Walk-in customer"
	}

	var items []ReceiptLine
	subtotal := order.Subtotal
	taxAmount := order.TaxTotal
	discountAmount := order.DiscountTotal
	chargesTotal := order.ChargesTotal
	roundOff := order.RoundOff
	totalAmount := order.TotalAmount
	amountPaid := opts.AmountPaid
	amountTendered := opts.AmountTendered
	changeDue := opts.ChangeDue

	if opts.SplitLineIDs != nil {
		var splitSubtotal float64
		for _, l := range lines {
			if !opts.SplitLineIDs[l.ID.String()] {
				continue
			}
			items = append(items, ReceiptLine{SKU: l.Sku, Name: l.Name, Quantity: l.Quantity, UnitPrice: l.UnitPrice, TotalPrice: l.TotalPrice})
			splitSubtotal += l.TotalPrice
		}
		// A split's items are VAT-inclusive line totals; order-level tax/discount/charges/
		// round-off/payment figures don't apply to an individual split.
		subtotal = splitSubtotal
		taxAmount, discountAmount, chargesTotal, roundOff = 0, 0, 0, 0
		totalAmount = splitSubtotal
		amountPaid, amountTendered, changeDue = 0, 0, 0
		if opts.SplitLabel != "" {
			billTo = opts.SplitLabel
		}
	} else {
		for _, l := range lines {
			items = append(items, ReceiptLine{SKU: l.Sku, Name: l.Name, Quantity: l.Quantity, UnitPrice: l.UnitPrice, TotalPrice: l.TotalPrice})
		}
	}

	v := ReceiptView{
		Type:           typ,
		ReceiptNumber:  "RCT-" + order.OrderNumber,
		OrderNumber:    order.OrderNumber,
		OutletID:       order.OutletID,
		IssuedAt:       order.CreatedAt,
		BillTo:         billTo,
		BillToLabel:    billToLabel,
		ServedBy:       opts.ServedBy,
		TableRef:       tableRef,
		Lines:          items,
		Currency:       currency,
		Subtotal:       subtotal,
		TaxAmount:      taxAmount,
		DiscountAmount: discountAmount,
		ChargesTotal:   chargesTotal,
		RoundOff:       roundOff,
		TotalAmount:    totalAmount,
		AmountPaid:     amountPaid,
		PaymentMethod:  opts.PaymentMethod,
		AmountTendered: amountTendered,
		ChangeDue:      changeDue,
		VoidReason:     opts.VoidReason,
	}

	if order.EtimsInvoiceNumber != nil {
		v.EtimsInvoiceNumber = *order.EtimsInvoiceNumber
	}
	if order.EtimsQrCodeURL != nil {
		v.EtimsQRCodeURL = *order.EtimsQrCodeURL
	}

	if outlet != nil {
		v.OutletName = outlet.Name
		v.Timezone = outlet.Timezone
		if addr := outlet.AddressJSON; addr != nil {
			if street, ok := addr["street"].(string); ok && street != "" {
				v.OutletAddress = street
			} else if city, ok := addr["city"].(string); ok {
				v.OutletAddress = city
			}
		}
	}

	// De-duplicate: when the outlet's address was set to the same text as its name (a common
	// mis-configuration — see "Urban Loft Cafe Busia" printed twice), drop the address so the
	// receipt shows each piece of information exactly once.
	if strings.EqualFold(strings.TrimSpace(v.OutletAddress), strings.TrimSpace(v.OutletName)) {
		v.OutletAddress = ""
	}

	if setting != nil {
		if setting.ReceiptHeader != nil {
			v.ReceiptHeader = *setting.ReceiptHeader
		}
		if setting.ReceiptFooter != nil {
			v.ReceiptFooter = *setting.ReceiptFooter
		}
		v.VatEnabled = setting.VatEnabled
		v.VatRate = setting.VatRate
		v.PaperWidth = setting.PaperWidth

		if setting.ShowPaymentInfoOnReceipt {
			pm := &ReceiptPaymentMethods{}
			if setting.MpesaPaybill != nil {
				pm.MpesaPaybill = *setting.MpesaPaybill
			}
			if setting.MpesaAccountReference != nil {
				pm.MpesaAccountRef = *setting.MpesaAccountReference
			}
			if setting.MpesaTill != nil {
				pm.MpesaTill = *setting.MpesaTill
			}
			if setting.MpesaPochi != nil {
				pm.MpesaPochi = *setting.MpesaPochi
			}
			if setting.BankName != nil {
				pm.BankName = *setting.BankName
			}
			if setting.BankAccountNumber != nil {
				pm.BankAccountNumber = *setting.BankAccountNumber
			}
			if setting.BankAccountName != nil {
				pm.BankAccountName = *setting.BankAccountName
			}
			if pm.HasAny() {
				v.PaymentMethods = pm
			}
		}
	}

	return v
}
