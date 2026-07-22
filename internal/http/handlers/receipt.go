package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entbillsplit "github.com/bengobox/pos-service/internal/ent/billsplit"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
	entuser "github.com/bengobox/pos-service/internal/ent/user"
	"github.com/bengobox/pos-service/internal/modules/documents"
	"github.com/bengobox/pos-service/internal/modules/payments"
	"github.com/bengobox/pos-service/internal/modules/printing"
	"github.com/bengobox/pos-service/internal/modules/printing/layouts"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// ReceiptHandler handles receipt generation endpoints. All layout rendering lives in the
// central printing/layouts package — this handler only assembles the canonical view and
// dispatches to the outlet's resolved layout.
type ReceiptHandler struct {
	log      *zap.Logger
	client   *ent.Client
	cache    *sharedcache.Aside // tenant branding cache (auth-api source)
	authURL  string
	auditSvc *audit.Service
	// fiscalPin resolves the tenant's KRA PIN for the receipt header when (and only when)
	// the tenant has activated eTIMS on treasury. Wired to orders.TaxResolver.FiscalPin;
	// used as fallback when the order doesn't yet carry its own transmitted fiscal identity.
	fiscalPin func(ctx context.Context, tenantSlug string) string
	// treasury backfills missing fiscal identity on receipts (see receipt_etims_backfill.go) and
	// resolves the customer account-balance line (see ensureCustomerAccountBalance below).
	treasury *treasury.Client
	// resolveCrmContact resolves a phone's CRM contact id the SAME way a credit sale/return does
	// (payments.Service.ResolveCrmContactID), so the receipt balance line reads the identical
	// treasury CustomerBalance row those flows write to. Nil-safe: falls back to the raw phone.
	resolveCrmContact func(ctx context.Context, tenantID uuid.UUID, phone string) string
	// seq, when wired, mints receipt numbers through the tenant-configurable pos_receipt
	// document sequence (numeric by default). Minted once per order and persisted to
	// order.Metadata["receipt_number"] so reprints/other render surfaces stay stable.
	seq *documents.SequenceService
}

// NewReceiptHandler creates a new ReceiptHandler.
func NewReceiptHandler(log *zap.Logger, client *ent.Client, cache *sharedcache.Aside, authURL string) *ReceiptHandler {
	return &ReceiptHandler{log: log, client: client, cache: cache, authURL: authURL}
}

// WithSequence wires the document-sequence service so receipt numbers are minted through the
// tenant's pos_receipt sequence (numeric by default), falling back to the legacy RCT- form.
func (h *ReceiptHandler) WithSequence(seq *documents.SequenceService) *ReceiptHandler {
	h.seq = seq
	return h
}

// ensureReceiptNumber returns a stable receipt number for the order: the value already persisted
// on order.Metadata["receipt_number"] if present, otherwise a freshly minted pos_receipt sequence
// number (persisted for reprint stability). Returns "" when no sequence service is wired or it
// errors — callers then fall back to the legacy "RCT-"+OrderNumber form in BuildReceiptView.
func (h *ReceiptHandler) ensureReceiptNumber(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder) string {
	if order == nil {
		return ""
	}
	if rn, ok := order.Metadata["receipt_number"].(string); ok && rn != "" {
		return rn
	}
	if h.seq == nil {
		return ""
	}
	n, err := h.seq.GenerateNumber(ctx, tenantID, documents.DocTypePosReceipt)
	if err != nil || n == "" {
		return ""
	}
	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	meta["receipt_number"] = n
	if _, uerr := h.client.POSOrder.UpdateOneID(order.ID).SetMetadata(meta).Save(ctx); uerr != nil {
		h.log.Warn("persist receipt_number failed", zap.Error(uerr))
	} else {
		order.Metadata = meta
	}
	return n
}

// SetAuditService wires the centralized audit trail for receipt reprints.
func (h *ReceiptHandler) SetAuditService(a *audit.Service) { h.auditSvc = a }

// SetFiscalPinResolver wires the eTIMS-gated KRA PIN lookup for receipt headers.
func (h *ReceiptHandler) SetFiscalPinResolver(fn func(ctx context.Context, tenantSlug string) string) {
	h.fiscalPin = fn
}

// SetCrmContactResolver wires the CRM-contact lookup used to key the receipt's customer
// account-balance line — the same resolver credit sales/returns use.
func (h *ReceiptHandler) SetCrmContactResolver(fn func(ctx context.Context, tenantID uuid.UUID, phone string) string) {
	h.resolveCrmContact = fn
}

