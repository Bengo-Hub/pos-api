package payments

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// ============================================================================================
// Treasury → POS AR reconciliation.
//
// Treasury's CustomerBalance is the AUTHORITATIVE operational AR for a customer (it owns invoices,
// opening balances AND POS credit-sale debt). When money is received against that balance ANYWHERE
// — the treasury Receive-Payment modal, a store-credit application, a manual adjustment, or the POS
// settle-credit path — treasury decrements the aggregate balance. Historically POS never learned of
// it, so a customer cleared to 0 in treasury kept showing per-order "Sell Due" forever on POS.
//
// This closes that gap: on every treasury.customer.balance_updated event, POS reconciles the
// customer's own OPEN credit orders DOWN to treasury's authoritative outstanding figure, FIFO
// oldest-first. It is:
//   - reduce-only: never fabricates debt. If treasury's outstanding ≥ POS's open total, it no-ops
//     (the surplus is non-POS AR — invoices/opening balance — which POS must not touch).
//   - idempotent / loop-safe: it reconciles TO a target, so redelivery no-ops, and POS's own
//     settle-credit (which records its local payment BEFORE calling treasury) means the echoed
//     event finds POS already at target → no double settlement.
//   - a single choke point: owed math is orders.ComputeSettlement, the same the UI reads.
// ============================================================================================

// ReconcileReport summarizes one reconciliation pass (used by the dry-run data-heal endpoint too).
type ReconcileReport struct {
	CustomerKey    string             `json:"customer_key"`
	TargetOutstanding float64         `json:"target_outstanding"`
	POSOpenBefore  float64            `json:"pos_open_before"`
	POSOpenAfter   float64            `json:"pos_open_after"`
	AmountSettled  float64            `json:"amount_settled"`
	OrdersTouched  []ReconciledOrder  `json:"orders_touched"`
	DryRun         bool               `json:"dry_run"`
}

// ReconciledOrder is one order a reconciliation pass settled (or would settle, in dry-run).
type ReconciledOrder struct {
	OrderID     uuid.UUID `json:"order_id"`
	OrderNumber string    `json:"order_number"`
	DueBefore   float64   `json:"due_before"`
	Applied     float64   `json:"applied"`
	DueAfter    float64   `json:"due_after"`
}

// ReconcileParams targets a customer's open credit orders and the treasury outstanding to match.
type ReconcileParams struct {
	TenantID uuid.UUID
	// CrmContactID / CustomerIdentifier are the treasury customer keys carried by the balance event
	// (either may be empty; at least one must be set). PhoneMatch additionally matches raw phone.
	CrmContactID       string
	CustomerIdentifier string
	// TargetOutstanding is treasury's authoritative remaining debit for this customer.
	TargetOutstanding float64
	PaymentMethod     string // stamped on the reconcile POSPayment (event's method, or "ar_receipt")
	Reference         string // treasury reference for audit/idempotency on payment_data
	DryRun            bool
}

// completedReturnsTotal sums the settled (completed) sell-returns for an order — the amount that has
// already reduced the customer's real debt (refund/credit-note/offset). Shared owed-math input.
func (s *Service) completedReturnsTotal(ctx context.Context, orderID uuid.UUID) (float64, error) {
	rets, err := s.client.POSReturn.Query().
		Where(posreturn.OrderID(orderID), posreturn.StatusEQ(posreturn.StatusCompleted)).
		All(ctx)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, r := range rets {
		total += r.RefundAmount
	}
	return total, nil
}

