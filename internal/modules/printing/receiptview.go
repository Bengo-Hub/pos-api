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

// receiptLineName appends the applied happy-hour deal label to the item name so the printed
// receipt shows the offer per line (e.g. "Long Island — Buy 1 Get 1 Free"). The label is
// stamped into line metadata at order creation by the orders service.
func receiptLineName(l *ent.POSOrderLine) string {
	if l == nil || l.Metadata == nil {
		return l.Name
	}
	hh, ok := l.Metadata["happy_hour"].(map[string]any)
	if !ok {
		return l.Name
	}
	if label, ok := hh["label"].(string); ok && strings.TrimSpace(label) != "" {
		return l.Name + " — " + label
	}
	return l.Name
}

// ReceiptLine is a single priced line on a receipt/ticket.
type ReceiptLine struct {
	SKU        string
	Name       string
	Quantity   float64
	UnitPrice  float64
	TotalPrice float64
	// When this line was added to the bill (nil for rows predating per-line timestamps). Shown on
	// the receipt for items added AFTER the order was opened, so a happy-hour deal that hinges on
	// the add-time (e.g. drinks rung up once the window opened on a tab opened earlier) is auditable.
	AddedAt *time.Time
}

// ReceiptPaymentMethods carries the "HOW TO PAY" payment-display info shown at the bottom of
// customer receipts, sourced from OutletSetting when ShowPaymentInfoOnReceipt is enabled.
type ReceiptPaymentMethods struct {
	MpesaPaybill      string
	MpesaAccountRef   string
	MpesaTill         string
	MpesaPochi        string
	AirtelMoneyNumber string
	MtnMomoNumber     string
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
		m.MpesaPochi != "" || m.AirtelMoneyNumber != "" || m.MtnMomoNumber != "" ||
		m.BankAccountNumber != "" || m.BankAccountName != ""
}

// resolveDisplayName picks the business name a receipt's header prints: tenantName by default
// (isHQ true, or the "receipt_show_tenant_name" metadata key absent/true), or outletName when
// isHQ is false AND that key is explicitly false (a tenant turning off the tenant-name default
// for one of its non-HQ branches — the BOI Enterprises multi-outlet case). Falls back to
// outletName whenever tenantName is empty (e.g. the tenant lookup failed), so a receipt header
// is never blank. Pure/DB-independent — the metadata read is the only OutletSetting-shaped input,
// passed in as a plain map so this is unit-testable without an ent client.
func resolveDisplayName(tenantName, outletName string, isHQ bool, metadata map[string]any) string {
	displayName := tenantName
	if !isHQ {
		if show, ok := metadata["receipt_show_tenant_name"].(bool); ok && !show {
			displayName = outletName
		}
	}
	if displayName == "" {
		displayName = outletName
	}
	return displayName
}

