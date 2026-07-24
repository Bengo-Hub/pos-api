// Package payments provides the payment service layer for POS operations.
package payments

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcommissionrule "github.com/bengobox/pos-service/internal/ent/commissionrule"
	entloyalty "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	outletsettingpredicate "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entcatalogoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	staffmemberent "github.com/bengobox/pos-service/internal/ent/staffmember"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tableassignment"
	"github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/notifications"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/printing"
	"github.com/bengobox/pos-service/internal/modules/staffcredit"
	"github.com/bengobox/pos-service/internal/modules/treasury"
	"github.com/bengobox/pos-service/internal/payref"
	"github.com/bengobox/pos-service/internal/platform/events"
	"github.com/bengobox/pos-service/internal/platform/marketflow"
)

// Shared "Walk-in" customer used to attribute transactions that need a customer but have none
// (e.g. an on-account sale rung up without selecting a customer). marketflow CRM is the source of
// truth; the contact is upserted there and treasury keys the AR balance on this identifier.
const (
	walkInPhone = "+000000000000"
	walkInName  = "Walk-in Customer"
)

// PaymentStatus defines valid payment states.
const (
	StatusPending   = "pending"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusRefunded  = "refunded"
)

// canonicalTenderMethod folds legacy/alias tender strings onto their canonical method so
// payment_data.method (which every method filter/breakdown reads) is always one of the
// advertised payment choices. The only rewrite today: the bare "manual" that older tills and
// queued offline payments send for the "M-Pesa Code" tender becomes "mpesa_manual" — reports
// used to surface those KES as an unexplained "manual (unspecified)" bucket even though the
// cashier explicitly picked M-Pesa Code (urban-loft 12 Jul audit: 84,250 of 112,180 affected).
func canonicalTenderMethod(method string) string {
	m := strings.ToLower(strings.TrimSpace(method))
	if m == "manual" {
		return "mpesa_manual"
	}
	return m
}

// isCashMethod returns true for tender types that settle immediately without a treasury gateway
// round-trip: cash, manual M-Pesa code entry (mpesa_manual, legacy "manual"), room charge, and
// external card-terminal/PDQ swipes (the standalone machine has already approved the card, so
// there is no online gateway step).
func isCashMethod(method string) bool {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "cash", "manual", "mpesa_manual", "room_charge", "card_manual", "pdq", "card_terminal":
		return true
	default:
		return false
	}
}

// treasuryMethodForImmediate maps an immediate-settle POS tender onto the payment_method treasury
// records. Cash/room charge are recorded as "cash"; external card-terminal swipes as "card_manual";
// a sighted M-Pesa Paybill/Till code as "mpesa_manual" (treasury settles it immediately but keeps
// it distinct from both "cash" and gateway "mpesa" so money-by-method reconciliation works —
// before 2026-07-13 these were lumped into "cash").
func treasuryMethodForImmediate(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "card_manual", "pdq", "card_terminal":
		return "card_manual"
	case "manual", "mpesa_manual":
		return "mpesa_manual"
	default:
		return "cash"
	}
}

// TenderOnAccount is the credit-sale ("sell on account") tender: no money is taken at the till;
// the amount is posted to the customer's AR balance in treasury instead.
const TenderOnAccount = "on_account"

// TenderCustomerCredit draws down a customer's EXISTING stored credit (a negative treasury AR
// balance) against this sale — the inverse-direction sibling of TenderOnAccount, which only ever
// CREATES a debt. Settles immediately like cash (no deferred settlement), usable on any sale type
// alongside other tenders in a split payment, not only on a fully-credit sale.
const TenderCustomerCredit = "customer_credit"

// TenderComplimentary is the "no-charge" tender: no money is taken and no debt is created —
// the bill is closed as a complimentary/goodwill gesture (staff meals, director's order,
// goodwill for a visiting team). Requires a mandatory reason and manager approval, enforced by
// the HTTP handler layer (see handlers/payments.go CreatePaymentIntent). Inventory still
// deducts (BOM backflush is price-agnostic); treasury posts the retail value to a
// Complimentary & Goodwill Expense account instead of Cash, so the cost stays visible on the P&L.
const TenderComplimentary = "complimentary"

// RecordPaymentRequest holds input for recording a POS payment.
type RecordPaymentRequest struct {
	TenantID      uuid.UUID
	TenantSlug    string
	OrderID       uuid.UUID
	TenderID      uuid.UUID
	TenderMethod  string // cash | card | mpesa | manual | room_charge | etc.
	Amount        float64
	Currency      string
	Reference     string // external reference (optional; set by treasury callback for digital)
	ExternalRef   string // cashier-entered ref for manual/paybill payments (stored on local payment)
	IntentID      string // treasury payment_intent_id (set for digital payments)
	PublicBaseURL string // used to construct initiateUrl for digital payments
	// Credit-sale (on_account) extras from the credit-sale details modal: an explicit due
	// date (wins over the customer's treasury credit period, which wins over +30 days) and
	// free-text notes stamped into order metadata.
	PaymentDueDate *time.Time
	CreditNotes    string
	// Complimentary (no-charge) extras — enforced (reason required, approval verified) by the
	// HTTP handler layer before RecordPaymentRequest is built; ApprovedByUserID is the manager
	// who authorized it (via PIN/card step-up or a one-time approval code).
	ComplimentaryReason string
	ApprovedByUserID    *uuid.UUID
}

// CreateIntentResult is returned to the caller when a digital payment intent is created.
type CreateIntentResult struct {
	PaymentIntentID string
	InitiateURL     string // proxy URL that treasury-ui calls when user selects gateway
	IsCash          bool   // true → payment is already settled, no further modal needed
}

// Service provides payment business logic.
type Service struct {
	client          *ent.Client
	orderSvc        *orders.Service
	treasuryClient  *treasury.Client
	inventoryClient *inventory.Client
	publisher       *events.Publisher
	marketflow      *marketflow.Client
	staffCredit     *staffcredit.Service
	log             *zap.Logger
	defaultCurrency string
	// printQueue enqueues the final customer receipt for the on-site Local Print Agent.
	printQueue *printing.Queue
	// notifHub pushes the KRA eTIMS fiscal block to the selling cashier's terminal the instant
	// a sale is signed — so the receipt's TIMS details appear via WS PUSH instead of the terminal
	// polling the receipt endpoint for ~30-50s waiting for the async fiscalisation to land.
	notifHub *notifications.Hub
}

// NewService creates a new payment service.
func NewService(client *ent.Client, orderSvc *orders.Service, defaultCurrency string, log *zap.Logger) *Service {
	if defaultCurrency == "" {
		defaultCurrency = "KES"
	}
	return &Service{
		client:          client,
		orderSvc:        orderSvc,
		log:             log.Named("payments.service"),
		defaultCurrency: defaultCurrency,
	}
}

// SetNotifHub injects the notification WebSocket hub used to PUSH the eTIMS fiscal block to the
// selling cashier's terminal the instant a sale is signed (replaces the terminal's receipt poll).
func (s *Service) SetNotifHub(h *notifications.Hub) {
	s.notifHub = h
}

// pushEtimsFiscalized pushes the order's KRA eTIMS fiscal identity to the selling cashier's
// terminal over the notification WebSocket, so the open receipt merges the "KRA TIMS Details"
// block immediately instead of polling the receipt endpoint. Best-effort + idempotent: a no-op
// when the hub is unset, the order carries no cashier, or the sale isn't fiscalised yet; the
// terminal keeps a short fallback poll for the case where its socket is momentarily down. Called
// from BOTH sign paths — the synchronous checkout sign (signEtimsSync) and the async
// treasury.etims.invoice_transmitted subscriber — so whichever lands first delivers the block.
func (s *Service) pushEtimsFiscalized(order *ent.POSOrder) {
	if s.notifHub == nil || order == nil || order.UserID == uuid.Nil {
		return
	}
	inv := derefStr(order.EtimsInvoiceNumber)
	cu := derefStr(order.EtimsCuInvNo)
	if inv == "" && cu == "" {
		return // nothing fiscalised yet — no block to push
	}
	s.notifHub.BroadcastToUser(order.TenantID, order.UserID, notifications.Message{
		Type: "etims_fiscalized",
		Payload: map[string]any{
			"order_id":       order.ID.String(),
			"order_number":   order.OrderNumber,
			"invoice_number": inv,
			"cu_invoice_no":  cu,
			"scu_id":         derefStr(order.EtimsScuID),
			"rcpt_sign":      derefStr(order.EtimsRcptSign),
			"qr_code_url":    derefStr(order.EtimsQrCodeURL),
			"kra_pin":        derefStr(order.EtimsKraPin),
		},
	})
}

