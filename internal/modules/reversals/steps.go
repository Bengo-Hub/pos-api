package reversals

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	entposreversal "github.com/bengobox/pos-service/internal/ent/posreversal"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// runSteps executes every non-completed step in order, persisting the step ledger after
// each one. A failed step never aborts the remaining steps — each is independently
// idempotent, and the Retry endpoint re-runs whatever failed.
func (s *Service) runSteps(ctx context.Context, rev *ent.POSReversal, tenantSlug string) *ent.POSReversal {
	steps := append([]entschema.ReversalStepJSON(nil), rev.Steps...)

	run := func(name string, fn func() (ref, detail string, skip bool, err error)) {
		i := stepIndex(steps, name)
		if i < 0 || steps[i].Status == StatusCompleted || steps[i].Status == StatusSkipped {
			return
		}
		ref, detail, skip, err := fn()
		steps[i].At = time.Now().UTC().Format(time.RFC3339)
		steps[i].Ref = ref
		steps[i].Detail = detail
		switch {
		case err != nil:
			steps[i].Status = StatusFailed
			steps[i].Detail = err.Error()
			s.log.Error("reversal step failed",
				zap.String("reversal", rev.ReversalNumber), zap.String("step", name), zap.Error(err))
		case skip:
			steps[i].Status = StatusSkipped
		default:
			steps[i].Status = StatusCompleted
		}
		rev = s.persistSteps(ctx, rev, steps)
	}

	run(StepPOSTotals, func() (string, string, bool, error) { return s.stepPOSTotals(ctx, rev) })
	run(StepInventory, func() (string, string, bool, error) { return s.stepInventory(ctx, rev) })
	run(StepTreasuryGL, func() (string, string, bool, error) { return s.stepTreasuryGL(ctx, rev, tenantSlug) })
	run(StepEtimsCreditNote, func() (string, string, bool, error) { return s.stepEtimsCreditNote(ctx, rev, tenantSlug) })

	return s.finalizeStatus(ctx, rev, steps)
}

func stepIndex(steps []entschema.ReversalStepJSON, name string) int {
	for i := range steps {
		if steps[i].Step == name {
			return i
		}
	}
	return -1
}

// persistSteps saves the step ledger mid-run so the sync-monitor tab streams real progress.
func (s *Service) persistSteps(ctx context.Context, rev *ent.POSReversal, steps []entschema.ReversalStepJSON) *ent.POSReversal {
	updated, err := rev.Update().SetSteps(steps).Save(ctx)
	if err != nil {
		s.log.Warn("reversal: persist steps failed", zap.Error(err))
		return rev
	}
	return updated
}

// finalizeStatus derives the overall reversal status from its steps.
func (s *Service) finalizeStatus(ctx context.Context, rev *ent.POSReversal, steps []entschema.ReversalStepJSON) *ent.POSReversal {
	completed, failed := 0, 0
	for _, st := range steps {
		switch st.Status {
		case StatusCompleted, StatusSkipped:
			completed++
		case StatusFailed:
			failed++
		}
	}
	status := entposreversal.StatusPending
	switch {
	case failed == 0 && completed == len(steps):
		status = entposreversal.StatusCompleted
	case failed > 0 && completed > 0:
		status = entposreversal.StatusPartialFailure
	case failed == len(steps):
		status = entposreversal.StatusFailed
	case failed > 0:
		status = entposreversal.StatusPartialFailure
	}
	updated, err := rev.Update().SetSteps(steps).SetStatus(status).Save(ctx)
	if err != nil {
		s.log.Warn("reversal: persist final status failed", zap.Error(err))
		return rev
	}
	return updated
}

