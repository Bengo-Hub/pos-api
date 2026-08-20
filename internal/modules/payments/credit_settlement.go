package payments

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// SettleCreditRequest records money collected AGAINST an existing on-account (credit) sale.
// This is deliberately a separate path from CreatePaymentIntent: the sale is already
// finalized (GL revenue posted as Dr AR, stock deducted, receipt issued) — running the
// normal payment path again would double-post the sale downstream. A credit settlement
// only (a) adds a collected POSPayment row so paid_total/payment_status read correctly,
// and (b) posts the AR receipt (Dr Cash / Cr AR) in treasury via S2S.
type SettleCreditRequest struct {
	TenantID     uuid.UUID
	TenantSlug   string
	OrderID      uuid.UUID
	TenderID     uuid.UUID
	TenderMethod string  // cash | mpesa | mpesa_manual | card_manual | bank | cheque | paystack…
	Amount       float64 // 0 → settle the full outstanding balance
	ExternalRef  string  // M-Pesa code / cheque no. for manual methods
	Currency     string
	// OccurredAt is when the money actually changed hands (e.g. cash handed over yesterday but
	// only entered today) — nil defaults to now. Backdating support, not postdating: a future
	// value is rejected by resolveOccurredAt.
	OccurredAt *time.Time
}

// SettleCreditResult reports what was applied and where the order now stands.
type SettleCreditResult struct {
	AmountApplied    float64 `json:"amount_applied"`
	OutstandingAfter float64 `json:"outstanding_after"`
	PaymentStatus    string  `json:"payment_status"` // paid | partial
	// TreasurySynced is false when the local collection recorded but the treasury AR receipt
	// failed (network) — the customer's treasury balance still shows the debt until the
	// payment is re-recorded from the treasury Customers page.
	TreasurySynced bool `json:"treasury_synced"`
}

// resolveOccurredAt validates and defaults a user-supplied "when did this payment actually
// happen" timestamp: nil defaults to now; a small clock-skew allowance is given, but anything
// meaningfully in the future is rejected — this is backdating support, not postdating.
func resolveOccurredAt(t *time.Time) (time.Time, error) {
	if t == nil || t.IsZero() {
		return time.Now(), nil
	}
	if t.After(time.Now().Add(5 * time.Minute)) {
		return time.Time{}, fmt.Errorf("payments: occurred_at cannot be in the future")
	}
	return *t, nil
}