// derefStr safely dereferences an optional string field.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// SetTreasuryClient injects the treasury S2S client after construction (avoids circular init).
func (s *Service) SetTreasuryClient(c *treasury.Client) {
	s.treasuryClient = c
}

// SetInventoryClient injects the inventory S2S client for consumption backflush.
func (s *Service) SetInventoryClient(c *inventory.Client) {
	s.inventoryClient = c
}

// SetPublisher injects the event publisher for pos.sale.finalized and related events.
func (s *Service) SetPublisher(p *events.Publisher) {
	s.publisher = p
}

// SetStaffCredit injects the staff fund-from-salary provisioner so a staff credit sale routes the
// debt to ERP payroll instead of customer AR. Optional; nil = staff credit sales fall back to AR.
func (s *Service) SetStaffCredit(sc *staffcredit.Service) {
	s.staffCredit = sc
}

// SetMarketFlowClient injects the marketflow CRM client, used to resolve the shared "Walk-in"
// customer when an on-account sale has no customer attached.
func (s *Service) SetMarketFlowClient(c *marketflow.Client) {
	s.marketflow = c
}

// SetPrintQueue wires the background print-job queue (final receipt on full payment).
func (s *Service) SetPrintQueue(q *printing.Queue) {
	s.printQueue = q
}

// CreatePaymentIntent creates a treasury payment intent and returns the intent ID + initiateUrl.
//
// Cash/manual/room_charge: creates a "cash" intent → treasury settles it immediately → records
// the local payment as completed and returns IsCash=true.
//
// Digital (card, mpesa, etc.): creates a "pending" intent → returns intent ID + initiateUrl for
// the pos-ui to open TreasuryPaymentModal. The local payment is recorded as pending and will be
// completed by the treasury.payment.success NATS subscriber.
func (s *Service) CreatePaymentIntent(ctx context.Context, req RecordPaymentRequest) (*CreateIntentResult, error) {
	if s.treasuryClient == nil {
		return nil, fmt.Errorf("payments: treasury client not configured")
	}

	// Canonicalize the tender before anything reads it: payment_data.method must always be one of
	// the advertised payment choices (legacy "manual" → "mpesa_manual"), for both online captures
	// and queued offline replays from older tills.
	req.TenderMethod = canonicalTenderMethod(req.TenderMethod)

	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(req.OrderID), posorder.TenantID(req.TenantID)).
		WithPayments().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: order not found: %w", err)
	}

	if order.Status == orders.StatusCancelled || order.Status == orders.StatusVoided {
		return nil, fmt.Errorf("payments: cannot pay %s order", order.Status)
	}
	// Settle gate: a completed/refunded order is already finalized (GL posted, stock
	// deducted, receipt issued) — recording another payment against it would double-post
	// the sale downstream.
	if order.Status == orders.StatusCompleted || order.Status == orders.StatusRefunded {
		return nil, fmt.Errorf("payments: order %s is already settled and cannot be paid again", order.OrderNumber)
	}

	// Never trust the client amount: settling a bill from a list (e.g. "Settle Bill" in
	// My Bills, or a resumed back-office sale) can pass amount=0 — or a STALE total when
	// the client's in-memory order diverged from the server (2026-07-14: a resumed sale
	// charged the pre-discount 10,180 against a 9,180 order). Derive the charge from the
	// order's outstanding balance when missing, and cap it there so a sale can never
	// collect more than the order is worth.
	outstanding := s.outstandingBalance(ctx, order)
	if outstanding <= 0 {
		return nil, fmt.Errorf("payments: order has no outstanding balance to charge")
	}
	if req.Amount <= 0 {
		req.Amount = outstanding
	}
	if req.Amount > outstanding+0.01 {
		s.log.Warn("payment amount exceeds outstanding balance — clamping to outstanding",
			zap.String("order_id", order.ID.String()),
			zap.Float64("requested", req.Amount),
			zap.Float64("outstanding", outstanding))
		req.Amount = outstanding
	}

	currency := req.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}

	// Credit sale (sell on account): no payment is taken now. Post the amount to the customer's AR
	// balance in treasury (which enforces the credit limit), then settle the order on account.
	if strings.EqualFold(req.TenderMethod, TenderOnAccount) {
		return s.recordCreditSale(ctx, order, req, currency)
	}

	// Complimentary (no-charge): close the bill without collecting cash or creating a debt.
	// Reason + manager approval are already enforced by the HTTP handler before this is called.
	if strings.EqualFold(req.TenderMethod, TenderComplimentary) {
		return s.recordComplimentarySale(ctx, order, req, currency)
	}

	// Apply existing stored credit: settle (part of) the sale from the customer's treasury credit
	// balance instead of collecting cash. Treasury enforces the available-credit cap.
	if strings.EqualFold(req.TenderMethod, TenderCustomerCredit) {
		return s.applyCustomerCreditTender(ctx, order, req, currency)
	}

	cash := isCashMethod(req.TenderMethod)

	paymentMethod := "pending"
	if cash {
		paymentMethod = treasuryMethodForImmediate(req.TenderMethod)
	}

	intentReq := treasury.CreateIntentRequest{
		SourceService: "pos",
		ReferenceID:   payref.Build("POS", req.TenantSlug, req.TenantID, req.OrderID),
		ReferenceType: "pos_order",
		Amount:        req.Amount,
		Currency:      currency,
		PaymentMethod: paymentMethod,
		Description:   fmt.Sprintf("POS order %s", order.OrderNumber),
		OutletID:      order.OutletID.String(),
		// entity_id lets consumers recover the order UUID now that reference_id is a prefixed,
		// service-identifiable string rather than the bare order UUID.
		Metadata: map[string]any{"service": "pos", "entity_id": req.OrderID.String()},
	}
	// Carry the cashier-entered external reference (card terminal approval code / M-Pesa code) so
	// treasury records it on the immediate-settle PaymentTransaction instead of a synthetic ref.
	if cash && req.ExternalRef != "" {
		intentReq.Metadata["external_ref"] = req.ExternalRef
	}

	intent, err := s.treasuryClient.CreateIntent(ctx, req.TenantSlug, req.OrderID.String(), intentReq)
	if err != nil {
		return nil, fmt.Errorf("payments: create treasury intent: %w", err)
	}

	result := &CreateIntentResult{
		PaymentIntentID: intent.ResolvedID(),
		IsCash:          cash,
	}

	if !cash {
		// Prefer treasury's returned initiate_url (public, no auth required).
		// Fall back to the pos-api proxy only when treasury doesn't return one.
		if intent.InitiateURL != "" {
			result.InitiateURL = intent.InitiateURL
		} else {
			result.InitiateURL = fmt.Sprintf("%s/api/v1/%s/pos/payments/initiate", req.PublicBaseURL, req.TenantSlug)
		}

		// Record local payment as pending — will be completed by treasury NATS subscriber
		_, err = s.client.POSPayment.Create().
			SetOrderID(req.OrderID).
			SetTenderID(req.TenderID).
			SetAmount(req.Amount).
			SetCurrency(currency).
			SetStatus(StatusPending).
			SetPaymentData(map[string]any{"method": req.TenderMethod}).
			SetNillableExternalReference(nilIfEmpty(intent.ResolvedID())).
			Save(ctx)
		if err != nil {
			// Must NOT swallow this: without the local pending POSPayment row, treasury's
			// payment.succeeded callback can't match the intent back to an order (it looks the order up
			// by this row's external_reference), leaving the order stuck unpaid. Fail loudly so the
			// caller can retry — each retry creates a fresh intent, and the unconfirmed one expires.
			return nil, fmt.Errorf("payments: record pending payment: %w", err)
		}
		return result, nil
	}

	// Cash / manual: record as completed immediately.
	// For manual payments use the cashier-entered ref; otherwise store the treasury intent ID.
	cashRef := intent.ResolvedID()
	if req.ExternalRef != "" {
		cashRef = req.ExternalRef
	}
	_, err = s.client.POSPayment.Create().
		SetOrderID(req.OrderID).
		SetTenderID(req.TenderID).
		SetAmount(req.Amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{"method": req.TenderMethod}).
		SetNillableExternalReference(nilIfEmpty(cashRef)).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: record cash payment: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order)
	return result, nil
}

