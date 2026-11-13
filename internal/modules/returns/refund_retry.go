package returns

import (
	"context"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// The main treasury refund of a completed return (S2-8 review I-1). When CompleteReturn's S2S
// refund fails (a treasury restart, a node reboot), the return still completes; it keeps the
// refund it meant to send under these metadata keys and RetryPendingRefunds sends it again.
//
// Treasury books a refund once per return id ("pos_return", return_id). If its
// pos.return.completed GL backstop booked the return first, it did so on the account the event's
// refund_channel names (the same resolution as the refund), and the retried refund books no second
// entry: it only adds what the backstop cannot (the COGS reversal, the store-credit grant or the
// AR offset on the customer's balance, the credit note's lines).
const (
	MetaRefundPending        = "treasury_refund_pending"
	MetaRefundAmount         = "treasury_refund_amount"
	MetaRefundTax            = "treasury_refund_tax"
	MetaRefundCost           = "treasury_refund_cost"
	MetaRefundExchangeCredit = "treasury_refund_exchange_credit"
	MetaRefundChannel        = "treasury_refund_channel"
	MetaRefundCurrency       = "treasury_refund_currency"
	// MetaRefundAttempts counts failed retries; after maxRefundAttempts the refund is marked
	// MetaRefundFailed and left for staff (the GL backstop has still booked the return).
	MetaRefundAttempts = "treasury_refund_attempts"
	MetaRefundFailed   = "treasury_refund_failed"
)

// maxRefundAttempts is about an hour at the 5 minute retry tick, like the folio rest.
const maxRefundAttempts = 12

// refundRetryBatch bounds one retry pass (a variable so tests can shrink it).
var refundRetryBatch = creditNoteRetryBatch

// mainRefund is the first (main) treasury portion of a return's refund, as CompleteReturn built it.
type mainRefund struct {
	Amount, Tax, Cost, ExchangeCredit float64
	Channel, Currency                 string
}

func (r mainRefund) toMetadata(md map[string]any) {
	md[MetaRefundPending] = true
	md[MetaRefundAmount] = r.Amount
	md[MetaRefundTax] = r.Tax
	md[MetaRefundCost] = r.Cost
	md[MetaRefundExchangeCredit] = r.ExchangeCredit
	md[MetaRefundChannel] = r.Channel
	md[MetaRefundCurrency] = r.Currency
}

// mainRefundFromMetadata reads a pending main refund back from the return's metadata.
func mainRefundFromMetadata(md map[string]any) (mainRefund, bool) {
	var r mainRefund
	r.Amount, _ = md[MetaRefundAmount].(float64)
	if r.Amount <= 0.009 {
		return mainRefund{}, false
	}
	r.Tax, _ = md[MetaRefundTax].(float64)
	r.Cost, _ = md[MetaRefundCost].(float64)
	r.ExchangeCredit, _ = md[MetaRefundExchangeCredit].(float64)
	r.Channel, _ = md[MetaRefundChannel].(string)
	r.Currency, _ = md[MetaRefundCurrency].(string)
	if r.Channel == "" {
		r.Channel = string(posreturn.RefundChannelCash)
	}
	if r.Currency == "" {
		r.Currency = "KES"
	}
	return r, true
}

// refundPending reports whether the return's main treasury refund is still waiting for a retry.
func refundPending(md map[string]any) bool {
	pending, _ := md[MetaRefundPending].(bool)
	return pending
}

// RetryPendingRefunds sends the main treasury refunds still marked pending and returns how many
// went through. It runs first on the credit-note retry tick (StartCreditNoteRetry), so a folio
// rest waiting on its main refund goes out on the same tick. Oldest returns go first,
// refundRetryBatch at a time; a refund treasury keeps refusing is tried maxRefundAttempts times,
// then marked treasury_refund_failed with one error log carrying ids only. The S2S call uses the
// return id as both reference and idempotency key, so a retry racing another pod books once.
func (s *Service) RetryPendingRefunds(ctx context.Context) int {
	if s.treasuryClient == nil {
		return 0
	}
	rets, err := s.client.POSReturn.Query().
		Where(posreturn.StatusEQ(posreturn.StatusCompleted), func(sel *sql.Selector) {
			sel.Where(sqljson.ValueEQ(sel.C(posreturn.FieldMetadata), true, sqljson.Path(MetaRefundPending)))
		}).
		WithLines().
		Order(ent.Asc(posreturn.FieldCreatedAt), ent.Asc(posreturn.FieldID)).
		Limit(refundRetryBatch).
		All(ctx)
	if err != nil {
		s.log.Warn("return refund retry: query failed", zap.Error(err))
		return 0
	}
	sent := 0
	for _, ret := range rets {
		r, ok := mainRefundFromMetadata(ret.Metadata)
		if !ok {
			s.updateFolioRestMeta(ctx, ret.ID, clearPendingRefund)
			continue
		}
		ref, linesSent, err := s.postMainRefund(ctx, ret, r)
		if err != nil {
			s.recordRefundFailure(ctx, ret.ID)
			continue
		}
		sent++
		s.completePendingRefund(ctx, ret.ID, ref, linesSent)
	}
	return sent
}

// postMainRefund sends the main refund exactly as CompleteReturn does. Treasury resolves a tenant
// id in place of the slug on its S2S routes. The returned lines go too when the sale was
// fiscalised: treasury keys the credit note on the return id, so one raised on its own meanwhile
// is the same credit note.
func (s *Service) postMainRefund(ctx context.Context, ret *ent.POSReturn, r mainRefund) (string, bool, error) {
	tenantRef := ret.TenantID.String()
	fiscal := s.fiscalRefundFor(ctx, tenantRef, ret, ret.Edges.Lines)
	crmContactID, customerName, customerIdentifier := orders.ResolveOrderCustomer(ctx, s.client, ret.TenantID, ret.OrderID)
	resp, err := s.treasuryClient.CreateRefund(ctx, tenantRef, ret.ID.String(), treasury.RefundRequest{
		SourceService:      "pos",
		ReferenceID:        ret.ID.String(),
		ReferenceType:      "pos_return",
		Reference:          ret.ReturnNumber,
		Amount:             r.Amount,
		TaxAmount:          r.Tax,
		Cost:               r.Cost,
		Currency:           r.Currency,
		Reason:             ret.Reason,
		RefundChannel:      r.Channel,
		CrmContactID:       crmContactID,
		CustomerIdentifier: customerIdentifier,
		CustomerName:       customerName,
		OutletID:           returnOutlet(ret),
		OriginalOrderID:    fiscal.orderID,
		Lines:              fiscal.lines,
		ExchangeCredit:     r.ExchangeCredit,
	})
	if err != nil {
		return "", false, err
	}
	return resp.ID, len(fiscal.lines) > 0, nil
}

// returnOutlet is the return's outlet, sent on every refund (not only a fiscalised sale's) so
// treasury credits that outlet's own account for the channel, the same account its
// pos.return.completed backstop picks (S2-8 N-3).
func returnOutlet(ret *ent.POSReturn) string {
	if ret.OutletID == uuid.Nil {
		return ""
	}
	return ret.OutletID.String()
}

// clearPendingRefund drops the pending main refund and its retry state.
func clearPendingRefund(md map[string]any) {
	for _, k := range []string{MetaRefundPending, MetaRefundAmount, MetaRefundTax, MetaRefundCost,
		MetaRefundExchangeCredit, MetaRefundChannel, MetaRefundCurrency, MetaRefundAttempts} {
		delete(md, k)
	}
}

// completePendingRefund records the treasury reference of a retried refund and clears it from the
// queue. A credit note still pending is done now that the refund carried the lines.
func (s *Service) completePendingRefund(ctx context.Context, returnID uuid.UUID, ref string, linesSent bool) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := lockReturn(ctx, tx, posreturn.ID(returnID))
	if err != nil {
		return
	}
	md := cloneReturnMetadata(locked.Metadata)
	clearPendingRefund(md)
	if linesSent && md[mdFiscalCreditNote] == creditNotePending {
		md[mdFiscalCreditNote] = creditNoteDone
	}
	upd := tx.POSReturn.UpdateOne(locked).SetMetadata(md)
	if ref != "" {
		upd = upd.SetTreasuryRefundRef(ref)
	}
	if _, err := upd.Save(ctx); err != nil {
		return
	}
	_ = tx.Commit()
}

// recordRefundFailure counts a failed retry and, at the cap, takes the refund out of the queue.
func (s *Service) recordRefundFailure(ctx context.Context, returnID uuid.UUID) {
	s.updateFolioRestMeta(ctx, returnID, func(md map[string]any) {
		attempts, _ := md[MetaRefundAttempts].(float64)
		attempts++
		md[MetaRefundAttempts] = attempts
		if attempts >= maxRefundAttempts {
			delete(md, MetaRefundPending)
			md[MetaRefundFailed] = true
			s.log.Error("return refund gave up after repeated treasury failures; the GL backstop booked the return, check the customer balance and COGS by hand",
				zap.String("return_id", returnID.String()), zap.Int("attempts", int(attempts)))
		}
	})
}