// ReprintReceipt handles POST /{tenantID}/pos/orders/{orderID}/receipt/reprint.
// Increments the order's reprint counter and records a receipt.reprint audit
// entry (duplicate receipts are a cash-skimming vector). Returns the new count.
func (h *ReceiptHandler) ReprintReceipt(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order id", http.StatusBadRequest)
		return
	}
	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	updated, err := order.Update().AddReprintCount(1).Save(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if h.auditSvc != nil {
		var actor uuid.UUID
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			actor, _ = uuid.Parse(claims.Subject)
		}
		oid := order.OutletID
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: actor,
			Action:      "receipt.reprint",
			EntityType:  "pos_order",
			EntityID:    orderID.String(),
			After:       map[string]any{"reprint_count": updated.ReprintCount, "order_number": order.OrderNumber},
		})
	}
	jsonOK(w, map[string]any{"reprint_count": updated.ReprintCount, "is_duplicate": updated.ReprintCount > 0})
}

// branding fetches tenant branding (logo/name/primary-color) from the shared cache, mirroring the
// documents module. Best-effort: returns a zero-value brand if anything is unavailable.
func (h *ReceiptHandler) branding(ctx context.Context, tenantID uuid.UUID) receiptBrand {
	var b receiptBrand
	if h.cache == nil || h.authURL == "" {
		return b
	}
	t, err := h.client.Tenant.Query().Where(enttenant.ID(tenantID)).Only(ctx)
	if err != nil {
		return b
	}
	b.CompanyName = t.Name
	td, err := sharedcache.GetTenantDetails(ctx, h.cache, h.authURL, t.Slug, sharedcache.DefaultTenantTTL)
	if err != nil {
		return b
	}
	tb := sharedcache.GetTenantBranding(td)
	if tb.Name != "" {
		b.CompanyName = tb.Name
	}
	b.LogoURL = tb.LogoURL
	b.PrimaryColor = tb.PrimaryColor
	return b
}

// payerNameFromPayment best-effort extracts the customer/payer name captured on an online payment
// (M-Pesa / card / Paystack), so a sale with no keyed-in customer still shows who paid. Treasury
// stamps the resolved payer name into POSPayment.payment_data on settlement (see the treasury
// payment-intent → POS payment sync); we read the common keys defensively. Returns "" when none.
func payerNameFromPayment(p *ent.POSPayment) string {
	if p == nil || p.PaymentData == nil {
		return ""
	}
	for _, k := range []string{"payer_name", "customer_name", "account_name", "sender_name", "name"} {
		if v, ok := p.PaymentData[k].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	// Some gateways split the name into first/last.
	first, _ := p.PaymentData["first_name"].(string)
	last, _ := p.PaymentData["last_name"].(string)
	if full := strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last)); full != "" {
		return full
	}
	return ""
}

// resolveServedBy returns the display name of the staff member who actually rang up the
// order — the order's own user_id — NOT whoever happens to be viewing/reprinting the
// receipt. POSOrder.user_id may be either the local pos user id or the auth-service user id,
// so both are matched. Falls back to the caller-supplied ?served_by= value (the legacy
// behaviour, which passed the current viewer) only when the order's user can't be resolved,
// so an S2S/online order with no staff still shows something sensible.
func (h *ReceiptHandler) resolveServedBy(ctx context.Context, tid, userID uuid.UUID, fallback string) string {
	if userID != uuid.Nil {
		if u, err := h.client.User.Query().
			Where(entuser.TenantID(tid), entuser.Or(entuser.ID(userID), entuser.AuthServiceUserID(userID))).
			First(ctx); err == nil && u != nil {
			if name := strings.TrimSpace(u.FullName); name != "" {
				return name
			}
			if email := strings.TrimSpace(u.Email); email != "" {
				return email
			}
		}
	}
	return strings.TrimSpace(fallback)
}