// recordCreditSale settles an order "on account" (credit sale). It posts the amount to the
// customer's AR balance in treasury FIRST — treasury enforces the customer credit limit and
// rejects over-limit sales — and only on success records a completed on-account payment and
// completes the order (so goods never leave without the debt being recorded). Requires a customer.
func (s *Service) recordCreditSale(ctx context.Context, order *ent.POSOrder, req RecordPaymentRequest, currency string) (*CreateIntentResult, error) {
	// Staff fund-from-salary: when the order metadata flags a staff-funded credit sale, route the
	// debt to ERP payroll (a StaffPurchaseDeduction) instead of customer AR. Still records the
	// on-account payment + completes the order so goods never leave without the debt recorded.
	if staffID, months, ok := staffCreditFromOrder(order); ok && s.staffCredit != nil && s.staffCredit.Entitled(ctx, req.TenantID) {
		if months < 1 {
			months = 1
		}
		principal := decimal.NewFromFloat(req.Amount)
		install := principal.Div(decimal.NewFromInt(int64(months)))
		orderID := order.ID
		outletID := order.OutletID
		if _, perr := s.staffCredit.Provision(ctx, req.TenantID, staffcredit.ProvisionInput{
			OutletID:          &outletID,
			StaffMemberID:     staffID,
			Origin:            "credit_sale",
			POSOrderID:        &orderID,
			Principal:         principal,
			InstallmentAmount: install,
			InstallmentsTotal: months,
		}); perr != nil {
			s.log.Warn("staff-credit provision failed (credit sale)", zap.Error(perr))
		} else {
			if _, err := s.client.POSPayment.Create().
				SetOrderID(req.OrderID).SetTenderID(req.TenderID).SetAmount(req.Amount).
				SetCurrency(currency).SetStatus(StatusCompleted).
				SetPaymentData(map[string]any{"method": TenderOnAccount, "fund_from_salary": true}).
				SetNillableExternalReference(nilIfEmpty(order.OrderNumber)).
				Save(ctx); err != nil {
				return nil, fmt.Errorf("payments: record staff on-account payment: %w", err)
			}
			s.completeOrderIfFullyPaid(ctx, order)
			return &CreateIntentResult{IsCash: true}, nil
		}
	}

	if s.treasuryClient == nil {
		return nil, fmt.Errorf("payments: treasury client not configured")
	}
	phone := ""
	if order.CustomerPhone != nil {
		phone = *order.CustomerPhone
	}
	name := ""
	if order.CustomerName != nil {
		name = *order.CustomerName
	}
	// A credit sale MUST be booked against a real customer account — never the shared "Walk-in" ghost
	// (it commingles unrelated debts onto one row that can never be collected or reconciled).
	// A STAFF credit sale that falls through to AR (fund-from-salary off or not entitled) has no
	// customer phone — key the treasury debtor on the staff member id so each staff member gets a
	// distinct, reconcilable AR row instead of an empty identifier.
	staffID, _, isStaff := staffCreditFromOrderParty(order)
	if isStaff && strings.TrimSpace(phone) == "" {
		phone = "staff:" + staffID.String()
	}
	// Non-staff: require a PHONE (the AR/CRM key) — a bare or "Walk-in Customer" name can't be
	// collected or netted against returns/payments. The pos-ui blocks this too; this is the
	// server-side backstop for direct API calls.
	if !isStaff {
		trimmedName := strings.TrimSpace(name)
		if strings.TrimSpace(phone) == "" ||
			strings.EqualFold(trimmedName, "walk-in customer") || strings.EqualFold(trimmedName, "walk in customer") {
			return nil, fmt.Errorf("payments: credit sale requires a selected customer with a phone number")
		}
	}
	// Resolve the canonical AR key — the selected customer's marketflow CRM contact (the SAME source
	// the return path uses), via the loyalty account for this phone. Sending both the CRM id and the
	// phone lets treasury net the credit sale, its returns and its opening balance on ONE customer row.
	crmContactID := s.ResolveCrmContactID(ctx, req.TenantID, phone)

	creditResp, err := s.treasuryClient.RecordCreditSale(ctx, req.TenantSlug, treasury.CreditSaleRequest{
		CrmContactID:       crmContactID,
		CustomerIdentifier: phone,
		CustomerName:       name,
		POSOrderID:         order.ID.String(),
		Reference:          order.OrderNumber,
		Amount:             req.Amount,
		Currency:           currency,
		UserID:             order.UserID.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("payments: credit sale rejected: %w", err)
	}
	// Mark the order as an on-account sale and stamp when the credit falls due, so the
	// All-Sales "Overdue" filter/badge can surface late credit sales. Precedence: the
	// cashier's explicit due date (credit-sale details modal) → the customer's treasury
	// payment period → a 30-day default (every credit sale MUST fall due eventually).
	// Best-effort: a metadata write failure never fails the sale.
	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	meta["on_account"] = true
	switch {
	case req.PaymentDueDate != nil:
		meta["payment_due_date"] = req.PaymentDueDate.Format(time.RFC3339)
	case creditResp != nil && creditResp.CreditPeriodDays != nil && *creditResp.CreditPeriodDays > 0:
		meta["payment_due_date"] = time.Now().AddDate(0, 0, *creditResp.CreditPeriodDays).Format(time.RFC3339)
	default:
		meta["payment_due_date"] = time.Now().AddDate(0, 0, 30).Format(time.RFC3339)
	}
	if req.CreditNotes != "" {
		meta["credit_notes"] = req.CreditNotes
	}
	if merr := s.client.POSOrder.UpdateOneID(order.ID).SetMetadata(meta).Exec(ctx); merr != nil {
		s.log.Warn("payments: failed to stamp on-account metadata", zap.Error(merr))
	}

	if _, err := s.client.POSPayment.Create().
		SetOrderID(req.OrderID).
		SetTenderID(req.TenderID).
		SetAmount(req.Amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{"method": TenderOnAccount}).
		SetNillableExternalReference(nilIfEmpty(order.OrderNumber)).
		Save(ctx); err != nil {
		return nil, fmt.Errorf("payments: record on-account payment: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order)
	return &CreateIntentResult{IsCash: true}, nil
}

// recordComplimentarySale closes a bill as "complimentary" (no-charge): no money is taken and no
// debt is created — unlike recordCreditSale, this never touches treasury AR/credit-limit checks.
// A reason is mandatory (the HTTP handler already enforces this alongside manager approval; this
// is the defense-in-depth backstop for direct API callers). Records a completed payment for the
// FULL order amount (not zero) so completeOrderIfFullyPaid closes the order normally and the
// pos.sale.finalized event carries a non-zero total_amount — treasury's zero-amount guard would
// otherwise also skip the still-required COGS/inventory-relief posting.
func (s *Service) recordComplimentarySale(ctx context.Context, order *ent.POSOrder, req RecordPaymentRequest, currency string) (*CreateIntentResult, error) {
	if strings.TrimSpace(req.ComplimentaryReason) == "" {
		return nil, fmt.Errorf("payments: complimentary sale requires a reason")
	}

	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	meta["complimentary"] = true
	meta["complimentary_reason"] = req.ComplimentaryReason
	if req.ApprovedByUserID != nil {
		meta["complimentary_approved_by"] = req.ApprovedByUserID.String()
	}
	if merr := s.client.POSOrder.UpdateOneID(order.ID).SetMetadata(meta).Exec(ctx); merr != nil {
		s.log.Warn("payments: failed to stamp complimentary metadata", zap.Error(merr))
	}

	if _, err := s.client.POSPayment.Create().
		SetOrderID(req.OrderID).
		SetTenderID(req.TenderID).
		SetAmount(req.Amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{"method": TenderComplimentary, "reason": req.ComplimentaryReason}).
		SetNillableExternalReference(nilIfEmpty(order.OrderNumber)).
		Save(ctx); err != nil {
		return nil, fmt.Errorf("payments: record complimentary payment: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order)
	return &CreateIntentResult{IsCash: true}, nil
}

// ResolveCrmContactID returns the marketflow CRM contact id for a customer phone (via the loyalty
// account), or "" when none is linked. This is the canonical treasury AR key — the same resolution
// the return path uses — so a credit sale, its returns and its opening balance all land on one row.
func (s *Service) ResolveCrmContactID(ctx context.Context, tenantID uuid.UUID, phone string) string {
	if strings.TrimSpace(phone) == "" {
		return ""
	}
	acc, err := s.client.LoyaltyAccount.Query().
		Where(entloyalty.TenantID(tenantID), entloyalty.CustomerPhone(phone)).
		First(ctx)
	if err != nil || acc == nil || acc.CrmContactID == nil {
		return ""
	}
	return acc.CrmContactID.String()
}

// staffCreditFromOrderParty extracts the staff BILL-TO party from an order's metadata
// (party_type=staff + staff_member_id), regardless of the fund-from-salary flag. Used to key the
// treasury AR debtor when a staff credit sale falls through to AR.
func staffCreditFromOrderParty(order *ent.POSOrder) (staffID uuid.UUID, fund bool, ok bool) {
	if order == nil || order.Metadata == nil {
		return uuid.Nil, false, false
	}
	sid, _ := order.Metadata["staff_member_id"].(string)
	if sid == "" {
		return uuid.Nil, false, false
	}
	id, err := uuid.Parse(sid)
	if err != nil {
		return uuid.Nil, false, false
	}
	fund, _ = order.Metadata["fund_from_salary"].(bool)
	return id, fund, true
}

// staffCreditFromOrder extracts the staff fund-from-salary intent from an order's metadata
// (fund_from_salary + staff_member_id [+ installment_months]), set by the credit-sale UI.
func staffCreditFromOrder(order *ent.POSOrder) (staffID uuid.UUID, months int, ok bool) {
	id, fund, ok := staffCreditFromOrderParty(order)
	if !ok || !fund {
		return uuid.Nil, 0, false
	}
	switch v := order.Metadata["installment_months"].(type) {
	case float64:
		months = int(v)
	case int:
		months = v
	}
	return id, months, true
}

// ConfirmPaymentByIntentID is called by the treasury.payment.succeeded NATS subscriber.
// It finds the pending local payment for the given treasury intent ID and marks it completed,
// then completes the order if fully paid. settledAmount is the amount treasury ACTUALLY
// captured (0 = unknown): when it differs from the pending row's opening amount, the row is
// corrected so paid_total only ever counts money that was really collected — an intent
// opened for the full total but partially captured must not flip the order to paid.
func (s *Service) ConfirmPaymentByIntentID(ctx context.Context, tenantID uuid.UUID, intentID string, settledAmount float64, payerName string) error {
	payments, err := s.client.POSPayment.Query().
		Where(pospayment.ExternalReference(intentID)).
		WithOrder().
		All(ctx)
	if err != nil || len(payments) == 0 {
		return fmt.Errorf("payments: no pending payment found for intent %s", intentID)
	}

	for _, p := range payments {
		if p.Status == StatusCompleted {
			continue
		}
		upd := s.client.POSPayment.UpdateOne(p).SetStatus(StatusCompleted)
		if settledAmount > 0 && settledAmount != p.Amount {
			s.log.Warn("treasury settled amount differs from pending payment — correcting local row",
				zap.String("payment_id", p.ID.String()),
				zap.Float64("pending_amount", p.Amount),
				zap.Float64("settled_amount", settledAmount))
			upd = upd.SetAmount(settledAmount)
		}
		// Mark the row gateway-settled so it can never be edited/voided at the till —
		// reversing gateway money must go through the refund flow.
		data := p.PaymentData
		if data == nil {
			data = map[string]any{}
		}
		data["settled_via"] = "treasury_gateway"
		// Payer name resolved by the gateway (e.g. Paystack customer) so the receipt can show
		// "Paid by: <name>" for online payments when no customer was keyed in at the till.
		if payerName != "" {
			data["payer_name"] = payerName
		}
		upd = upd.SetPaymentData(data)
		if _, err := upd.Save(ctx); err != nil {
			return fmt.Errorf("payments: confirm payment %s: %w", p.ID, err)
		}
		if p.Edges.Order != nil {
			s.completeOrderIfFullyPaid(ctx, p.Edges.Order)
		}
	}
	return nil
}

// FailPaymentByIntentID marks a pending payment as failed (called by treasury.payment.failed subscriber).
func (s *Service) FailPaymentByIntentID(ctx context.Context, intentID string) error {
	payments, err := s.client.POSPayment.Query().
		Where(pospayment.ExternalReference(intentID)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("payments: query by intent: %w", err)
	}
	for _, p := range payments {
		if p.Status != StatusPending {
			continue
		}
		if _, err := s.client.POSPayment.UpdateOne(p).SetStatus(StatusFailed).Save(ctx); err != nil {
			return fmt.Errorf("payments: fail payment %s: %w", p.ID, err)
		}
	}
	return nil
}

// RecordPayment is the legacy direct-record path (cash tender with no treasury intent).
// Kept for internal use and backward compatibility with existing POS terminal flows.
func (s *Service) RecordPayment(ctx context.Context, req RecordPaymentRequest) (*ent.POSPayment, error) {
	// Same canonicalization as CreatePaymentIntent: payment_data.method must always be one of
	// the advertised payment choices (legacy "manual" → "mpesa_manual").
	req.TenderMethod = canonicalTenderMethod(req.TenderMethod)

	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(req.OrderID), posorder.TenantID(req.TenantID)).
		WithPayments().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: order not found: %w", err)
	}

	if order.Status == orders.StatusCancelled || order.Status == orders.StatusVoided {
		return nil, fmt.Errorf("payments: cannot pay %s order", order.Status)
	}
	// Same settle gate as CreatePaymentIntent: an already-finalized order must never take
	// another payment, and the charge is derived from / capped at the outstanding balance.
	if order.Status == orders.StatusCompleted || order.Status == orders.StatusRefunded {
		return nil, fmt.Errorf("payments: order %s is already settled and cannot be paid again", order.OrderNumber)
	}

	outstanding := s.outstandingBalance(ctx, order)
	if outstanding <= 0 {
		return nil, fmt.Errorf("payments: order has no outstanding balance to charge")
	}
	if req.Amount <= 0 {
		req.Amount = outstanding
	}
	if req.Amount > outstanding+0.01 {
		s.log.Warn("payment amount exceeds outstanding balance — clamping to outstanding",
			zap.String("order_id", order.ID.String()),
			zap.Float64("requested", req.Amount),
			zap.Float64("outstanding", outstanding))
		req.Amount = outstanding
	}

	currency := req.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}

	payment, err := s.client.POSPayment.Create().
		SetOrderID(req.OrderID).
		SetTenderID(req.TenderID).
		SetAmount(req.Amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetNillableExternalReference(nilIfEmpty(req.Reference)).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: record failed: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order)
	return payment, nil
}

// ListOrderPayments returns all payments for an order.
func (s *Service) ListOrderPayments(ctx context.Context, tenantID, orderID uuid.UUID) ([]*ent.POSPayment, error) {
	exists, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		Exist(ctx)
	if err != nil || !exists {
		return nil, fmt.Errorf("payments: order not found")
	}

	return s.client.POSPayment.Query().
		Where(pospayment.OrderID(orderID)).
		All(ctx)
}

// outstandingBalance returns the order total minus all completed payments (>= 0).
func (s *Service) outstandingBalance(ctx context.Context, order *ent.POSOrder) float64 {
	payments, err := s.client.POSPayment.Query().
		Where(pospayment.OrderID(order.ID), pospayment.Status(StatusCompleted)).
		All(ctx)
	if err != nil {
		return order.TotalAmount
	}
	var paid float64
	for _, p := range payments {
		paid += p.Amount
	}
	if remaining := order.TotalAmount - paid; remaining > 0 {
		return remaining
	}
	return 0
}

// RecomputePaidTotal recalculates an order's paid_total from its COMPLETED payments and
// persists it on the order. It is the single write path for paid_total — called after every
// payment mutation (record, confirm, void/edit) — so the payment-status filter, the row
// badge, and completeOrderIfFullyPaid all read one consistent value.
//
// paid_total counts only money ACTUALLY COLLECTED: on-account (credit-sale) tender rows are
// excluded — credit is a debt in treasury AR, not cash banked, so a credit sale must read
// due/overdue on every sales surface instead of "paid". The second return value (settled)
// additionally includes on-account rows — it is what order COMPLETION keys on, because a
// credit sale still closes the order (goods leave; stock/GL/AR fire via pos.sale.finalized).
func (s *Service) RecomputePaidTotal(ctx context.Context, orderID uuid.UUID) (collected float64, settled float64, err error) {
	rows, err := s.client.POSPayment.Query().
		Where(pospayment.OrderID(orderID), pospayment.Status(StatusCompleted)).
		All(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("payments: sum completed payments: %w", err)
	}
	for _, p := range rows {
		settled += p.Amount
		method, _ := p.PaymentData["method"].(string)
		if !strings.EqualFold(method, TenderOnAccount) {
			collected += p.Amount
		}
	}
	if err := s.client.POSOrder.UpdateOneID(orderID).SetPaidTotal(collected).Exec(ctx); err != nil {
		return collected, settled, fmt.Errorf("payments: store paid_total: %w", err)
	}
	return collected, settled, nil
}

// RecheckCompletion re-evaluates a single order's payment-completion status through the exact
// same path a normal payment submission uses (RecomputePaidTotal + completeOrderIfFullyPaid —
// which, if satisfied, transitions status, publishes pos.sale.finalized for treasury GL/inventory
// backflush/eTIMS, enqueues the receipt, calculates commissions, and releases the table).
//
// This exists as a manual recovery tool for orders whose totals were corrected out-of-band (e.g. a
// direct DB fix for a stuck order) after the completion check had already failed once against a
// wrong total — normal payment submission never re-checks a $0-outstanding order, so nothing else
// re-triggers this. Not wired to any public route; call only via the S2S recheck-completion
// endpoint (internal ops tool, INTERNAL_SERVICE_KEY-gated).
func (s *Service) RecheckCompletion(ctx context.Context, tenantID, orderID uuid.UUID) error {
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return fmt.Errorf("payments: order not found: %w", err)
	}
	s.completeOrderIfFullyPaid(ctx, order)
	return nil
}

// completeOrderIfFullyPaid recomputes the order's paid_total and marks the order completed
// when its completed payments cover the total.
//
// NOTE: this used to take an additionalAmount that was ADDED ON TOP of the queried sum —
// but every caller had already persisted the payment row before calling, so the amount was
// double-counted and any partial payment >= half the total wrongly completed the order
// (root cause of "PAID badge with a positive Sell Due" / broken Partial filtering).
func (s *Service) completeOrderIfFullyPaid(ctx context.Context, order *ent.POSOrder) {
	// Completion keys on SETTLED (collected + on-account credit): a credit sale closes the
	// order even though paid_total (collected cash only) stays below the total.
	_, settled, err := s.RecomputePaidTotal(ctx, order.ID)
	if err != nil {
		s.log.Warn("recompute paid_total failed", zap.String("order_id", order.ID.String()), zap.Error(err))
		return
	}

	if settled+0.01 >= order.TotalAmount {
		// Atomically CLAIM the completion transition: the conditional UPDATE flips the order
		// to completed only while it is still in a settleable state, so exactly ONE of any
		// concurrent payment confirmations wins the transition. The in-memory
		// ValidateStatusTransition check this replaces was a TOCTOU race — two near-simultaneous
		// full payments could both pass it and publish pos.sale.finalized twice, double-posting
		// the sale to treasury GL and inventory backflush (same family as the 2026-07-14
		// duplicate-settle incident).
		n, updateErr := s.client.POSOrder.Update().
			Where(
				posorder.ID(order.ID),
				posorder.StatusIn(orders.StatusDraft, orders.StatusOpen, orders.StatusPendingPayment),
			).
			SetStatus(orders.StatusCompleted).
			Save(ctx)
		if updateErr != nil {
			s.log.Warn("failed to complete order after full payment",
				zap.String("order_id", order.ID.String()),
				zap.Error(updateErr))
			return
		}
		if n == 0 {
			// Already completed by another path/event (e.g. a second payment confirmation, or a
			// flow that completed the order directly), or in a state that can't complete: still
			// sweep any open tickets — idempotent, only pending/in_progress/ready tickets are
			// touched — so a settled bill can never leave food sitting on the live KDS board.
			s.orderSvc.AutoClearKDSTicketsForOrder(ctx, order.TenantID, order.ID)
			return
		}
		updated, gerr := s.client.POSOrder.Get(ctx, order.ID)
		if gerr != nil {
			// Publishing must not be skipped once the transition is claimed — fall back to the
			// pre-update snapshot (its totals are what the settlement check validated).
			s.log.Warn("reload completed order failed — publishing from pre-update snapshot",
				zap.String("order_id", order.ID.String()), zap.Error(gerr))
			updated = order
			updated.Status = orders.StatusCompleted
		}
		// Hand the post-settlement fan-out (eTIMS sign + receipt print, event publish, commissions,
		// table release, KDS clear) to a background worker so the cashier's "Confirm" returns the
		// moment the sale is claimed completed — none of this needs to block the response, INCLUDING
		// the KRA eTIMS sign (its sandbox latency was hanging Confirm ~30s). The single-winner UPDATE
		// above already ran, so exactly one dispatch happens per order.
		s.dispatchPostFinalize(updated)
	}
}

// dispatchPostFinalize runs the post-settlement fan-out OFF the payment request path so a cash
// (or async digital) confirmation returns as soon as the sale is claimed completed, instead of
// waiting on the event publish, receipt enqueue, commission calc, table release and KDS sweep.
//
// Safe because the atomic single-winner completion UPDATE in completeOrderIfFullyPaid has already
// committed before this is called: exactly one dispatch happens per order, so nothing here can
// double-post the sale to treasury GL / inventory backflush. Runs on a DETACHED, timeout-bounded
// context (never the request context — that is cancelled the instant the handler responds; see
// feedback_background_notifications_scope) with panic recovery so a fan-out error can never crash
// the process or unwind a settled sale.
// FinalizeExternalOrder runs the post-finalize fan-out (GL posting, inventory stock backflush, and
// eTIMS fiscalisation) for an already-completed order created OUTSIDE the payment-intent flow — the
// layaway completion path, where installments accrued on the plan rather than as POSPayments. It
// reuses the exact same detached, panic-guarded fan-out as a normal sale so a fully-paid layaway is
// synced identically to any other completed sale. Idempotency is the subscribers' responsibility
// (all keyed on order id).
func (s *Service) FinalizeExternalOrder(order *ent.POSOrder) {
	s.dispatchPostFinalize(order)
}

func (s *Service) dispatchPostFinalize(order *ent.POSOrder) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("post-finalize panic recovered",
					zap.String("order_id", order.ID.String()), zap.Any("panic", r))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.runPostFinalize(ctx, order)
	}()
}