// effectiveOrderDate mirrors orders.Service's EffectiveOrderDate (modules/orders/service.go)
// byte-for-byte: the admin-set business_date backdate/move (see CreateOrder's backdate-at-entry
// field and MoveOrderDate) when present, else CreatedAt. Duplicated locally rather than imported
// because modules/orders already imports this printing package (service.go / print_enqueue.go),
// so the reverse import would be a cycle. Keep both copies in sync by hand.
func effectiveOrderDate(o *ent.POSOrder) time.Time {
	if o.BusinessDate != nil {
		return *o.BusinessDate
	}
	if o.OfflineCreatedAt != nil {
		return *o.OfflineCreatedAt
	}
	return o.CreatedAt
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
	// DisplayName is the resolved business name PRINTED AS THE RECEIPT HEADER — tenant name by
	// default, or this outlet's own name when the tenant has turned off "show tenant name" for a
	// non-HQ outlet (see ReceiptHandler.buildReceiptForOrder, the only place this is set; empty
	// here until that handler populates it). Deliberately separate from OutletName, which stays
	// the TRUE outlet name for KDS tickets/dispensing labels (orderdata.go) regardless of this
	// toggle — those must never show the tenant name.
	DisplayName   string
	OutletAddress string
	// OutletPhones is the formatted labeled-phone line from the outlet's contact_phones
	// (auth outlet metadata, mirrored into address_json) — printed as
	// "Mobile: AIRTEL +254754300099 · MTN +256782323113 · SAF +254112626692".
	OutletPhones string
	// OutletEmail is the branch's own contact_email (auth outlet metadata). Both this and
	// OutletPhones fall back to the tenant's general contact info in receipt.go when the branch
	// hasn't configured its own (see printing.Brand.Phone/Email).
	OutletEmail string
	Timezone    string // outlet IANA timezone, e.g. "Africa/Nairobi"
	// IssuedAt is the order's real, immutable creation timestamp (server ingestion time) — used
	// ONLY for internal same-order time math (e.g. receiptDataFromView's "added HH:MM" line
	// note), never rendered directly. What every layout prints as "Date:" is DisplayDate.
	IssuedAt time.Time
	// DisplayDate is the calendar day printed on the receipt/invoice/packing-slip as "Date:":
	// the admin backdate-at-entry / MoveOrderDate override (business_date) when present, else
	// IssuedAt. A backdated business_date is stored at tenant midnight (see
	// parseAndValidateEffectiveDate), so it naturally prints as "11-08-2026 00:00" rather than a
	// fabricated ring-up time — a plain, honest signal that this is a reporting day, not a live
	// timestamp. Matches orders.EffectiveOrderDate so a receipt never disagrees with the
	// All-Sales list or reports for the same order.
	DisplayDate time.Time
	BillTo      string
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
	// Charges is the named breakdown behind ChargesTotal (packaging/service/shipping…), from
	// order metadata — rendered as individual rows when present ("Shipping(+) 200").
	Charges       map[string]float64
	RoundOff      float64
	TotalAmount   float64
	AmountPaid    float64
	PaymentMethod string
	// PaymentDate is when the shown payment settled (POSPayment.CreatedAt) — retail receipts
	// print it next to the method, e.g. "Cash (14-07-2026)". Nil when unpaid/bill.
	PaymentDate *time.Time
	// BalanceDue = TotalAmount − AmountPaid: positive on a partly-paid/on-account sale,
	// negative when the customer holds credit/over-paid, zero on an exact settle.
	BalanceDue     float64
	AmountTendered float64
	ChangeDue      float64

	VatEnabled bool
	VatRate    float64

	ReceiptHeader string
	ReceiptFooter string
	PaperWidth    string

	// UseCase is the outlet's use case ("retail", "hospitality", …) — the HTML/PDF renderers
	// select the retail boxed-invoice template on it; empty falls back to the classic thermal look.
	UseCase string
	// ShowLogo: include the tenant/outlet logo on rendered receipts (Receipt & Printing setting;
	// defaults to true).
	ShowLogo bool

	// ProviderFooter is the platform-owner (Codevertex) advertisement printed at the very bottom
	// of customer receipts. Resolved by the handler (which can reach the tenant cache); when left
	// zero the renderers substitute DefaultProviderFooter() so the advertisement always prints.
	ProviderFooter ProviderFooter
	// ShowProviderFooter gates whether ProviderFooter renders at all — platform-level default
	// (ON) with an optional per-tenant override, resolved by the handler via
	// modules/providerfooter.Resolve (which can reach the DB). Defaults to true (unset means
	// "show") so any caller that doesn't explicitly resolve it keeps today's behavior.
	ShowProviderFooter bool

	// eTIMS fiscalisation ("KRA TIMS Details" on the printed receipt, mirroring paper ETR
	// receipts): KRA PIN prints in the business header; SCU ID + CU Inv No + signature + QR
	// print in the fiscal block. All empty when the sale wasn't (yet) fiscalised.
	EtimsInvoiceNumber string
	EtimsQRCodeURL     string
	EtimsKraPin        string // taxpayer KRA PIN — "KRA PIN: P0…" header line
	EtimsScuID         string // OSCU device serial — "SCU ID" line
	EtimsCuInvNo       string // "{SCU ID}/{receipt no}" — "CU Inv No." line
	EtimsRcptSign      string // KRA receipt signature (fiscal signing proof)
	EtimsInternalData  string // KRA control-unit internal data (intrlData) — printed dash-chunked
	PaymentMethods     *ReceiptPaymentMethods

	// CustomerAccountBalance is the customer's overall treasury AR position (distinct from
	// BalanceDue, which is scoped to THIS order) — set by the caller (GetReceipt), never here,
	// since BuildReceiptView has no treasury dependency. Nil when not resolved/applicable
	// (walk-in customer, treasury unreachable, or the balance is exactly zero).
	CustomerAccountBalance      *float64
	CustomerAccountBalanceLabel string // "Amount Owing" | "Store Credit Available"

	VoidReason string

	// IsReturn marks the document a REFUND/return rather than a sale: every layout swaps the
	// grand-total label to "REFUND TOTAL" and prints OriginalOrderNumber instead of framing the
	// document as an order/invoice. Set only by BuildReturnReceiptView.
	IsReturn bool
	// OriginalOrderNumber is the sale this return reverses (only meaningful when IsReturn).
	OriginalOrderNumber string
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
	// PaymentDate — when the payment shown on the receipt settled (nil for unpaid bills).
	PaymentDate *time.Time
	// TenantName is the tenant's resolved company name (caller looks this up via tenant
	// branding — BuildReceiptView has no tenant/branding access of its own). Drives
	// ReceiptView.DisplayName: shown by default on every outlet's receipts; a non-HQ outlet
	// with the "receipt_show_tenant_name" metadata toggle turned off shows its own outlet name
	// instead (see the outlet != nil block below). Empty TenantName (a caller that hasn't wired
	// branding through yet) falls back to the outlet's own name, matching the pre-existing
	// behaviour — never a regression, just not yet opted into the tenant-name default.
	TenantName string
	// SplitLineIDs, when non-nil, restricts the receipt to only these POSOrderLine ids (a
	// split-by-item guest bill) and zeroes order-level tax/discount/charges/round-off/payment
	// figures (a split's total is just the sum of its own line totals).
	SplitLineIDs map[string]bool
	SplitLabel   string // BillTo override for a split-by-item receipt, e.g. "Guest 1"
	// ShowProviderFooter, when non-nil, overrides the default (true — platform-owner footer
	// shown). Callers that can resolve modules/providerfooter.Resolve (they hold an *ent.Client
	// + tenant id) should set this explicitly; nil leaves the platform-wide default in effect.
	ShowProviderFooter *bool
	// ReceiptNumber, when set, overrides the default. Empty falls back to order.OrderNumber —
	// the receipt and the order it's printed from always carry the same number.
	ReceiptNumber string
}