// newReceiptResponse maps the canonical printing.ReceiptView onto the JSON receipt payload — the
// single conversion point so the JSON API, the HTML endpoint, and the PDF endpoint always agree
// with the ESC/POS thermal receipt (which is built from the same ReceiptView).
func newReceiptResponse(v printing.ReceiptView, layout string) receiptResponse {
	lines := make([]layouts.Line, 0, len(v.Lines))
	for _, l := range v.Lines {
		lines = append(lines, layouts.Line{SKU: l.SKU, Name: l.Name, Quantity: l.Quantity, UnitPrice: l.UnitPrice, TotalPrice: l.TotalPrice})
	}
	var pm *layouts.PaymentMethods
	if v.PaymentMethods.HasAny() {
		pm = &layouts.PaymentMethods{
			MpesaPaybill:      v.PaymentMethods.MpesaPaybill,
			MpesaAccountRef:   v.PaymentMethods.MpesaAccountRef,
			MpesaTill:         v.PaymentMethods.MpesaTill,
			MpesaPochi:        v.PaymentMethods.MpesaPochi,
			BankName:          v.PaymentMethods.BankName,
			BankAccountNumber: v.PaymentMethods.BankAccountNumber,
			BankAccountName:   v.PaymentMethods.BankAccountName,
		}
	}
	// The printed barcode encodes the sale's fiscal identity once eTIMS-signed (matching a
	// genuine KRA ETR receipt's own barcode), else the POS order number for retail sales —
	// ReceiptView.FiscalBarcodeValue is the ONE place this decision is made (ESC/POS mirrors
	// it via receiptDataFromView). Empty string on encode failure — never fatal.
	barcodeValue := v.FiscalBarcodeValue()
	barcodePNG := ""
	if barcodeValue != "" {
		barcodePNG = printing.Code128DataURI(barcodeValue, 320, 56)
	}
	// Fiscalised sales embed the KRA verification QR as a real image (the stored URL is a
	// verification LINK — browsers can't render it via <img src>).
	etimsQRPNG := ""
	if v.EtimsQRCodeURL != "" {
		etimsQRPNG = printing.QRDataURI(v.EtimsQRCodeURL, 192)
	}
	return receiptResponse{
		ReceiptNumber:               v.ReceiptNumber,
		OrderNumber:                 v.OrderNumber,
		OutletID:                    v.OutletID,
		OutletName:                  v.OutletName,
		OutletAddress:               v.OutletAddress,
		OutletPhones:                v.OutletPhones,
		BillTo:                      v.BillTo,
		BillToLabel:                 v.BillToLabel,
		IssuedAt:                    v.IssuedAt,
		Timezone:                    v.Timezone,
		Lines:                       lines,
		Subtotal:                    v.Subtotal,
		TaxAmount:                   v.TaxAmount,
		DiscountAmount:              v.DiscountAmount,
		ChargesTotal:                v.ChargesTotal,
		Charges:                     v.Charges,
		RoundOff:                    v.RoundOff,
		TotalAmount:                 v.TotalAmount,
		Currency:                    v.Currency,
		AmountPaid:                  v.AmountPaid,
		PaymentMethod:               v.PaymentMethod,
		PaymentDate:                 v.PaymentDate,
		BalanceDue:                  v.BalanceDue,
		CustomerAccountBalance:      v.CustomerAccountBalance,
		CustomerAccountBalanceLabel: v.CustomerAccountBalanceLabel,
		AmountTendered:              v.AmountTendered,
		ChangeDue:                   v.ChangeDue,
		EtimsInvoiceNumber:          v.EtimsInvoiceNumber,
		EtimsQRCodeURL:              v.EtimsQRCodeURL,
		EtimsKraPin:                 v.EtimsKraPin,
		EtimsScuID:                  v.EtimsScuID,
		EtimsCuInvNo:                v.EtimsCuInvNo,
		EtimsRcptSign:               v.EtimsRcptSign,
		EtimsQRPNG:                  etimsQRPNG,
		PaymentMethods:              pm,
		ReceiptHeader:               v.ReceiptHeader,
		ReceiptFooter:               v.ReceiptFooter,
		VatEnabled:                  v.VatEnabled,
		VatRate:                     v.VatRate,
		PaperWidth:                  v.PaperWidth,
		ServedBy:                    v.ServedBy,
		UseCase:                     v.UseCase,
		Layout:                      layout,
		ShowLogo:                    v.ShowLogo,
		BarcodePNG:                  barcodePNG,
		BarcodeValue:                barcodeValue,
		ProviderFooterLead:          v.ProviderFooter.OrDefault().Lead,
		ProviderFooterContact:       v.ProviderFooter.OrDefault().Contact,
	}
}

