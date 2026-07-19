package payments

import (
	"context"
	"fmt"
	"strings"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// applyCustomerCreditTender settles (part of) an order using the customer's EXISTING stored
// credit (a negative treasury AR balance_due) — the inverse-direction sibling of recordCreditSale,
// which only ever CREATES a debt. Requires a real, selected customer with a phone number (same
// requirement as a credit sale — the shared "Walk-in" ghost can't hold reconcilable credit).
// Treasury enforces the available-credit cap; pos-api never duplicates that check. Settles
// immediately like cash (no deferred settlement), so this composes with any other tender in a
// split/mixed payment.
func (s *Service) applyCustomerCreditTender(ctx context.Context, order *ent.POSOrder, req RecordPaymentRequest, currency string) (*CreateIntentResult, error) {
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
	trimmedName := strings.TrimSpace(name)
	if strings.TrimSpace(phone) == "" ||
		strings.EqualFold(trimmedName, "walk-in customer") || strings.EqualFold(trimmedName, "walk in customer") {
		return nil, fmt.Errorf("payments: applying stored credit requires a selected customer with a phone number")
	}

	// Resolve the canonical AR key the SAME way a credit sale does, so the applied credit nets
	// against the identical treasury CustomerBalance row a return or credit sale would touch.
	crmContactID := s.ResolveCrmContactID(ctx, req.TenantID, phone)
	key := crmContactID
	if key == "" {
		key = phone
	}

	if _, err := s.treasuryClient.ApplyCustomerCredit(ctx, req.TenantSlug, key, treasury.ApplyCreditRequest{
		Amount:     req.Amount,
		POSOrderID: order.ID.String(),
		Reference:  order.OrderNumber,
		UserID:     order.UserID.String(),
	}); err != nil {
		return nil, fmt.Errorf("payments: apply customer credit rejected: %w", err)
	}

	if _, err := s.client.POSPayment.Create().
		SetOrderID(req.OrderID).
		SetTenderID(req.TenderID).
		SetAmount(req.Amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{"method": TenderCustomerCredit}).
		SetNillableExternalReference(nilIfEmpty(order.OrderNumber)).
		Save(ctx); err != nil {
		return nil, fmt.Errorf("payments: record customer-credit payment: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order)
	return &CreateIntentResult{IsCash: true}, nil
}