// runPostFinalize performs the best-effort downstream work after an order settles. It runs OFF the
// payment request path (dispatchPostFinalize spawns it on a detached, timeout-bounded context), so
// none of it delays the cashier's Confirm. The independent steps fan out concurrently to minimise
// total settle time; only the eTIMS sign → receipt-print chain runs in sequence, because the printed
// bill must carry the fiscal block. Each step logs its own errors and is individually panic-guarded.
func (s *Service) runPostFinalize(ctx context.Context, order *ent.POSOrder) {
	var wg sync.WaitGroup
	// pos.sale.finalized → treasury GL posting + inventory stock backflush (async NATS consumers,
	// via the durable outbox). Independent of the rest.
	s.goSafe(&wg, order, "publish", func() { s.publishSaleFinalized(ctx, order) })
	s.goSafe(&wg, order, "commissions", func() { s.calcCommissions(ctx, order) })
	// Free the table once the bill is settled, regardless of which flow (waiter My Bills, cashier
	// orders page, or async digital confirmation) closed it — mirrors the manual ReleaseTable endpoint.
	s.goSafe(&wg, order, "release-table", func() { s.releaseTableForOrder(ctx, order.ID) })
	// A settled order is done in the kitchen/bar regardless of whether staff ever bumped its tickets
	// on the KDS board (quick-service counter sales skip the board entirely) — force-serve any that
	// are still open so the live board doesn't show food for an order that's already been paid for.
	s.goSafe(&wg, order, "kds-clear", func() { s.orderSvc.AutoClearKDSTicketsForOrder(ctx, order.TenantID, order.ID) })

	// eTIMS sign → receipt print, in sequence (the printed bill must be fiscalised first). Runs here
	// on the fan-out goroutine — NOT the confirm request path — so KRA latency never hangs Confirm;
	// the terminal's receipt poll merges the TIMS block into the on-screen receipt when it lands.
	s.signEtimsSync(ctx, order)
	s.enqueueReceiptPrint(ctx, order)

	wg.Wait()
}