// SettleCreditPayment applies a collected payment to a completed on-account sale.
func (s *Service) SettleCreditPayment(ctx context.Context, req SettleCreditRequest) (*SettleCreditResult, error) {
	occurredAt, err := resolveOccurredAt(req.OccurredAt)
	if err != nil {
		return nil, err
	}
	req.TenderMethod = canonicalTenderMethod(req.TenderMethod)
	if strings.EqualFold(req.TenderMethod, TenderOnAccount) || strings.EqualFold(req.TenderMethod, TenderComplimentary) {
		return nil, fmt.Errorf("payments: %s is not a settlement method", req.TenderMethod)
	}

	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(req.OrderID), posorder.TenantID(req.TenantID)).
		WithPayments().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: order not found: %w", err)
	}
	if order.Status == orders.StatusCancelled || order.Status == orders.StatusVoided || order.Status == orders.StatusRefunded {
		return nil, fmt.Errorf("payments: cannot settle a %s order", order.Status)
	}
	// A genuine on-account sale is ALWAYS orders.StatusCompleted — recordCreditSale stamps
	// on_account then completes the order in the same call (extending credit IS the settlement
	// event, even though no cash moved yet). So a still-draft/open/pending_payment order can never
	// be a real credit sale, regardless of what its metadata/payment-status label claims — this is
	// the real security boundary behind the frontend's identical gate (SalesActionsMenu), closing
	// the exact confusion that let a still-unfinished retail cart (boi-enterprises order 000278)
	// reach this far and fail confusingly on the on-account check below instead of here.
	if order.Status != orders.StatusCompleted {
		return nil, fmt.Errorf("payments: order %s is not a completed sale", order.OrderNumber)
	}
	if on, _ := order.Metadata["on_account"].(bool); !on {
		return nil, fmt.Errorf("payments: order %s is not an on-account (credit) sale", order.OrderNumber)
	}

	// Outstanding = total − money ACTUALLY collected − settled returns (paid_total excludes
	// on-account rows; a COMPLETED return already reduced the customer's real debt but never
	// touches paid_total — see orders.ComputeSettlement, the one owed-amount definition). Without
	// netting the return here, a credit sale with a completed partial return could still be
	// "settled" against its full pre-return balance, overcollecting from the customer.
	collected, _, err := s.RecomputePaidTotal(ctx, order.ID)
	if err != nil {
		return nil, err
	}
	completedReturns, rerr := s.completedReturnsTotal(ctx, order.ID)
	if rerr != nil {
		s.log.Warn("credit settlement: completed-returns lookup failed", zap.String("order", order.OrderNumber), zap.Error(rerr))
	}
	outstanding := order.TotalAmount - collected - completedReturns
	if outstanding <= 0.01 {
		return nil, fmt.Errorf("payments: order %s has no outstanding credit balance", order.OrderNumber)
	}
	if req.Amount <= 0 {
		req.Amount = outstanding
	}
	if req.Amount > outstanding+0.01 {
		s.log.Warn("credit settlement exceeds outstanding — clamping",
			zap.String("order_id", order.ID.String()),
			zap.Float64("requested", req.Amount), zap.Float64("outstanding", outstanding))
		req.Amount = outstanding
	}
	currency := req.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}

	// Local collection row first: the till has the money in hand; treasury sync below is
	// surfaced (never silently dropped) but must not lose the collected cash record.
	//
	// Locked inside a transaction: two concurrent settlements against the same credit order
	// (settled from two surfaces at once, or a retried request) previously both used the
	// outstanding balance computed above and both created a completed payment row against it,
	// overcollecting from the customer. ForUpdate() blocks a second racing request until the
	// first commits, so its re-check sees the first settlement already landed and clamps.
	tx, txErr := s.client.Tx(ctx)
	if txErr != nil {
		return nil, fmt.Errorf("payments: start transaction: %w", txErr)
	}
	if _, err := tx.POSOrder.Query().Where(posorder.ID(order.ID), posorder.TenantID(req.TenantID)).ForUpdate().Only(ctx); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("payments: lock order: %w", err)
	}
	completedRows, err := tx.POSPayment.Query().
		Where(pospayment.OrderID(order.ID), pospayment.Status(StatusCompleted)).
		All(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("payments: re-check outstanding: %w", err)
	}
	var freshCollected float64
	for _, p := range completedRows {
		method, _ := p.PaymentData["method"].(string)
		if !strings.EqualFold(method, TenderOnAccount) {
			freshCollected += p.Amount
		}
	}
	freshOutstanding := order.TotalAmount - freshCollected - completedReturns
	if freshOutstanding <= 0.01 {
		_ = tx.Rollback()
		return nil, fmt.Errorf("payments: order %s has no outstanding credit balance", order.OrderNumber)
	}
	if req.Amount > freshOutstanding+0.01 {
		req.Amount = freshOutstanding
	}
	paymentData := map[string]any{"method": req.TenderMethod, "credit_settlement": true}
	if req.OccurredAt != nil {
		// Backdated — leave a breadcrumb of when this was actually entered, since occurred_at
		// itself is now user-editable and no longer doubles as a system-record-time audit trail.
		paymentData["backdated"] = true
		paymentData["recorded_at"] = time.Now().Format(time.RFC3339)
	}
	if _, err := tx.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(req.TenderID).SetAmount(req.Amount).
		SetCurrency(currency).SetStatus(StatusCompleted).
		SetOccurredAt(occurredAt).
		SetPaymentData(paymentData).
		SetNillableExternalReference(nilIfEmpty(req.ExternalRef)).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("payments: record credit settlement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("payments: commit credit settlement: %w", err)
	}
	collectedAfter, _, _ := s.RecomputePaidTotal(ctx, order.ID)
	outstandingAfter := order.TotalAmount - collectedAfter - completedReturns
	if outstandingAfter < 0 {
		outstandingAfter = 0
	}

	// Fully collected → stamp settlement time (the overdue badge derives paid from
	// paid_total, this is for the audit trail/statement).
	if outstandingAfter <= 0.01 {
		meta := order.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		meta["credit_settled_at"] = time.Now().Format(time.RFC3339)
		if merr := s.client.POSOrder.UpdateOneID(order.ID).SetMetadata(meta).Exec(ctx); merr != nil {
			s.log.Warn("credit settlement: stamp metadata failed", zap.Error(merr))
		}
	}

	// Treasury AR receipt (Dr Cash / Cr AR + CustomerBalance decrement) — same key
	// resolution the credit sale used, so the receipt lands on the row that was debited.
	synced := false
	if s.treasuryClient != nil {
		key := s.creditSettlementKey(ctx, req.TenantID, order)
		if key == "" {
			s.log.Warn("credit settlement: no customer key on order — treasury AR not decremented",
				zap.String("order", order.OrderNumber))
		} else if _, terr := s.treasuryClient.RecordARPayment(ctx, req.TenantSlug, key, treasury.ARPaymentRequest{
			Amount:        req.Amount,
			PaymentMethod: req.TenderMethod,
			Reference:     order.OrderNumber,
			PaidAt:        &occurredAt,
			OutletID:      order.OutletID.String(),
		}); terr != nil {
			s.log.Error("credit settlement: treasury AR receipt failed — settle from treasury Customers page",
				zap.String("order", order.OrderNumber), zap.Error(terr))
		} else {
			synced = true
		}
	}

	status := "partial"
	if outstandingAfter <= 0.01 {
		status = "paid"
	}
	return &SettleCreditResult{
		AmountApplied:    req.Amount,
		OutstandingAfter: outstandingAfter,
		PaymentStatus:    status,
		TreasurySynced:   synced,
	}, nil
}

