package reversals

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	entposreversal "github.com/bengobox/pos-service/internal/ent/posreversal"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/salecorrection"
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
	run(StepLoyaltyCommission, func() (string, string, bool, error) { return s.stepLoyaltyCommission(ctx, rev) })

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
//
// The whole sequence (void lines → recompute totals → net payments, for partial scope; void
// lines → mark refunded, for full scope) runs inside ONE ent.Tx: previously these were 3+
// separate unwrapped .Save(ctx) calls, so a crash between them could leave the order in a
// genuinely inconsistent state (lines voided but totals stale, or totals fixed but payments
// un-netted) with only Retry to converge it — and Retry re-running from scratch used to be
// unsafe for netPayments specifically (see its own doc comment). Wrapping in a transaction
// means either everything in this step lands, or nothing does — mirrors the locked-Tx
// pattern reverseLoyalty already uses elsewhere in this file for the same reason.
func (s *Service) stepPOSTotals(ctx context.Context, rev *ent.POSReversal) (string, string, bool, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("begin tx: %w", err)
	}
	txClient := tx.Client()

	order, err := txClient.POSOrder.Query().
		Where(entposorder.ID(rev.OrderID), entposorder.TenantID(rev.TenantID)).
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return "", "", false, fmt.Errorf("load order: %w", err)
	}

	now := time.Now()
	voidReason := fmt.Sprintf("Reversal %s: %s", rev.ReversalNumber, rev.Reason)
	for _, rl := range rev.Lines {
		line, lerr := txClient.POSOrderLine.Query().
			Where(entposorderline.ID(rl.LineID), entposorderline.OrderID(order.ID)).
			Only(ctx)
		if lerr != nil {
			_ = tx.Rollback()
			return "", "", false, fmt.Errorf("load line %s: %w", rl.LineID, lerr)
		}
		// VoidedQty is CUMULATIVE — it tracks how much of this line has been voided so far,
		// across possibly several prior partial reversals, not a one-shot "has this line ever
		// been touched" flag (resolveLines' guard was fixed to match). So the idempotent-retry
		// check here can no longer be "VoidedQty != nil" (that would now also block a
		// genuinely NEW reduction of an already-partially-voided line) — it has to detect
		// specifically whether THIS reversal already wrote its own increment on a prior,
		// failed attempt. rev.ReversalNumber is unique per reversal, so comparing against the
		// exact voidReason string this reversal stamps is a safe, precise per-line-per-reversal
		// idempotency key.
		if line.VoidedReason != nil && *line.VoidedReason == voidReason {
			continue // idempotent retry — this reversal already voided its share of this line
		}
		already := 0.0
		if line.VoidedQty != nil {
			already = *line.VoidedQty
		}
		if _, uerr := line.Update().
			SetVoidedQty(round2(already + rl.Quantity)).
			SetVoidedReason(voidReason).
			SetVoidedBy(rev.RequestedBy).
			SetVoidedAt(now).
			Save(ctx); uerr != nil {
			_ = tx.Rollback()
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
			_ = tx.Rollback()
			return "", "", false, fmt.Errorf("mark order refunded: %w", uerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return "", "", false, fmt.Errorf("commit: %w", cerr)
		}
		return rev.ReversalNumber, "order marked refunded; all active lines voided; totals/payments kept for history", false, nil
	}

	// Partial: re-derive totals from the (now soft-voided) lines — the same recompute the
	// line-void flow uses, so tax_total stays consistent — then net the payments. Both run
	// against the SAME tx-scoped client as the line-voiding above.
	updatedOrder, rerr := orders.RecomputeTotalsWithClient(ctx, txClient, rev.TenantID, order.ID)
	if rerr != nil {
		_ = tx.Rollback()
		return "", "", false, fmt.Errorf("recompute totals: %w", rerr)
	}
	if nerr := s.netPayments(ctx, txClient, rev, updatedOrder, md); nerr != nil {
		_ = tx.Rollback()
		return "", "", false, nerr
	}
	if cerr := tx.Commit(); cerr != nil {
		return "", "", false, fmt.Errorf("commit: %w", cerr)
	}
	detail := fmt.Sprintf("line(s) voided; totals recomputed to %.2f; payments netted down by %.2f", updatedOrder.TotalAmount, rev.Amount)
	return rev.ReversalNumber, detail, false, nil
}