// goSafe runs one post-finalize step in its own panic-guarded goroutine and tracks it on wg. A
// panic in a bare goroutine would crash the process, so each parallel step is individually recovered
// (dispatchPostFinalize's recover only covers its own stack, not children it spawns).
func (s *Service) goSafe(wg *sync.WaitGroup, order *ent.POSOrder, step string, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("post-finalize step panic recovered",
					zap.String("step", step), zap.String("order_id", order.ID.String()), zap.Any("panic", r))
			}
		}()
		fn()
	}()
}

// releaseTableForOrder frees any table occupied by this order: it closes the
// active table assignment and sets the table back to available. No-op when the
// order isn't tied to a table (takeaway, delivery, retail).
func (s *Service) releaseTableForOrder(ctx context.Context, orderID uuid.UUID) {
	asgns, err := s.client.TableAssignment.Query().
		Where(tableassignment.OrderID(orderID), tableassignment.ReleasedAtIsNil()).
		All(ctx)
	if err != nil || len(asgns) == 0 {
		return
	}
	now := time.Now()
	for _, a := range asgns {
		if _, uerr := s.client.TableAssignment.UpdateOne(a).SetReleasedAt(now).Save(ctx); uerr != nil {
			s.log.Warn("release table: close assignment failed", zap.Error(uerr))
		}
		if _, uerr := s.client.Table.Update().
			Where(enttable.ID(a.TableID)).
			SetStatus("available").
			Save(ctx); uerr != nil {
			s.log.Warn("release table: set available failed", zap.Error(uerr))
		}
	}
}