// formatContactPhones renders the outlet's labeled phone list ([{label,value}, …] after a JSON
// round-trip) as one display line: "AIRTEL +254754300099 · MTN +256782323113". Unlabeled
// entries print just the number; junk entries are skipped. "" when there are none.
func formatContactPhones(raw any) string {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	parts := make([]string, 0, len(list))
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		value, _ := m["value"].(string)
		if strings.TrimSpace(value) == "" {
			continue
		}
		label, _ := m["label"].(string)
		if strings.TrimSpace(label) != "" {
			parts = append(parts, strings.TrimSpace(label)+" "+strings.TrimSpace(value))
		} else {
			parts = append(parts, strings.TrimSpace(value))
		}
	}
	return strings.Join(parts, " · ")
}

// chargesBreakdown reads the named additional-charge amounts (packaging/service/shipping…) the
// order-create flow stamps into order metadata under "charges". Values arrive as float64 after a
// JSON round-trip; anything non-positive or non-numeric is skipped. Nil when there are none.
func chargesBreakdown(meta map[string]any) map[string]float64 {
	raw, ok := meta["charges"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := map[string]float64{}
	for k, v := range raw {
		if f, ok := v.(float64); ok && f > 0 {
			out[k] = f
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FiscalBarcodeValue returns the value the printed Code 128 barcode encodes — the single
// decision point every renderer (ESC/POS, thermal HTML/PDF, A4 HTML/PDF) and the client
// (pos-ui) reads from, so a barcode can never diverge per surface:
//   - Fiscalised sale (eTIMS): the KRA CU Invoice Number (falls back to the legacy invoice
//     number) — mirrors the barcode printed on a genuine KRA ETR paper receipt (see the
//     Jazaribu Retail reference receipt, whose barcode HRI text is its own fiscal number,
//     never the internal till transaction id).
//   - Non-fiscalised retail sale: the POS order number, as a scan-for-lookup convenience.
//   - Everything else (non-fiscalised, non-retail): "" — no barcode printed.
func (v ReceiptView) FiscalBarcodeValue() string {
	if v.EtimsCuInvNo != "" {
		return v.EtimsCuInvNo
	}
	if v.EtimsInvoiceNumber != "" {
		return v.EtimsInvoiceNumber
	}
	if v.UseCase == "retail" {
		return v.OrderNumber
	}
	return ""
}

// DashChunk formats a KRA fiscal proof string (receipt signature) dash-separated every 4
// characters, matching the Technical Specification of TIS for OSCU/VSCU v2.0's worked examples
// ("V249-J39C-FJ48-HE2W", §6.23.7) — the spec-mandated PRINTED form of the signature. Shared by
// every receipt renderer (ESC/POS, thermal HTML/PDF, A4 HTML/PDF) so they can never disagree on
// the format.
func DashChunk(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
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

	// Receipt number is always the order's own number — uniform document numbering (see
	// documents.DocTypeOrder / handlers.ReceiptHandler.ensureReceiptNumber): the order and the
	// receipt printed for it are the same document, one number.
	receiptNumber := opts.ReceiptNumber
	if receiptNumber == "" {
		receiptNumber = order.OrderNumber
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
			items = append(items, ReceiptLine{SKU: l.Sku, Name: receiptLineName(l), Quantity: l.Quantity, UnitPrice: l.UnitPrice, TotalPrice: l.TotalPrice, AddedAt: l.CreatedAt})
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
			// A voided line (fully or partially) must print its remaining ACTIVE quantity, not
			// the original order quantity — the order total already nets voided_qty out (see
			// orders.Service's activeQty/activeFraction convention), so printing the raw
			// l.Quantity/l.TotalPrice here made a voided item still appear in full on the
			// receipt even though the customer was never charged for it. A fully voided line
			// (activeQty <= 0) is dropped entirely.
			activeQty := l.Quantity
			if l.VoidedQty != nil {
				activeQty -= *l.VoidedQty
			}
			if activeQty <= 0 {
				continue
			}
			items = append(items, ReceiptLine{SKU: l.Sku, Name: receiptLineName(l), Quantity: activeQty, UnitPrice: l.UnitPrice, TotalPrice: l.UnitPrice * activeQty, AddedAt: l.CreatedAt})
		}
	}

	// Balance still owed on the sale (negative = customer credit / over-payment). Keyed off the
	// order's maintained paid_total so a credit/on-account sale correctly shows the FULL amount
	// due on every print path (incl. background prints that pass no AmountPaid); falls back to
	// the caller-resolved payment amount for rows predating paid_total maintenance. A split
	// receipt zeroes payment figures, so its balance stays zero too.
	settled := order.PaidTotal
	if amountPaid > settled {
		settled = amountPaid
	}
	// on_account marker: credit sales stamp metadata.on_account=true at create (see the
	// pos-qa overdue-filter model) — payment status itself is derived, not a column.
	onAccount, _ := order.Metadata["on_account"].(bool)
	balanceDue := 0.0
	if opts.SplitLineIDs == nil && (settled > 0 || onAccount) {
		balanceDue = totalAmount - settled
	}

	v := ReceiptView{
		Type:               typ,
		ReceiptNumber:      receiptNumber,
		OrderNumber:        order.OrderNumber,
		OutletID:           order.OutletID,
		IssuedAt:           order.CreatedAt,
		DisplayDate:        effectiveOrderDate(order),
		BillTo:             billTo,
		BillToLabel:        billToLabel,
		ServedBy:           opts.ServedBy,
		TableRef:           tableRef,
		Lines:              items,
		Currency:           currency,
		Subtotal:           subtotal,
		TaxAmount:          taxAmount,
		DiscountAmount:     discountAmount,
		ChargesTotal:       chargesTotal,
		Charges:            chargesBreakdown(order.Metadata),
		RoundOff:           roundOff,
		TotalAmount:        totalAmount,
		AmountPaid:         amountPaid,
		PaymentMethod:      opts.PaymentMethod,
		PaymentDate:        opts.PaymentDate,
		BalanceDue:         balanceDue,
		AmountTendered:     amountTendered,
		ChangeDue:          changeDue,
		VoidReason:         opts.VoidReason,
		ShowLogo:           true,
		ShowProviderFooter: true,
	}
	if opts.ShowProviderFooter != nil {
		v.ShowProviderFooter = *opts.ShowProviderFooter
	}

	if order.EtimsInvoiceNumber != nil {
		v.EtimsInvoiceNumber = *order.EtimsInvoiceNumber
	}
	if order.EtimsQrCodeURL != nil {
		v.EtimsQRCodeURL = *order.EtimsQrCodeURL
	}
	if order.EtimsKraPin != nil {
		v.EtimsKraPin = *order.EtimsKraPin
	}
	if order.EtimsScuID != nil {
		v.EtimsScuID = *order.EtimsScuID
	}
	if order.EtimsCuInvNo != nil {
		v.EtimsCuInvNo = *order.EtimsCuInvNo
	}
	if order.EtimsRcptSign != nil {
		v.EtimsRcptSign = *order.EtimsRcptSign
	}
	if order.EtimsInternalData != nil {
		v.EtimsInternalData = *order.EtimsInternalData
	}
	// Business identity, receipt settings and the header de-duplication guards — shared with
	// every other document type built in this package (see receiptview_outlet.go).
	applyOutletContext(&v, outlet, setting, opts.TenantName)

	return v
}

// headerRepeatsOutletIdentity reports whether a custom receipt-header string just repeats the
// business/outlet identity already printed above it on every renderer (display name, tenant
// name, outlet name, then address) — or, on a non-HQ outlet hiding the tenant name, would leak
// it back in through the free-text header.
func headerRepeatsOutletIdentity(header, displayName, tenantName, outletName, outletAddress string) bool {
	h := strings.ToLower(header)
	if eq := strings.ToLower(strings.TrimSpace(outletAddress)); eq != "" && h == eq {
		return true
	}
	if name := strings.ToLower(strings.TrimSpace(displayName)); name != "" && strings.Contains(h, name) {
		return true
	}
	if name := strings.ToLower(strings.TrimSpace(tenantName)); name != "" && strings.Contains(h, name) {
		return true
	}
	if name := strings.ToLower(strings.TrimSpace(outletName)); name != "" && strings.Contains(h, name) {
		return true
	}
	return false
}