// creditSettlementKey resolves the treasury AR customer key for an order: the CRM contact of
// the customer's phone (same resolve-or-create resolution recordCreditSale used to open the
// credit sale in the first place), falling back to the raw phone, falling back to the staff key
// for staff credit sales.
//
// Was ResolveCrmContactID (cached-loyalty-only, no marketflow fallback) until 2026-08-15 — a
// customer with no loyalty enrollment resolved to "", so this fell back to a bare phone. When the
// original credit sale had already landed on a crm-linked row with no customer_identifier set
// (the common case once contact resolution is fixed to prefer the real CRM id), a phone-keyed
// RecordARPayment call can't match it: the payment silently fails to post (only logged, never
// surfaced or retried — see SettleCreditPayment's TreasurySynced=false path), leaving the
// customer's treasury balance showing them still owing money they already paid. Confirmed live:
// boi-enterprises order 000278 (MR OKELO TORORO) — paid_total=215000, credit_settled_at stamped,
// but zero ar_receipt ever reached treasury and balance_due stayed at 215000.
func (s *Service) creditSettlementKey(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder) string {
	phone := ""
	if order.CustomerPhone != nil {
		phone = strings.TrimSpace(*order.CustomerPhone)
	}
	if phone == "" {
		if staffID, _, isStaff := staffCreditFromOrderParty(order); isStaff {
			return "staff:" + staffID.String()
		}
		return ""
	}
	name := ""
	if order.CustomerName != nil {
		name = *order.CustomerName
	}
	if crmID := s.ResolveOrCreateCrmContactID(ctx, tenantID, phone, name); crmID != "" {
		return crmID
	}
	return phone
}