// netPayments reduces the order's completed payment rows (newest first) by the reversed
// amount, stamps each touched row, and re-derives paid_total from the netted payments. Takes
// an explicit client (the caller's tx-scoped client, so this participates in stepPOSTotals'
// single transaction) rather than reading s.client directly.
func (s *Service) netPayments(ctx context.Context, client *ent.Client, rev *ent.POSReversal, order *ent.POSOrder, orderMD map[string]any) error {
	payments, err := client.POSPayment.Query().
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
		// Idempotent-retry check: if THIS reversal already netted this exact payment row on a
		// prior attempt (stamped in PaymentData["reversal"]), reuse its already-computed
		// netted_to value directly rather than cutting `remaining` against it again —
		// otherwise a retry re-derives `remaining` from rev.Amount (the reversal's ORIGINAL
		// full amount) and silently double-nets every payment already touched on a prior pass.
		// The transaction wrapping this whole step (see stepPOSTotals) already prevents most
		// of this from ever being reachable, but this stays as defense-in-depth for the one
		// residual class of failure a DB transaction can't cover (a commit that succeeds
		// server-side but whose acknowledgement is lost) — the same reasoning every other
		// reference-keyed idempotency guard in this codebase (e.g. ledger's
		// JournalEntryExistsByReference) exists for.
		if existing, ok := p.PaymentData["reversal"].(map[string]any); ok {
			if rn, _ := existing["reversal_number"].(string); rn == rev.ReversalNumber {
				if v, ok := existing["netted_to"].(float64); ok {
					newPaidTotal += v
					continue
				}
			}
		}
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
//
// The reversed amount can legitimately be larger than what stepPOSTotals (previous step) was
// able to net back from real POSPayment rows: an in-place Edit-Sale increase
// (saleedit.applyInPlaceIncrease) adds value to the order posted straight to AR/receivable,
// never collected as cash, so reducing/removing that same line later has no cash to give
// back for that portion. Found live 2026-08-05: removing a line added via Edit Sale netted a
// real KES 100 cash payment down to 0 while treasury recorded a phantom KES 1,400 CASH refund
// for money the business never actually received. Fix: split the GL reversal between however
// much was actually cash-netted (derived from stepPOSTotals' own PaymentData stamps — no new
// field) and whatever remains, which write off against AR via offset_invoice instead.
func (s *Service) stepTreasuryGL(ctx context.Context, rev *ent.POSReversal, tenantSlug string) (string, string, bool, error) {
	if s.treasuryClient == nil {
		return "", "treasury client not configured", true, nil
	}
	if rev.Amount <= 0.009 {
		return "", "nothing to settle (zero-value lines)", true, nil
	}

	crmContactID, customerName, customerPhone := orders.ResolveOrderCustomer(ctx, s.client, rev.TenantID, rev.OrderID)

	cashNetted := round2(s.cashNettedForReversal(ctx, rev))
	if cashNetted < 0 {
		cashNetted = 0
	}
	if cashNetted > rev.Amount {
		cashNetted = rev.Amount
	}
	arPortion := round2(rev.Amount - cashNetted)
	ratio := 0.0
	if rev.Amount > 0 {
		ratio = cashNetted / rev.Amount
	}
	cashChannel := rev.RefundChannel
	if cashChannel == "" || cashChannel == "offset_invoice" {
		cashChannel = "cash"
	}

	base := treasury.RefundRequest{
		SourceService: "pos", ReferenceType: "pos_return",
		Reference: rev.ReversalNumber, Currency: "KES", Reason: rev.Reason,
		CrmContactID: crmContactID, CustomerIdentifier: customerPhone, CustomerName: customerName,
	}

	// treasury's ProcessRefund requires ReferenceID to parse as a real uuid.UUID (it rejects
	// anything else with "reference_id must be a uuid") — a plain string-suffixed variant of
	// rev.ID is NOT one. Found live 2026-08-05: the split branch below silently failed both
	// calls in production (400 from treasury) before this was caught by checking treasury's own
	// handler, since the test double used while developing this fix didn't validate UUID
	// format. uuid.NewSHA1 derives a real, distinct UUID per portion that is nonetheless
	// deterministic across retries (same rev.ID + suffix → same id every time). The single-
	// portion cases keep posting under rev.ID itself (suffix ""), byte-identical to the
	// reference id this call used before this fix existed.
	var ids, details []string
	post := func(suffix, channel string, amount, tax, cost float64) error {
		id := rev.ID
		if suffix != "" {
			id = uuid.NewSHA1(rev.ID, []byte(suffix))
		}
		req := base
		req.ReferenceID = id.String()
		req.Amount, req.TaxAmount, req.Cost, req.RefundChannel = amount, tax, cost, channel
		resp, err := s.treasuryClient.CreateRefund(ctx, tenantSlug, id.String(), req)
		if err != nil {
			return err
		}
		ids = append(ids, resp.ID)
		details = append(details, fmt.Sprintf("%.2f via %s", amount, channel))
		return nil
	}

	if arPortion <= 0.009 {
		// Fully cash-backed — the common case, identical to pre-fix behavior (single call).
		if err := post("", cashChannel, rev.Amount, rev.TaxAmount, rev.CostAmount); err != nil {
			return "", "", false, err
		}
	} else if cashNetted <= 0.009 {
		// Fully AR-backed (e.g. an on-account sale, or an edit-added line removed before any
		// cash ever moved) — single call, same reference id as before, explicit channel only.
		if err := post("", "offset_invoice", rev.Amount, rev.TaxAmount, rev.CostAmount); err != nil {
			return "", "", false, err
		}
	} else {
		if err := post("cash", cashChannel, cashNetted, round2(rev.TaxAmount*ratio), round2(rev.CostAmount*ratio)); err != nil {
			return "", "", false, fmt.Errorf("cash portion: %w", err)
		}
		if err := post("ar", "offset_invoice", arPortion, round2(rev.TaxAmount*(1-ratio)), round2(rev.CostAmount*(1-ratio))); err != nil {
			return "", "", false, fmt.Errorf("AR write-off portion: %w", err)
		}
	}

	detail := fmt.Sprintf("GL reversed %.2f (tax %.2f, cost %.2f): %s", rev.Amount, rev.TaxAmount, rev.CostAmount, strings.Join(details, " + "))
	return strings.Join(ids, ","), detail, false, nil
}

// cashNettedForReversal sums how much real POSPayment amount stepPOSTotals actually netted
// back for THIS reversal (matched via the reversal_number it stamps into each touched
// payment's PaymentData — see netPayments). This is the ground truth for "how much of this
// reversal's amount was ever real collected cash," independent of the order-level on_account
// flag, so it correctly handles an order that mixes real cash with edit-added AR value.
func (s *Service) cashNettedForReversal(ctx context.Context, rev *ent.POSReversal) float64 {
	payments, err := s.client.POSPayment.Query().Where(entpospayment.OrderID(rev.OrderID)).All(ctx)
	if err != nil {
		return 0
	}
	var total float64
	for _, p := range payments {
		rmeta, ok := p.PaymentData["reversal"].(map[string]any)
		if !ok {
			continue
		}
		if num, _ := rmeta["reversal_number"].(string); num != rev.ReversalNumber {
			continue
		}
		from, _ := rmeta["netted_from"].(float64)
		to, _ := rmeta["netted_to"].(float64)
		total += from - to
	}
	return total
}

// stepEtimsCreditNote raises the VAT-reversal credit note against the original sale's tax
// invoice when one exists in treasury (fiscalised sales). Sales that were never invoiced /
// transmitted (most cash POS sales) are SKIPPED — there is nothing to reverse at KRA.
func (s *Service) stepEtimsCreditNote(ctx context.Context, rev *ent.POSReversal, tenantSlug string) (string, string, bool, error) {
	if s.treasuryClient == nil {
		return "", "treasury client not configured", true, nil
	}
	fiscalized, invoiceID, _ := orders.IsFiscalized(ctx, s.treasuryClient, tenantSlug, rev.OrderID)
	if !fiscalized {
		return "", "sale has no treasury tax invoice — no eTIMS credit note needed", true, nil
	}
	cn, err := s.treasuryClient.CreateCreditNote(ctx, tenantSlug, invoiceID)
	if err != nil {
		return "", "", false, err
	}
	return cn.Number, "credit note raised against invoice " + invoiceID, false, nil
}

// stepLoyaltyCommission claws back loyalty points and voids staff commission earned on the
// reversed portion of the sale, so a replacement/addendum sale (or the plain passage of time on
// a soft-voided-but-kept fiscalized order) never lets the customer/staff double-earn for the same
// underlying value. Prorated by rev.Amount / order.total_amount for a partial reversal; the full
// amount for a full reversal. Never fails the reversal — a lookup/write error just skips its half
// with a logged warning, since loyalty/commission are not money the business owes anyone.
func (s *Service) stepLoyaltyCommission(ctx context.Context, rev *ent.POSReversal) (string, string, bool, error) {
	order, err := s.client.POSOrder.Query().
		Where(entposorder.ID(rev.OrderID), entposorder.TenantID(rev.TenantID)).
		Only(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("load order: %w", err)
	}

	ratio := 1.0
	if rev.Scope == entposreversal.ScopePartial {
		if order.TotalAmount <= 0.009 {
			return "", "order has no total to prorate against", true, nil
		}
		ratio = rev.Amount / order.TotalAmount
		if ratio > 1 {
			ratio = 1
		}
	}

	// Skus scopes commission clawback to the reversed lines for a partial reversal; full scope
	// claws back every commission on the order regardless of which SKUs happen to be listed.
	skus := map[string]bool{}
	if rev.Scope == entposreversal.ScopePartial {
		for _, rl := range rev.Lines {
			if rl.SKU != "" {
				skus[rl.SKU] = true
			}
		}
	}

	parts := make([]string, 0, 2)
	if d := salecorrection.ReverseLoyalty(ctx, s.log, s.client, rev.TenantID, order.ID, ratio, "Reversal "+rev.ReversalNumber); d != "" {
		parts = append(parts, d)
	}
	if d := salecorrection.ReverseCommission(ctx, s.log, s.client, rev.TenantID, order.ID, skus, "reversal "+rev.ReversalNumber); d != "" {
		parts = append(parts, d)
	}
	if len(parts) == 0 {
		return "", "nothing to reverse (no loyalty earn / commission on this sale)", true, nil
	}
	return rev.ReversalNumber, strings.Join(parts, "; "), false, nil
}