// signEtimsSync signs the sale with KRA eTIMS synchronously (treasury S2S) at checkout, so the
// terminal prints an eTIMS-signed ETR receipt immediately instead of waiting for the async
// pos.sale.finalized → 30s-worker path. Best-effort: on any failure the eTIMS fields are left empty
// and the async event path (still published in the fan-out) signs the sale, with GetReceipt's pull
// backfilling on reprint. Idempotent end-to-end — treasury unique-keys the sale and returns the
// existing fiscal record for an already-signed sale. Mutates `order` in place on success so the
// fan-out's receipt print carries the fiscal block.
// saleTender is the per-tender breakdown shared by the pos.sale.finalized event and the
// synchronous eTIMS sign request, so the mode of payment treasury sees is identical on both paths.
type saleTender struct {
	Type   string
	Amount float64
	Status string
}

// resolveSaleSettlement inspects an order's payments and returns its selling scheme plus the
// on-account / complimentary amounts and the per-tender breakdown. Shared by publishSaleFinalized
// (async event → GL routing) and signEtimsSync (sync ETR sign → KRA pmtTyCd) so both declare the
// same mode of payment. Type falls back to the PaymentData method when the Tender catalog Type is
// blank, so digital instruments (card/mpesa) are still identifiable.
func (s *Service) resolveSaleSettlement(ctx context.Context, order *ent.POSOrder) (scheme string, onAccount, comp float64, compReason string, tenders []saleTender) {
	scheme = "cash"
	if pays, perr := s.client.POSPayment.Query().Where(pospayment.OrderID(order.ID)).All(ctx); perr == nil {
		// Preload every referenced tender in ONE query instead of a Tender.Get per payment
		// (N+1 on the hot sign / sale-finalize path). Reuses the IDIn preload in manage.go.
		tenderType := make(map[uuid.UUID]string, len(pays))
		ids := make([]uuid.UUID, 0, len(pays))
		for _, p := range pays {
			ids = append(ids, p.TenderID)
		}
		if len(ids) > 0 {
			if ts, terr := s.client.Tender.Query().Where(tender.IDIn(ids...)).All(ctx); terr == nil {
				for _, t := range ts {
					tenderType[t.ID] = t.Type
				}
			}
		}
		for _, p := range pays {
			tType := tenderType[p.TenderID]
			method, _ := p.PaymentData["method"].(string)
			typ := tType
			if typ == "" {
				typ = method
			}
			tenders = append(tenders, saleTender{Type: typ, Amount: p.Amount, Status: p.Status})
			if strings.EqualFold(tType, TenderOnAccount) && p.Status == StatusCompleted {
				onAccount += p.Amount
			}
			if strings.EqualFold(method, TenderComplimentary) && p.Status == StatusCompleted {
				comp += p.Amount
				if reason, ok := p.PaymentData["reason"].(string); ok && reason != "" {
					compReason = reason
				}
			}
		}
	}
	switch {
	case comp > 0 && comp >= order.TotalAmount-0.01:
		scheme = "complimentary"
	case onAccount > 0 && onAccount >= order.TotalAmount-0.01:
		scheme = "credit"
	case onAccount > 0 || comp > 0:
		scheme = "mixed"
	}
	return
}

