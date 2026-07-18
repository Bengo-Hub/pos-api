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

// SettleCreditPayment applies a collected payment to a completed on-account sale.
func (s *Service) SettleCreditPayment(ctx context.Context, req SettleCreditRequest) (*SettleCreditResult, error) {
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
	if on, _ := order.Metadata["on_account"].(bool); !on {
		return nil, fmt.Errorf("payments: order %s is not an on-account (credit) sale", order.OrderNumber)
	}

	// Outstanding = total − money ACTUALLY collected (paid_total excludes on-account rows).
	collected, _, err := s.RecomputePaidTotal(ctx, order.ID)
	if err != nil {
		return nil, err
	}
	outstanding := order.TotalAmount - collected
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
	if _, err := s.client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(req.TenderID).SetAmount(req.Amount).
		SetCurrency(currency).SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{"method": req.TenderMethod, "credit_settlement": true}).
		SetNillableExternalReference(nilIfEmpty(req.ExternalRef)).
		Save(ctx); err != nil {
		return nil, fmt.Errorf("payments: record credit settlement: %w", err)
	}
	collectedAfter, _, _ := s.RecomputePaidTotal(ctx, order.ID)
	outstandingAfter := order.TotalAmount - collectedAfter
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
// the customer's phone (same resolution recordCreditSale used), falling back to the raw phone,
// falling back to the staff key for staff credit sales.
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
	if crmID := s.ResolveCrmContactID(ctx, tenantID, phone); crmID != "" {
		return crmID
	}
	return phone
}