// stepPOSTotals soft-voids the reversed lines and fixes the order's money records:
//   - partial: RecomputeTotals (drops the voided value + its tax) and nets the completed
//     payment rows + paid_total down to the new total (2026-07-17 platform decision).
//   - full: all lines voided, order → refunded; totals and payments are KEPT (history
//     preserved) and stamped with the reversal reference.
func (s *Service) stepPOSTotals(ctx context.Context, rev *ent.POSReversal) (string, string, bool, error) {
	order, err := s.client.POSOrder.Query().
		Where(entposorder.ID(rev.OrderID), entposorder.TenantID(rev.TenantID)).
		Only(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("load order: %w", err)
	}

	now := time.Now()
	voidReason := fmt.Sprintf("Reversal %s: %s", rev.ReversalNumber, rev.Reason)
	for _, rl := range rev.Lines {
		line, lerr := s.client.POSOrderLine.Query().
			Where(entposorderline.ID(rl.LineID), entposorderline.OrderID(order.ID)).
			Only(ctx)
		if lerr != nil {
			return "", "", false, fmt.Errorf("load line %s: %w", rl.LineID, lerr)
		}
		if line.VoidedQty != nil {
			continue // idempotent retry — already voided by a previous attempt
		}
		if _, uerr := line.Update().
			SetVoidedQty(rl.Quantity).
			SetVoidedReason(voidReason).
			SetVoidedBy(rev.RequestedBy).
			SetVoidedAt(now).
			Save(ctx); uerr != nil {
			return "", "", false, fmt.Errorf("void line %s: %w", rl.LineID, uerr)
		}
	}

	stamp := map[string]any{
		"reversal_number": rev.ReversalNumber,
		"reversal_id":     rev.ID.String(),
		"reversed_amount": rev.Amount,
		"reversed_at":     now.UTC().Format(time.RFC3339),
	}
	md := map[string]any{}
	for k, v := range order.Metadata {
		md[k] = v
	}
	md["reversal"] = stamp

	if rev.Scope == entposreversal.ScopeFull {
		if _, uerr := order.Update().
			SetStatus("refunded").
			SetMetadata(md).
			Save(ctx); uerr != nil {
			return "", "", false, fmt.Errorf("mark order refunded: %w", uerr)
		}
		return rev.ReversalNumber, "order marked refunded; all active lines voided; totals/payments kept for history", false, nil
	}

	// Partial: re-derive totals from the (now soft-voided) lines — the same recompute the
	// line-void flow uses, so tax_total stays consistent — then net the payments.
	updatedOrder, rerr := s.orderSvc.RecomputeTotals(ctx, rev.TenantID, order.ID)
	if rerr != nil {
		return "", "", false, fmt.Errorf("recompute totals: %w", rerr)
	}
	if nerr := s.netPayments(ctx, rev, updatedOrder, md); nerr != nil {
		return "", "", false, nerr
	}
	detail := fmt.Sprintf("line(s) voided; totals recomputed to %.2f; payments netted down by %.2f", updatedOrder.TotalAmount, rev.Amount)
	return rev.ReversalNumber, detail, false, nil
}

// netPayments reduces the order's completed payment rows (newest first) by the reversed
// amount, stamps each touched row, and re-derives paid_total from the netted payments.
func (s *Service) netPayments(ctx context.Context, rev *ent.POSReversal, order *ent.POSOrder, orderMD map[string]any) error {
	payments, err := s.client.POSPayment.Query().
		Where(entpospayment.OrderID(order.ID), entpospayment.StatusEQ("completed")).
		Order(ent.Desc(entpospayment.FieldOccurredAt)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("load payments: %w", err)
	}

	remaining := rev.Amount
	var newPaidTotal float64
	for _, p := range payments {
		newAmt := p.Amount
		if remaining > 0.009 && p.Amount > 0 {
			cut := remaining
			if cut > p.Amount {
				cut = p.Amount
			}
			newAmt = round2(p.Amount - cut)
			remaining = round2(remaining - cut)

			pd := map[string]any{}
			for k, v := range p.PaymentData {
				pd[k] = v
			}
			pd["reversal"] = map[string]any{
				"reversal_number": rev.ReversalNumber,
				"netted_from":     p.Amount,
				"netted_to":       newAmt,
			}
			if _, uerr := p.Update().SetAmount(newAmt).SetPaymentData(pd).Save(ctx); uerr != nil {
				return fmt.Errorf("net payment %s: %w", p.ID, uerr)
			}
		}
		newPaidTotal += newAmt
	}

	if _, err := order.Update().
		SetPaidTotal(round2(newPaidTotal)).
		SetMetadata(orderMD).
		Save(ctx); err != nil {
		return fmt.Errorf("update paid_total: %w", err)
	}
	return nil
}

// stepInventory reverses the recorded BOM/ingredient consumption via inventory-api S2S.
// Idempotent on the reversal id; inventory additionally caps add-backs so overlapping
// reversals can never over-return stock, and shortfall portions never return (they never left).
func (s *Service) stepInventory(ctx context.Context, rev *ent.POSReversal) (string, string, bool, error) {
	if s.inventoryClient == nil {
		return "", "inventory client not configured", true, nil
	}

	req := inventory.ReverseConsumptionRequest{
		OrderID:        rev.OrderID.String(),
		Reason:         fmt.Sprintf("Reversal %s: %s", rev.ReversalNumber, rev.Reason),
		IdempotencyKey: "pos-reversal-" + rev.ID.String(),
	}
	if rev.Scope == entposreversal.ScopePartial {
		// of_quantity must be the TOTAL sold quantity of that SKU on the order (a SKU may
		// span several lines; inventory's consumption is aggregated per order+SKU).
		soldBySKU := map[string]float64{}
		lines, err := s.client.POSOrderLine.Query().
			Where(entposorderline.OrderID(rev.OrderID)).
			All(ctx)
		if err != nil {
			return "", "", false, fmt.Errorf("load lines for sku totals: %w", err)
		}
		for _, l := range lines {
			soldBySKU[l.Sku] += l.Quantity
		}
		revBySKU := map[string]float64{}
		for _, rl := range rev.Lines {
			revBySKU[rl.SKU] += rl.Quantity
		}
		for sku, qty := range revBySKU {
			req.Items = append(req.Items, inventory.ReverseConsumptionItem{
				SKU: sku, Quantity: qty, OfQuantity: soldBySKU[sku],
			})
		}
	}

	resp, err := s.inventoryClient.ReverseConsumption(ctx, rev.TenantID.String(), req)
	if err != nil {
		return "", "", false, err
	}
	detail := fmt.Sprintf("%d ingredient line(s) reversed; actual cost %.2f", len(resp.Ingredients), resp.TotalCostReversed)
	if resp.AlreadyProcessed {
		detail = "already reversed (idempotent replay)"
	}
	return resp.ID, detail, false, nil
}