// ReconcileCustomerOrders settles a customer's open POS credit orders down to treasury's
// authoritative outstanding figure (reduce-only, FIFO oldest-first, idempotent).
func (s *Service) ReconcileCustomerOrders(ctx context.Context, p ReconcileParams) (*ReconcileReport, error) {
	key := p.CrmContactID
	if key == "" {
		key = p.CustomerIdentifier
	}
	report := &ReconcileReport{
		CustomerKey:       key,
		TargetOutstanding: round2(p.TargetOutstanding),
		DryRun:            p.DryRun,
		OrdersTouched:     []ReconciledOrder{},
	}
	if p.CrmContactID == "" && strings.TrimSpace(p.CustomerIdentifier) == "" {
		return report, nil // nothing to key against
	}

	// Candidate set: this tenant's committed on-account orders (credit sales). Bounded — a tenant's
	// open credit debtors are few — so an in-Go key match + owed compute is cheap and avoids
	// duplicating treasury's key-resolution rules in a SQL predicate.
	candidates, err := s.client.POSOrder.Query().
		Where(
			posorder.TenantID(p.TenantID),
			posorder.StatusNotIn(orders.StatusVoided, orders.StatusCancelled, orders.StatusRefunded, orders.StatusDraft),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile: load candidate orders: %w", err)
	}

	// Match each order to the treasury customer via the SAME key resolution the credit sale used
	// (crm contact → phone → staff), caching phone→crm so we resolve each distinct phone once.
	crmByPhone := map[string]string{}
	type openOrder struct {
		order *ent.POSOrder
		due   float64
	}
	var open []openOrder
	var posOpen float64
	for _, o := range candidates {
		if !orders.IsOnAccount(o.Metadata) {
			continue
		}
		if !s.orderMatchesCustomer(ctx, p.TenantID, o, p.CrmContactID, p.CustomerIdentifier, crmByPhone) {
			continue
		}
		cr, rerr := s.completedReturnsTotal(ctx, o.ID)
		if rerr != nil {
			s.log.Warn("reconcile: completed-returns lookup failed", zap.String("order", o.OrderNumber), zap.Error(rerr))
		}
		due := orders.ComputeSettlement(o, cr).AmountDue
		if due <= 0.01 {
			continue
		}
		open = append(open, openOrder{order: o, due: due})
		posOpen += due
	}
	report.POSOpenBefore = round2(posOpen)
	report.POSOpenAfter = round2(posOpen)

	// Reduce-only: only settle the surplus of POS-open over treasury's authoritative outstanding.
	// If treasury still shows ≥ what POS knows about, the difference is non-POS AR — never fabricate.
	reduce := posOpen - p.TargetOutstanding
	if reduce <= 0.01 || len(open) == 0 {
		return report, nil
	}

	// FIFO oldest-first so the customer's earliest debts clear first (matches AR aging + how a
	// cashier/accountant intuitively applies a payment).
	sort.Slice(open, func(i, j int) bool { return open[i].order.CreatedAt.Before(open[j].order.CreatedAt) })

	method := canonicalTenderMethod(p.PaymentMethod)
	if method == "" || strings.EqualFold(method, TenderOnAccount) {
		method = "ar_receipt"
	}
	remaining := reduce
	var settled float64
	for _, oo := range open {
		if remaining <= 0.01 {
			break
		}
		apply := oo.due
		if apply > remaining {
			apply = remaining
		}
		rec := ReconciledOrder{
			OrderID:     oo.order.ID,
			OrderNumber: oo.order.OrderNumber,
			DueBefore:   round2(oo.due),
			Applied:     round2(apply),
			DueAfter:    round2(oo.due - apply),
		}
		if !p.DryRun {
			if err := s.applyReconcileSettlement(ctx, oo.order, apply, method, p.Reference); err != nil {
				s.log.Error("reconcile: settle order failed", zap.String("order", oo.order.OrderNumber), zap.Error(err))
				continue // best-effort per order; a redelivery/next event retries the rest
			}
		}
		settled += apply
		remaining -= apply
		report.OrdersTouched = append(report.OrdersTouched, rec)
	}
	report.AmountSettled = round2(settled)
	report.POSOpenAfter = round2(posOpen - settled)
	if !p.DryRun {
		s.log.Info("reconcile: settled POS credit orders to treasury balance",
			zap.String("customer_key", key),
			zap.Float64("target", p.TargetOutstanding),
			zap.Float64("settled", settled),
			zap.Int("orders", len(report.OrdersTouched)))
	}
	return report, nil
}

// applyReconcileSettlement records a reconcile payment on an order (NOT on_account, so it counts in
// paid_total) and re-derives paid_total. Marked ar_reconciled so it is auditable and never mistaken
// for a real till collection; carries the treasury reference for traceability/idempotency.
func (s *Service) applyReconcileSettlement(ctx context.Context, order *ent.POSOrder, amount float64, method, reference string) error {
	currency := order.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}
	if _, err := s.client.POSPayment.Create().
		SetOrderID(order.ID).
		SetTenderID(uuid.Nil).
		SetAmount(amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{
			"method":              method,
			"ar_reconciled":       true,
			"treasury_reference":  reference,
			"reconcile_source":    "treasury_balance_updated",
		}).
		SetNillableExternalReference(nilIfEmpty(reference)).
		Save(ctx); err != nil {
		return fmt.Errorf("record reconcile payment: %w", err)
	}
	if _, _, err := s.RecomputePaidTotal(ctx, order.ID); err != nil {
		return err
	}
	// Stamp settlement time when the order is now fully collected (mirrors credit_settlement).
	collected, _, _ := s.RecomputePaidTotal(ctx, order.ID)
	cr, _ := s.completedReturnsTotal(ctx, order.ID)
	if order.TotalAmount-collected-cr <= 0.01 {
		meta := order.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		if _, done := meta["credit_settled_at"]; !done {
			meta["credit_settled_at"] = time.Now().Format(time.RFC3339)
			if merr := s.client.POSOrder.UpdateOneID(order.ID).SetMetadata(meta).Exec(ctx); merr != nil {
				s.log.Warn("reconcile: stamp settled metadata failed", zap.Error(merr))
			}
		}
	}
	return nil
}

// orderMatchesCustomer reports whether an order belongs to the treasury customer identified by the
// event's crm_contact_id / customer_identifier, using the SAME resolution the credit sale used
// (crm contact from phone → raw phone → staff:key). crmByPhone caches phone→crm within a pass.
func (s *Service) orderMatchesCustomer(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, crmContactID, identifier string, crmByPhone map[string]string) bool {
	phone := ""
	if order.CustomerPhone != nil {
		phone = strings.TrimSpace(*order.CustomerPhone)
	}
	// Staff credit sale: keyed staff:<id> in treasury; matches on customer_identifier.
	if phone == "" {
		if staffID, _, isStaff := staffCreditFromOrderParty(order); isStaff {
			return identifier != "" && identifier == "staff:"+staffID.String()
		}
		return false
	}
	// Phone-keyed treasury balance (crm didn't resolve at sale time).
	if identifier != "" && strings.EqualFold(identifier, phone) {
		return true
	}
	if crmContactID == "" {
		return false
	}
	crm, ok := crmByPhone[phone]
	if !ok {
		crm = s.ResolveCrmContactID(ctx, tenantID, phone)
		crmByPhone[phone] = crm
	}
	return crm != "" && crm == crmContactID
}