func (s *Service) signEtimsSync(ctx context.Context, order *ent.POSOrder) {
	if s.treasuryClient == nil || order == nil {
		return
	}
	// Complimentary (no-charge) sales carry no taxable supply — never transmitted, mirroring the
	// pos.sale.finalized subscriber's skip.
	if order.Metadata != nil {
		if comp, _ := order.Metadata["complimentary"].(bool); comp {
			return
		}
	}
	// Already fiscalised (re-entry / event beat us) — nothing to do.
	if order.EtimsInvoiceNumber != nil && *order.EtimsInvoiceNumber != "" {
		return
	}

	lines, err := s.client.POSOrderLine.Query().Where(posorderline.OrderID(order.ID)).All(ctx)
	if err != nil || len(lines) == 0 {
		return
	}
	items := make([]treasury.SignPOSSaleItem, 0, len(lines))
	for _, l := range lines {
		// Void-aware: post only the surviving (non-voided) quantity — mirrors publishSaleFinalized so
		// the sync and async eTIMS payloads match.
		effQty := l.Quantity
		if l.VoidedQty != nil {
			effQty = l.Quantity - *l.VoidedQty
		}
		if effQty <= 0 {
			continue
		}
		effTotal := l.TotalPrice
		if l.Quantity > 0 && effQty != l.Quantity {
			effTotal = (l.TotalPrice / l.Quantity) * effQty
		}
		it := treasury.SignPOSSaleItem{
			SKU:              l.Sku,
			Name:             l.Name,
			Quantity:         effQty,
			UnitPrice:        l.UnitPrice,
			TotalPrice:       effTotal,
			PriceIncludesTax: l.PriceIncludesTax,
			TaxCodeID:        l.TaxCodeID,
			TaxKRACode:       l.TaxKraCode,
		}
		if l.TaxRate != nil {
			it.TaxRate = *l.TaxRate
		}
		if l.TaxAmount != nil {
			ta := *l.TaxAmount
			if l.Quantity > 0 && effQty != l.Quantity {
				ta = ta * effQty / l.Quantity
			}
			it.TaxAmount = ta
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return
	}

	tenantSlug := ""
	if outlet, oErr := s.client.Outlet.Get(ctx, order.OutletID); oErr == nil {
		tenantSlug = outlet.TenantSlug
	}
	if tenantSlug == "" {
		return
	}

	scheme, _, _, _, saleTenders := s.resolveSaleSettlement(ctx, order)
	reqTenders := make([]treasury.SignPOSSaleTender, 0, len(saleTenders))
	for _, t := range saleTenders {
		reqTenders = append(reqTenders, treasury.SignPOSSaleTender{Type: t.Type, Amount: t.Amount})
	}
	fi, err := s.treasuryClient.SignPOSSale(ctx, tenantSlug, treasury.SignPOSSaleRequest{
		OrderID:       order.ID.String(),
		OrderNumber:   order.OrderNumber,
		TotalAmount:   order.TotalAmount,
		Currency:      order.Currency,
		OutletID:      order.OutletID.String(),
		Items:         items,
		SellingScheme: scheme,
		Tenders:       reqTenders,
	})
	if err != nil {
		s.log.Warn("etims: synchronous sign failed — falling back to async pos.sale.finalized",
			zap.String("order_id", order.ID.String()), zap.Error(err))
		return
	}
	if fi == nil { // tenant not eTIMS-activated — plain receipt, nothing to persist
		return
	}

	upd := s.client.POSOrder.UpdateOneID(order.ID).
		SetEtimsInvoiceNumber(fi.ReceiptNo).
		SetEtimsCuInvNo(fi.CuInvoiceNo).
		SetEtimsKraPin(fi.KraPin)
	if fi.DeviceSerial != "" {
		upd = upd.SetEtimsScuID(fi.DeviceSerial)
	}
	if fi.Signature != "" {
		upd = upd.SetEtimsRcptSign(fi.Signature)
	}
	if fi.QRURL != "" {
		upd = upd.SetEtimsQrCodeURL(fi.QRURL)
	}
	if _, uerr := upd.Save(ctx); uerr != nil {
		s.log.Warn("etims: persist synchronous fiscal identity failed",
			zap.String("order_id", order.ID.String()), zap.Error(uerr))
	}
	// Reflect on the in-memory order so the fan-out's receipt print carries the fiscal block.
	order.EtimsInvoiceNumber = &fi.ReceiptNo
	order.EtimsCuInvNo = &fi.CuInvoiceNo
	order.EtimsKraPin = &fi.KraPin
	if fi.DeviceSerial != "" {
		order.EtimsScuID = &fi.DeviceSerial
	}
	if fi.Signature != "" {
		order.EtimsRcptSign = &fi.Signature
	}
	if fi.QRURL != "" {
		order.EtimsQrCodeURL = &fi.QRURL
	}
	s.log.Info("etims: pos sale signed synchronously at checkout",
		zap.String("order_id", order.ID.String()), zap.String("cu_inv_no", fi.CuInvoiceNo))
	// PUSH the fiscal block to the terminal so the open receipt shows the KRA TIMS details
	// immediately, instead of the terminal polling the receipt endpoint until it lands.
	s.pushEtimsFiscalized(order)
}

// publishSaleFinalized emits pos.sale.finalized to the NATS outbox.
// treasury-api consumes this for ledger posting; inventory-api consumes it for stock backflush.
func (s *Service) publishSaleFinalized(ctx context.Context, order *ent.POSOrder) {
	if s.publisher == nil {
		return
	}

	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(order.ID)).
		WithModifiers().
		All(ctx)
	if err != nil {
		s.log.Warn("sale.finalized: failed to load lines", zap.String("order_id", order.ID.String()), zap.Error(err))
	}

	// Resolve modifier -> inventory modifier-option id once, so the backflush payload
	// can deduct modifier ingredient stock (previously dropped — modifier ingredients leaked).
	modifierOptionByID := map[uuid.UUID]*uuid.UUID{}
	for _, l := range lines {
		for _, m := range l.Edges.Modifiers {
			if _, seen := modifierOptionByID[m.ModifierID]; seen {
				continue
			}
			if mod, mErr := s.client.Modifier.Get(ctx, m.ModifierID); mErr == nil {
				modifierOptionByID[m.ModifierID] = mod.InventoryModifierOptionID
			} else {
				modifierOptionByID[m.ModifierID] = nil
			}
		}
	}

	// Resolve default warehouse from outlet setting for inventory backflush routing.
	warehouseID := ""
	outletSlug := ""
	if outlet, oErr := s.client.Outlet.Get(ctx, order.OutletID); oErr == nil {
		outletSlug = outlet.TenantSlug
		if setting, sErr := s.client.OutletSetting.Query().
			Where(outletsettingpredicate.OutletID(order.OutletID)).
			Only(ctx); sErr == nil && setting.DefaultWarehouseID != nil {
			warehouseID = setting.DefaultWarehouseID.String()
		}
	}

	// Resolve per-unit item COST so treasury can post per-sale Cost of Goods Sold
	// (DR 5400 / CR 1500). pos-api stores no cost on the order line; the authoritative
	// per-unit cost is the inventory-synced value cached on POSCatalogOverride.metadata["cost_price"]
	// (keyed by tenant + inventory_sku, see catalog/inventory_events.go and reports_profitability.go).
	// Resolved here as a SKU->cost map in one query. Missing cost => 0 (never blocks the sale).
	costBySKU, uomBySKU := s.resolveLineCosts(ctx, order.TenantID, lines)

	items := make([]map[string]any, 0, len(lines))
	costTotal := 0.0
	for _, l := range lines {
		// Void-aware: a line partially or fully voided after the order was sent (e.g. an item went
		// out of stock) was already subtracted from the header total. Its voided portion must NOT
		// reach treasury (revenue, VAT/eTIMS, COGS) or inventory backflush — otherwise the per-item
		// GL/eTIMS lines would over-state the sale versus the reduced cash receipt, and stock for a
		// voided item would be wrongly deducted. Post only the surviving (non-voided) quantity; skip
		// lines voided in full. See VoidOrderLine in handlers/orders_line_void.go.
		effQty := l.Quantity
		if l.VoidedQty != nil {
			effQty = l.Quantity - *l.VoidedQty
		}
		if effQty <= 0 {
			continue // fully voided — nothing to post or backflush for this line
		}
		// Scale money/cost/tax to the surviving quantity on a unit-price basis.
		effTotal := l.TotalPrice
		if l.Quantity > 0 && effQty != l.Quantity {
			effTotal = (l.TotalPrice / l.Quantity) * effQty
		}
		costAmount := costBySKU[l.Sku] // per-unit cost; 0 when not available
		lineCost := costAmount * effQty
		costTotal += lineCost
		item := map[string]any{
			"sku":         l.Sku,
			"name":        l.Name,
			"quantity":    effQty,
			"unit_price":  l.UnitPrice,
			"total_price": effTotal,
			// The item's inventory stock unit (synced via catalog events); "" when unknown.
			// inventory-api converts non-stock-unit quantities (incl. the bottle
			// content-per-unit bridge) before deducting.
			"uom_code":           uomBySKU[l.Sku],
			"price_includes_tax": l.PriceIncludesTax,
			// COGS support (additive): per-unit cost and line cost (cost x qty). 0 when unknown.
			"cost_amount": costAmount,
			"line_cost":   lineCost,
		}
		// Include pre-computed tax breakdown if available — treasury uses this verbatim for eTIMS.
		if l.TaxCodeID != "" {
			item["tax_code_id"] = l.TaxCodeID
		}
		if l.TaxKraCode != "" {
			item["tax_kra_code"] = l.TaxKraCode
		}
		if l.TaxRate != nil {
			item["tax_rate"] = *l.TaxRate
		}
		if l.TaxAmount != nil {
			// tax_amount is for the whole line (qty × unit tax); scale to the surviving quantity.
			taxAmt := *l.TaxAmount
			if l.Quantity > 0 && effQty != l.Quantity {
				taxAmt = taxAmt * effQty / l.Quantity
			}
			item["tax_amount"] = taxAmt
		}
		// Attach captured serial(s) so inventory flips the specific sold unit(s) to "sold" in its
		// per-unit serial registry (serial-tracked items: electronics, equipment).
		if l.SerialNumber != nil && *l.SerialNumber != "" {
			item["serials"] = []string{*l.SerialNumber}
		}
		// Attach modifiers so inventory can backflush their ingredient consumption.
		if len(l.Edges.Modifiers) > 0 {
			mods := make([]map[string]any, 0, len(l.Edges.Modifiers))
			for _, m := range l.Edges.Modifiers {
				mod := map[string]any{
					"modifier_id": m.ModifierID.String(),
					"name":        m.Name,
					// Deliberately NOT sending a "quantity" here — inventory-api resolves the
					// authoritative per-selection deduction amount from the option's own
					// deduction_qty. This field used to send the PARENT line's quantity, which
					// inventory-api's modifierConsumption then multiplied by the line quantity
					// AGAIN, double-counting every modifier with a stock-tracked SKU.
				}
				if optID := modifierOptionByID[m.ModifierID]; optID != nil {
					mod["inventory_modifier_option_id"] = optID.String()
				}
				mods = append(mods, mod)
			}
			item["modifiers"] = mods
		}
		items = append(items, item)
	}

	// Determine the SELLING SCHEME from the order's tenders so treasury routes the ledger correctly.
	// A credit ("on account") sale has ALREADY posted the amount to the customer's AR balance in
	// treasury (recordCreditSale); treasury's sale.finalized subscriber must therefore NOT also post
	// a cash receipt for it (that double-counted credit sales as cash). We surface the scheme, the
	// on-account amount, and a per-tender breakdown for accurate GL posting.
	// Shared with signEtimsSync so the async event and the sync ETR sign declare the identical
	// mode of payment (scheme + tender breakdown).
	sellingScheme, onAccountAmount, complimentaryAmount, complimentaryReason, saleTenders := s.resolveSaleSettlement(ctx, order)
	tenderBreakdown := make([]map[string]any, 0, len(saleTenders))
	for _, t := range saleTenders {
		tenderBreakdown = append(tenderBreakdown, map[string]any{
			"type":   t.Type,
			"amount": t.Amount,
			"status": t.Status,
		})
	}

	data := map[string]any{
		"order_id":     order.ID.String(),
		"order_number": order.OrderNumber,
		"tenant_id":    order.TenantID.String(),
		"tenant_slug":  outletSlug,
		"outlet_id":    order.OutletID.String(),
		"warehouse_id": warehouseID,
		"total_amount": order.TotalAmount,
		"currency":     order.Currency,
		"items":        items,
		// Sum of per-line cost (cost_amount x quantity) for the whole sale; used by treasury
		// to post Cost of Goods Sold (DR 5400 / CR 1500). 0 when no item carries a known cost.
		"cost_total": costTotal,
		// Selling scheme + tender breakdown for treasury GL routing (cash receipt vs AR vs
		// complimentary expense). complimentary_reason feeds the treasury journal description.
		"selling_scheme":       sellingScheme,
		"on_account_amount":    onAccountAmount,
		"complimentary_amount": complimentaryAmount,
		"complimentary_reason": complimentaryReason,
		"tenders":              tenderBreakdown,
		"customer_phone": func() string {
			if order.CustomerPhone != nil {
				return *order.CustomerPhone
			}
			return ""
		}(),
	}

	if err := s.publisher.PublishSaleFinalized(ctx, order.TenantID, data); err != nil {
		s.log.Warn("failed to publish pos.sale.finalized", zap.String("order_id", order.ID.String()), zap.Error(err))
	}

	// Stock backflush for the sale is handled EXCLUSIVELY by inventory-api's
	// pos.sale.finalized consumer (in-process, with BOM explosion) — see PublishSaleFinalized
	// above. We deliberately do NOT also call RecordConsumption here. The two paths each
	// deduct independently, so once the S2S client's request started succeeding (after the
	// order_id JSON-tag fix) every sold item was consumed twice (two consumptions per order).
	// backflushInventory is retained only for reference / non-sale direct backflush callers.
}