// stepTreasuryGL posts the GL reversal via treasury's refunds endpoint — the SAME call the
// returns flow settles through (revenue+VAT reversal, COGS reversal when cost was posted,
// AR netting for store_credit/offset channels, auto credit-note document). Idempotent on
// the reversal id (reference_id + Idempotency-Key).
func (s *Service) stepTreasuryGL(ctx context.Context, rev *ent.POSReversal, tenantSlug string) (string, string, bool, error) {
	if s.treasuryClient == nil {
		return "", "treasury client not configured", true, nil
	}
	if rev.Amount <= 0.009 {
		return "", "nothing to settle (zero-value lines)", true, nil
	}

	crmContactID, customerName, customerPhone := s.resolveOrderCustomer(ctx, rev.TenantID, rev.OrderID)
	resp, err := s.treasuryClient.CreateRefund(ctx, tenantSlug, rev.ID.String(), treasury.RefundRequest{
		SourceService:      "pos",
		ReferenceID:        rev.ID.String(),
		ReferenceType:      "pos_return", // treasury's return settlement path: GL reversal + numbered credit-note doc
		Reference:          rev.ReversalNumber,
		Amount:             rev.Amount,
		TaxAmount:          rev.TaxAmount,
		Cost:               rev.CostAmount,
		Currency:           "KES",
		Reason:             rev.Reason,
		RefundChannel:      rev.RefundChannel,
		CrmContactID:       crmContactID,
		CustomerIdentifier: customerPhone,
		CustomerName:       customerName,
	})
	if err != nil {
		return "", "", false, err
	}
	detail := fmt.Sprintf("GL reversed %.2f (tax %.2f, cost %.2f) via %s", rev.Amount, rev.TaxAmount, rev.CostAmount, rev.RefundChannel)
	return resp.ID, detail, false, nil
}

// stepEtimsCreditNote raises the VAT-reversal credit note against the original sale's tax
// invoice when one exists in treasury (fiscalised sales). Sales that were never invoiced /
// transmitted (most cash POS sales) are SKIPPED — there is nothing to reverse at KRA.
func (s *Service) stepEtimsCreditNote(ctx context.Context, rev *ent.POSReversal, tenantSlug string) (string, string, bool, error) {
	if s.treasuryClient == nil {
		return "", "treasury client not configured", true, nil
	}
	inv, err := s.treasuryClient.GetInvoiceByReference(ctx, tenantSlug, "pos_order", rev.OrderID.String())
	if err != nil || inv == nil || inv.ID == "" {
		return "", "sale has no treasury tax invoice — no eTIMS credit note needed", true, nil
	}
	cn, err := s.treasuryClient.CreateCreditNote(ctx, tenantSlug, inv.ID)
	if err != nil {
		return "", "", false, err
	}
	return cn.Number, "credit note raised against invoice " + inv.ID, false, nil
}

// resolveOrderCustomer returns the original buyer's CRM contact (via the phone-matched
// loyalty account), name and phone — the same linkage the returns flow forwards to treasury.
func (s *Service) resolveOrderCustomer(ctx context.Context, tenantID, orderID uuid.UUID) (crmContactID, name, phone string) {
	order, err := s.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return "", "", ""
	}
	if order.CustomerName != nil {
		name = *order.CustomerName
	}
	if order.CustomerPhone != nil {
		phone = *order.CustomerPhone
	}
	if phone != "" {
		if acc, accErr := s.client.LoyaltyAccount.Query().
			Where(entla.TenantID(tenantID), entla.CustomerPhone(phone)).
			First(ctx); accErr == nil && acc != nil && acc.CrmContactID != nil {
			crmContactID = acc.CrmContactID.String()
		}
	}
	return crmContactID, name, phone
}
