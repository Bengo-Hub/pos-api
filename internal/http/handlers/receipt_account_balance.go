package handlers

import (
	"context"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/printing"
)

// ensureCustomerAccountBalance best-effort resolves the customer's OVERALL treasury AR position
// (distinct from the order-scoped BalanceDue) and stamps it onto the receipt view — mirrors
// ensureEtimsFiscal's shape: nil-guarded, logs at Debug on failure, NEVER blocks or fails the
// receipt. Shown regardless of whether THIS sale was cash or credit — a cash-paying customer who
// separately holds stored credit still sees it. Skipped entirely for a walk-in sale (no phone, or
// the "Walk-in Customer" ghost) since that identity holds no reconcilable balance, and whenever
// the resolved balance is exactly zero (nothing meaningful to disclose).
func (h *ReceiptHandler) ensureCustomerAccountBalance(ctx context.Context, tenantSlug string, order *ent.POSOrder, view *printing.ReceiptView) {
	if h.treasury == nil || order == nil || tenantSlug == "" {
		return
	}
	phone := ""
	if order.CustomerPhone != nil {
		phone = strings.TrimSpace(*order.CustomerPhone)
	}
	name := ""
	if order.CustomerName != nil {
		name = strings.TrimSpace(*order.CustomerName)
	}
	if phone == "" || strings.EqualFold(name, "walk-in customer") || strings.EqualFold(name, "walk in customer") {
		return
	}

	key := phone
	if h.resolveCrmContact != nil {
		if crmID := h.resolveCrmContact(ctx, order.TenantID, phone); crmID != "" {
			key = crmID
		}
	}

	terms, err := h.treasury.GetCreditTerms(ctx, tenantSlug, key)
	if err != nil {
		h.log.Debug("receipt: customer account-balance lookup failed", zap.Error(err))
		return
	}
	if terms == nil {
		return
	}
	balance, _ := strconv.ParseFloat(terms.BalanceDue, 64)
	if balance > -0.005 && balance < 0.005 {
		return // settled — nothing meaningful to disclose
	}

	label := "Amount Owing"
	if balance < 0 {
		label = "Store Credit Available"
		balance = -balance
	}
	view.CustomerAccountBalance = &balance
	view.CustomerAccountBalanceLabel = label
}
