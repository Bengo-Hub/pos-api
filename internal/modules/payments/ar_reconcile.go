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
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
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
	CustomerKey       string            `json:"customer_key"`
	TargetOutstanding float64           `json:"target_outstanding"`
	POSOpenBefore     float64           `json:"pos_open_before"`
	POSOpenAfter      float64           `json:"pos_open_after"`
	AmountSettled     float64           `json:"amount_settled"`
	OrdersTouched     []ReconciledOrder `json:"orders_touched"`
	DryRun            bool              `json:"dry_run"`
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

// completedReturnsTotal sums the settled (completed) sell-returns for an order that STILL need
// manually netting against the order's own total — the amount that has already reduced the
// customer's real debt via a channel treasury never learns about (cash/mpesa/bank/cheque/
// store_credit). offset_invoice-channel returns are deliberately EXCLUDED: that channel reduces
// the customer's treasury CustomerBalance directly, and ReconcileCustomerOrders (below, this same
// file) — or the identically-scoped handlers.returnsRollupFor for the list/detail/report read
// paths — folds it into this order's own paid_total once the resulting balance_updated event
// lands. Including it here too would double-subtract the same return (confirmed live 2026-08-06,
// see orders/settlement.go's doc comment for the exact numbers) — this shared helper is exactly
// the second of the two call paths that bug hit (the first being the list/detail rollup).
func (s *Service) completedReturnsTotal(ctx context.Context, orderID uuid.UUID) (float64, error) {
	rets, err := s.client.POSReturn.Query().
		Where(posreturn.OrderID(orderID), posreturn.StatusEQ(posreturn.StatusCompleted)).
		All(ctx)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, r := range rets {
		if r.RefundChannel != nil && *r.RefundChannel == posreturn.RefundChannelOffsetInvoice {
			continue
		}
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
	// matchedAll is every on-account order belonging to this customer regardless of current due —
	// unlike open (due>0.01 only), a fully phantom-paid order (due==0 only because of a PRIOR
	// ar_reconciled marker payment) must still be visible to unreconcilePhantomPayments below, or a
	// treasury-side void that reinstates its debt could never find the phantom row to undo.
	var matchedAll []*ent.POSOrder
	var posOpen float64
	for _, o := range candidates {
		if !orders.IsOnAccount(o.Metadata) {
			continue
		}
		if !s.orderMatchesCustomer(ctx, p.TenantID, o, p.CrmContactID, p.CustomerIdentifier, crmByPhone) {
			continue
		}
		matchedAll = append(matchedAll, o)
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
		// Mirror-image case: treasury's outstanding is now HIGHER than POS's own open total — the
		// signature of a treasury-side AR receipt VOID reinstating debt a PRIOR reconcile pass on
		// this exact function already phantom-paid down (VoidARReceipt republishes
		// customer.balance_updated with reference "VOID-<original reference>"). Never a blind
		// guess: only ever undoes a phantom payment whose own treasury_reference exactly matches
		// the voided receipt's original reference — see unreconcilePhantomPayments' doc comment.
		if increase := p.TargetOutstanding - posOpen; increase > 0.01 {
			if uerr := s.unreconcilePhantomPayments(ctx, matchedAll, increase, p.Reference, p.DryRun, report); uerr != nil {
				s.log.Warn("reconcile: unreconcile phantom payments failed", zap.Error(uerr))
			}
		}
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
			"method":             method,
			"ar_reconciled":      true,
			"treasury_reference": reference,
			"reconcile_source":   "treasury_balance_updated",
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

// unreconcilePhantomPayments reverses a PRIOR ar_reconciled marker payment (applyReconcileSettlement,
// above) when treasury's outstanding has risen back above what POS currently shows as collected via
// that phantom row — the mirror-image of the reduce-only settle in ReconcileCustomerOrders, needed
// for exactly one real-world case: a treasury-side AR receipt VOID (arpa.VoidARReceipt's negative-
// paidDelta ProjectInvoiceAR call) reinstates the customer's debt and republishes
// customer.balance_updated with reference "VOID-<the voided receipt's own original reference>".
// Before this existed, that event hit the reduce-only branch's no-op guard (treasury's outstanding
// now exceeds POS's open total — indistinguishable, to that branch, from a fresh invoice/opening-
// balance bump that is genuinely none of POS's business) and the phantom payment this exact
// mechanism created when the (now-voided) receipt first landed stayed on the order FOREVER — a
// confirmed live gap: voiding a duplicate payment in treasury left the matching POS order's balance
// due completely unchanged. Deliberately narrow and precise rather than a blind LIFO guess: this
// ONLY ever touches a payment whose own stamped treasury_reference exactly equals the voided
// receipt's original reference (after stripping the "VOID-" prefix VoidARReceipt always adds) — a
// generic "treasury shows more debt than POS" gap with no such match is left untouched, exactly like
// the reduce-only path leaves a genuine non-POS AR surplus untouched.
func (s *Service) unreconcilePhantomPayments(ctx context.Context, matchedOrders []*ent.POSOrder, increase float64, reference string, dryRun bool, report *ReconcileReport) error {
	if !strings.HasPrefix(reference, "VOID-") {
		return nil // no voided-receipt reference to precisely match against — never guess
	}
	matchRef := strings.TrimPrefix(reference, "VOID-")
	if matchRef == "" {
		return nil
	}
	remaining := increase
	for _, o := range matchedOrders {
		if remaining <= 0.01 {
			break
		}
		pays, err := s.client.POSPayment.Query().
			Where(entpospayment.OrderID(o.ID), entpospayment.StatusEQ(StatusCompleted)).
			All(ctx)
		if err != nil {
			s.log.Warn("unreconcile: load payments failed", zap.String("order", o.OrderNumber), zap.Error(err))
			continue
		}
		for _, p := range pays {
			if remaining <= 0.01 {
				break
			}
			reconciled, _ := p.PaymentData["ar_reconciled"].(bool)
			if !reconciled {
				continue
			}
			if already, _ := p.PaymentData["unreconciled"].(bool); already {
				continue // already undone by a prior pass — idempotent
			}
			ref, _ := p.PaymentData["treasury_reference"].(string)
			if ref != matchRef {
				continue
			}
			cut := p.Amount
			if cut > remaining {
				cut = remaining
			}
			dueBefore := round2(orders.ComputeSettlement(o, 0).AmountDue)
			if !dryRun {
				pd := map[string]any{}
				for k, v := range p.PaymentData {
					pd[k] = v
				}
				pd["unreconciled"] = true
				pd["unreconciled_amount"] = cut
				pd["unreconciled_reason"] = "source ar_receipt voided in treasury"
				if _, uerr := p.Update().SetAmount(round2(p.Amount - cut)).SetPaymentData(pd).Save(ctx); uerr != nil {
					return fmt.Errorf("unreconcile payment %s: %w", p.ID, uerr)
				}
				collected, _, rerr := s.RecomputePaidTotal(ctx, o.ID)
				if rerr != nil {
					s.log.Warn("unreconcile: recompute paid total failed", zap.String("order", o.OrderNumber), zap.Error(rerr))
				}
				// The order only looked fully collected because of the phantom payment just
				// reversed — a stale credit_settled_at stamp now overstates how paid it is.
				cr, _ := s.completedReturnsTotal(ctx, o.ID)
				if _, done := o.Metadata["credit_settled_at"]; done && o.TotalAmount-collected-cr > 0.01 {
					md := map[string]any{}
					for k, v := range o.Metadata {
						md[k] = v
					}
					delete(md, "credit_settled_at")
					if merr := s.client.POSOrder.UpdateOneID(o.ID).SetMetadata(md).Exec(ctx); merr != nil {
						s.log.Warn("unreconcile: clear credit_settled_at failed", zap.String("order", o.OrderNumber), zap.Error(merr))
					}
				}
			}
			remaining = round2(remaining - cut)
			report.OrdersTouched = append(report.OrdersTouched, ReconciledOrder{
				OrderID: o.ID, OrderNumber: o.OrderNumber, DueBefore: dueBefore, Applied: -cut, DueAfter: round2(dueBefore + cut),
			})
		}
	}
	report.AmountSettled = round2(report.AmountSettled - (increase - remaining))
	report.POSOpenAfter = round2(report.POSOpenAfter + (increase - remaining))
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