// resolveLineCosts returns SKU -> per-unit cost and SKU -> stock-unit (uom) maps for the
// sold lines, used to enrich the pos.sale.finalized payload with COGS figures and a real
// uom_code. Both are the inventory-synced values cached on POSCatalogOverride.metadata
// ("cost_price"/"uom", keyed by tenant + inventory_sku; see catalog/inventory_events.go).
// SKUs with no cached value are simply absent from the maps, so the caller reads 0/"" —
// a missing cost or unit must never block the sale.
func (s *Service) resolveLineCosts(ctx context.Context, tenantID uuid.UUID, lines []*ent.POSOrderLine) (map[string]float64, map[string]string) {
	costs := make(map[string]float64)
	uoms := make(map[string]string)
	if len(lines) == 0 {
		return costs, uoms
	}
	skus := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		if l.Sku == "" {
			continue
		}
		if _, ok := seen[l.Sku]; ok {
			continue
		}
		seen[l.Sku] = struct{}{}
		skus = append(skus, l.Sku)
	}
	if len(skus) == 0 {
		return costs, uoms
	}

	overrides, err := s.client.POSCatalogOverride.Query().
		Where(
			entcatalogoverride.TenantID(tenantID),
			entcatalogoverride.InventorySkuIn(skus...),
		).
		All(ctx)
	if err != nil {
		s.log.Warn("sale.finalized: failed to resolve item costs (defaulting to 0)",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return costs, uoms
	}
	for _, ov := range overrides {
		if ov.Metadata == nil {
			continue
		}
		switch v := ov.Metadata["cost_price"].(type) {
		case float64:
			costs[ov.InventorySku] = v
		case int:
			costs[ov.InventorySku] = float64(v)
		}
		if u, ok := ov.Metadata["uom"].(string); ok && u != "" {
			uoms[ov.InventorySku] = u
		}
	}
	return costs, uoms
}

// backflushInventory calls inventory-api to deduct stock for each sold item.
// Runs in a goroutine to avoid blocking the payment flow.
// Publishes pos.inventory.consumption.failed on error so a retry worker can re-attempt.
func (s *Service) backflushInventory(parent context.Context, order *ent.POSOrder, lines []*ent.POSOrderLine) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	items := make([]inventory.ConsumptionItem, 0, len(lines))
	for _, l := range lines {
		if l.Sku != "" {
			items = append(items, inventory.ConsumptionItem{
				SKU:      l.Sku,
				Quantity: float64(l.Quantity),
			})
		}
	}
	if len(items) == 0 {
		return
	}

	result, err := s.inventoryClient.RecordConsumption(ctx, order.TenantID.String(), inventory.ConsumptionRequest{
		OrderID: order.ID.String(),
		Items:   items,
	})
	if err != nil {
		s.log.Warn("inventory backflush failed",
			zap.String("order_id", order.ID.String()),
			zap.Error(err))
		if s.publisher != nil {
			_ = s.publisher.PublishInventoryConsumptionFailed(ctx, order.TenantID, map[string]any{
				"order_id":  order.ID.String(),
				"tenant_id": order.TenantID.String(),
				"items":     items,
				"error":     err.Error(),
			})
		}
		return
	}

	s.stampOrderLineLots(ctx, lines, result)
}

// stampOrderLineLots writes the FEFO/FIFO/LIFO-selected lot's number/expiry back onto each
// sold POSOrderLine (Phase 2 traceability — previously these fields existed on the schema but
// were never populated). When a SKU's quantity spans two lots at a draw boundary, the largest
// contribution wins for display purposes; ConsumptionLine (inventory-api) retains the full,
// accurate per-lot split regardless — this is a display simplification, not a data loss.
func (s *Service) stampOrderLineLots(ctx context.Context, lines []*ent.POSOrderLine, result *inventory.ConsumptionResult) {
	if result == nil || len(result.LotsConsumed) == 0 {
		return
	}
	bestBySKU := make(map[string]inventory.ConsumedLot, len(result.LotsConsumed))
	for _, lot := range result.LotsConsumed {
		if cur, ok := bestBySKU[lot.SKU]; !ok || lot.Quantity > cur.Quantity {
			bestBySKU[lot.SKU] = lot
		}
	}
	for _, l := range lines {
		lot, ok := bestBySKU[l.Sku]
		if !ok {
			continue
		}
		upd := s.client.POSOrderLine.UpdateOneID(l.ID).SetLotNumber(lot.LotNumber)
		if lot.ExpiryDate != nil {
			upd = upd.SetExpiryDate(*lot.ExpiryDate)
		}
		if _, err := upd.Save(ctx); err != nil {
			s.log.Warn("failed to stamp order line lot", zap.String("order_line_id", l.ID.String()), zap.Error(err))
		}
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// calcCommissions creates CommissionRecord entries for the order's creator (staff member)
// based on active CommissionRule definitions. Called after order status → completed.
// Errors are logged but never block the caller — commission calc is best-effort.
func (s *Service) calcCommissions(ctx context.Context, order *ent.POSOrder) {
	// Resolve the staff member from the order's user_id.
	staffMembers, err := s.client.StaffMember.Query().
		Where(staffmemberent.TenantID(order.TenantID), staffmemberent.UserID(order.UserID)).
		All(ctx)
	if err != nil || len(staffMembers) == 0 {
		return
	}
	staffMember := staffMembers[0]

	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(order.ID)).
		All(ctx)
	if err != nil || len(lines) == 0 {
		return
	}

	// Fetch all active rules for this tenant once.
	rules, err := s.client.CommissionRule.Query().
		Where(entcommissionrule.TenantID(order.TenantID), entcommissionrule.IsActive(true)).
		All(ctx)
	if err != nil || len(rules) == 0 {
		return
	}

	for _, line := range lines {
		// Find the most specific rule: staff+item > staff-only > item-only > global.
		var bestRule *ent.CommissionRule
		for _, r := range rules {
			staffMatch := r.StaffMemberID == nil || *r.StaffMemberID == staffMember.ID
			itemMatch := r.CatalogItemID == nil || *r.CatalogItemID == line.CatalogItemID
			if !staffMatch || !itemMatch {
				continue
			}
			if bestRule == nil {
				bestRule = r
				continue
			}
			// More specific wins (non-nil fields beat nil).
			bestScore, rScore := 0, 0
			if bestRule.StaffMemberID != nil {
				bestScore++
			}
			if bestRule.CatalogItemID != nil {
				bestScore++
			}
			if r.StaffMemberID != nil {
				rScore++
			}
			if r.CatalogItemID != nil {
				rScore++
			}
			if rScore > bestScore {
				bestRule = r
			}
		}
		if bestRule == nil {
			continue
		}

		var rate, amount float64
		switch bestRule.RuleType {
		case "flat":
			if bestRule.FlatAmount != nil {
				amount = *bestRule.FlatAmount
			}
		default: // "percentage"
			if bestRule.Percentage != nil {
				rate = *bestRule.Percentage
				amount = line.TotalPrice * rate / 100
			}
		}
		if amount <= 0 {
			continue
		}

		creator := s.client.CommissionRecord.Create().
			SetTenantID(order.TenantID).
			SetStaffMemberID(staffMember.ID).
			SetOrderID(order.ID).
			SetOrderLineID(line.ID).
			SetServiceSku(line.Sku).
			SetSaleAmount(line.TotalPrice).
			SetCommissionRate(rate).
			SetCommissionAmount(amount).
			SetStatus("pending")
		if _, err := creator.Save(ctx); err != nil {
			s.log.Warn("commission record create failed",
				zap.String("order_id", order.ID.String()),
				zap.String("staff_id", staffMember.ID.String()),
				zap.Error(err))
		}
	}
}