// receiptFormatSetting reads the outlet's configured receipt_format (nil-safe; "" = auto).
func receiptFormatSetting(setting *ent.OutletSetting) string {
	if setting == nil {
		return ""
	}
	return string(setting.ReceiptFormat)
}

// GetReceipt handles GET /{tenantID}/pos/orders/{orderID}/receipt
// Query param ?format=pdf|html renders the receipt with the outlet's resolved layout
// (printing/layouts registry); default returns the JSON receipt data (incl. `layout`).
func (h *ReceiptHandler) GetReceipt(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderIDStr := chi.URLParam(r, "orderID")
	orderID, err := uuid.Parse(orderIDStr)
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		WithLines().
		WithPayments(func(q *ent.POSPaymentQuery) {
			// Deterministic: the LATEST completed payment, not whatever order the DB happens to
			// return — matters when a retry (e.g. an auth hiccup) left more than one completed
			// row on the same order.
			q.Where(pospayment.StatusEQ("completed")).Order(ent.Desc(pospayment.FieldOccurredAt)).Limit(1)
		}).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("get receipt: query order", zap.Error(err))
		jsonError(w, "failed to get order", http.StatusInternalServerError)
		return
	}

	// Optional split-by-item receipt: ?split_id=<billsplit> filters the receipt to only the
	// order lines assigned to that split, so each payer gets their own itemised bill.
	var splitLineSet map[string]bool
	var splitLabel string
	if splitParam := r.URL.Query().Get("split_id"); splitParam != "" {
		if splitID, perr := uuid.Parse(splitParam); perr == nil {
			if split, serr := h.client.BillSplit.Query().
				Where(entbillsplit.ID(splitID), entbillsplit.TenantID(tid), entbillsplit.OrderID(orderID)).
				Only(ctx); serr == nil && len(split.OrderLineIds) > 0 {
				splitLineSet = make(map[string]bool, len(split.OrderLineIds))
				for _, id := range split.OrderLineIds {
					splitLineSet[id] = true
				}
				splitLabel = split.SplitLabel
			}
		}
	}

	// Safety: if a split's stored line ids match NONE of the order's lines (e.g. a caller stored
	// catalog ids instead of order-line ids), fall back to the full order rather than a blank bill.
	if splitLineSet != nil {
		matched := false
		for _, l := range order.Edges.Lines {
			if splitLineSet[l.ID.String()] {
				matched = true
				break
			}
		}
		if !matched {
			splitLineSet = nil
			splitLabel = ""
		}
	}

	var amountPaid float64
	var payerName string
	var paymentDate *time.Time
	paymentMethod := "cash"
	if len(order.Edges.Payments) > 0 {
		p := order.Edges.Payments[0]
		amountPaid = p.Amount
		payerName = payerNameFromPayment(p)
		occurred := p.OccurredAt
		paymentDate = &occurred
		// PaymentData["method"] is stamped reliably by CreatePaymentIntent for EVERY tender
		// (cash/card_manual/mpesa_manual/...) — unlike the Tender catalog row, which the terminal
		// UI never actually resolves to a real per-method row (it sends a nil placeholder id), so
		// a Tender.Get lookup keyed by TenderID silently misses for ordinary sales and used to
		// leave paymentMethod stuck on the "cash" default regardless of the real tender. Trust the
		// stamped method first; only fall back to the Tender catalog row (then "cash") if it's
		// somehow missing (e.g. very old orders predating this field).
		if method, _ := p.PaymentData["method"].(string); strings.EqualFold(method, payments.TenderComplimentary) {
			paymentMethod = "COMPLIMENTARY — NOT CHARGED"
			if reason, _ := p.PaymentData["reason"].(string); reason != "" {
				paymentMethod = fmt.Sprintf("COMPLIMENTARY — NOT CHARGED (%s)", reason)
			}
		} else if strings.EqualFold(method, payments.TenderOnAccount) {
			// Credit sale: the on-account row marks a DEBT, not cash collected — the receipt
			// shows the method with nothing paid, and the balance-due line carries the amount
			// owed (BuildReceiptView reads the order's on_account marker).
			paymentMethod = "Credit (on account)"
			amountPaid = 0
		} else if method != "" {
			paymentMethod = exportMethodLabel(method)
		} else if t, terr := h.client.Tender.Get(ctx, p.TenderID); terr == nil {
			if t.Type != "" {
				paymentMethod = t.Type
			} else if t.Name != "" {
				paymentMethod = t.Name
			}
		}
	}
	changeDue := amountPaid - order.TotalAmount
	if changeDue < 0 {
		changeDue = 0
	}

	// Outlet + POS settings drive header/footer/VAT/paper-width/payment-display/layout on
	// every receipt surface — loaded once here and handed to the shared BuildReceiptView.
	outlet, _ := h.client.Outlet.Query().Where(entoutlet.ID(order.OutletID)).Only(ctx)
	setting, _ := h.client.OutletSetting.Query().Where(entoutletsetting.OutletID(order.OutletID)).Only(ctx)

	// Backfill missing eTIMS fiscal identity from treasury (pull path for a missed
	// transmission event) so the KRA TIMS block never silently disappears from a receipt.
	if outlet != nil {
		order = h.ensureEtimsFiscal(ctx, outlet.TenantSlug, order)
	}

	view := printing.BuildReceiptView(order, order.Edges.Lines, outlet, setting, printing.ReceiptViewOpts{
		Type:           "customer",
		ReceiptNumber:  h.ensureReceiptNumber(ctx, tid, order),
		PaymentMethod:  paymentMethod,
		AmountPaid:     amountPaid,
		AmountTendered: amountPaid,
		ChangeDue:      changeDue,
		ServedBy:       h.resolveServedBy(ctx, tid, order.UserID, r.URL.Query().Get("served_by")),
		PayerName:      payerName,
		PaymentDate:    paymentDate,
		SplitLineIDs:   splitLineSet,
		SplitLabel:     splitLabel,
	})
	// Platform-owner (Codevertex) advertisement footer — cache-first, static fallback.
	view.ProviderFooter = printing.ResolveProviderFooter(ctx, h.cache, h.authURL)
	// KRA PIN header line: the transmitted order carries its own fiscal identity; older/
	// not-yet-transmitted sales fall back to the tenant tax profile — but ONLY when the
	// tenant has activated eTIMS (the resolver returns "" otherwise).
	if view.EtimsKraPin == "" && h.fiscalPin != nil && outlet != nil {
		view.EtimsKraPin = h.fiscalPin(ctx, outlet.TenantSlug)
	}
	// Customer account-balance line: the customer's overall treasury AR position (store credit
	// available or amount owing), independent of whether THIS sale was cash or credit.
	if outlet != nil {
		h.ensureCustomerAccountBalance(ctx, outlet.TenantSlug, order, &view)
	}

	// Resolve the outlet's receipt layout: the receipt_format setting wins; "auto" (the
	// default) picks the best layout for the use case — thermal, the layout that prints
	// crisp on receipt printers. The A4 invoice sheet is strictly opt-in via settings.
	layout := layouts.Resolve(receiptFormatSetting(setting), view.UseCase)
	receipt := newReceiptResponse(view, layout)

	switch r.URL.Query().Get("format") {
	case "pdf":
		brand := h.branding(ctx, tid)
		pdfBytes, err := layouts.RenderPDF(receipt, brand)
		if err != nil {
			h.log.Error("generate receipt pdf", zap.Error(err))
			jsonError(w, "Failed to generate receipt PDF", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="receipt-%s.pdf"`, order.OrderNumber))
		_, _ = w.Write(pdfBytes)
	case "html":
		// Receipts print on thermal/non-colour printers, so we take only the tenant LOGO from
		// branding and deliberately ignore the brand primary colour (coloured text prints faint).
		brand := h.branding(ctx, tid)
		htmlOut := layouts.RenderHTML(receipt, brand.LogoURL)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="receipt-%s.html"`, order.OrderNumber))
		_, _ = w.Write(htmlOut)
	default:
		jsonOK(w, receipt)
	}
}

// GetReceiptHTML handles GET /{tenantID}/pos/orders/{orderID}/receipt/html
func (h *ReceiptHandler) GetReceiptHTML(w http.ResponseWriter, r *http.Request) {
	// Delegate to GetReceipt with format=html
	q := r.URL.Query()
	q.Set("format", "html")
	r.URL.RawQuery = q.Encode()
	h.GetReceipt(w, r)
}

// GetReceiptPDF handles GET /{tenantID}/pos/orders/{orderID}/receipt/pdf — a real PDF
// rendered with the outlet's resolved layout (printing/layouts).
func (h *ReceiptHandler) GetReceiptPDF(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	q.Set("format", "pdf")
	r.URL.RawQuery = q.Encode()
	h.GetReceipt(w, r)
}
