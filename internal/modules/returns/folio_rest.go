package returns

import (
	"context"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/payments"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// folioRest is the part of a room-charged return the open stay's folio no longer owed: it is
// refunded through the cashier's channel as its own treasury refund.
type folioRest struct {
	Amount, Tax, Cost float64
	Channel           string
}

// postFolioRest sends the rest refund to treasury under RestRefundReferenceType (no credit-note
// document) and its deterministic id, with the return's outlet for GL attribution. It is sent only
// after the main refund went through: the main refund's credit note carries every returned line,
// and the return.completed GL backstop books only the main part (ledger_refund_amount).
func (s *Service) postFolioRest(ctx context.Context, tenantRef string, ret *ent.POSReturn, rest folioRest) error {
	crmContactID, customerName, customerIdentifier := orders.ResolveOrderCustomer(ctx, s.client, ret.TenantID, ret.OrderID)
	currency := "KES"
	if o, err := s.client.POSOrder.Query().
		Where(entposorder.ID(ret.OrderID), entposorder.TenantID(ret.TenantID)).
		Only(ctx); err == nil && o.Currency != "" {
		currency = o.Currency
	}
	outlet := ""
	if ret.OutletID != uuid.Nil {
		outlet = ret.OutletID.String()
	}
	id := restRefundID(ret.ID)
	_, err := s.treasuryClient.CreateRefund(ctx, tenantRef, id.String(), treasury.RefundRequest{
		SourceService: "pos", ReferenceID: id.String(), ReferenceType: RestRefundReferenceType,
		Reference: ret.ReturnNumber, Amount: rest.Amount, TaxAmount: rest.Tax, Cost: rest.Cost,
		Currency: currency, Reason: ret.Reason, RefundChannel: payments.TreasuryRefundChannel(rest.Channel),
		CrmContactID: crmContactID, CustomerIdentifier: customerIdentifier, CustomerName: customerName,
		OutletID: outlet,
	})
	return err
}

// folioRestFromMetadata reads a pending rest back from the return's metadata.
func folioRestFromMetadata(md map[string]any) (folioRest, bool) {
	amount, _ := md[MetaFolioRestAmount].(float64)
	if amount <= 0.009 {
		return folioRest{}, false
	}
	r := folioRest{Amount: amount}
	r.Tax, _ = md[MetaFolioRestTax].(float64)
	r.Cost, _ = md[MetaFolioRestCost].(float64)
	r.Channel, _ = md[MetaFolioRestChannel].(string)
	if r.Channel == "" {
		r.Channel = string(posreturn.RefundChannelCash)
	}
	return r, true
}

// RetryPendingFolioRests sends the rest refunds still marked pending and returns how many went
// through. It runs with the credit-note retry (StartCreditNoteRetry). Treasury dedupes on the
// rest's deterministic id, so a retry that races another pod still books it once.
//
// Oldest returns go first, folioRestRetryBatch at a time. A rest treasury keeps refusing is tried
// maxFolioRestAttempts times (about an hour at the 5 minute tick), then marked folio_rest_failed
// with one error log carrying ids only, and leaves the queue so it never starves newer ones.
func (s *Service) RetryPendingFolioRests(ctx context.Context) int {
	if s.treasuryClient == nil {
		return 0
	}
	rets, err := s.client.POSReturn.Query().
		Where(posreturn.StatusEQ(posreturn.StatusCompleted), func(sel *sql.Selector) {
			sel.Where(sqljson.ValueEQ(sel.C(posreturn.FieldMetadata), true, sqljson.Path(MetaFolioRestPending)))
		}).
		Order(ent.Asc(posreturn.FieldCreatedAt), ent.Asc(posreturn.FieldID)).
		Limit(folioRestRetryBatch).
		All(ctx)
	if err != nil {
		s.log.Warn("folio rest retry: query failed", zap.Error(err))
		return 0
	}
	sent := 0
	for _, ret := range rets {
		rest, ok := folioRestFromMetadata(ret.Metadata)
		if ok {
			// Treasury resolves a tenant id in place of the slug on its S2S routes.
			if err := s.postFolioRest(ctx, ret.TenantID.String(), ret, rest); err != nil {
				s.recordFolioRestFailure(ctx, ret.ID)
				continue
			}
			sent++
		}
		s.updateFolioRestMeta(ctx, ret.ID, func(md map[string]any) {
			delete(md, MetaFolioRestPending)
			delete(md, MetaFolioRestAttempts)
		})
	}
	return sent
}

// maxFolioRestAttempts and folioRestRetryBatch bound the rest retry (variables so tests can
// shrink the batch).
const maxFolioRestAttempts = 12

var folioRestRetryBatch = creditNoteRetryBatch

// recordFolioRestFailure counts a failed retry and, at the cap, takes the rest out of the queue.
func (s *Service) recordFolioRestFailure(ctx context.Context, returnID uuid.UUID) {
	s.updateFolioRestMeta(ctx, returnID, func(md map[string]any) {
		attempts, _ := md[MetaFolioRestAttempts].(float64)
		attempts++
		md[MetaFolioRestAttempts] = attempts
		if attempts >= maxFolioRestAttempts {
			delete(md, MetaFolioRestPending)
			md[MetaFolioRestFailed] = true
			s.log.Error("room folio rest refund gave up after repeated treasury failures; settle it by hand",
				zap.String("return_id", returnID.String()), zap.Int("attempts", int(attempts)))
		}
	})
}

// updateFolioRestMeta changes the return's metadata under its row lock.
func (s *Service) updateFolioRestMeta(ctx context.Context, returnID uuid.UUID, change func(map[string]any)) {
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
	change(md)
	if _, err := tx.POSReturn.UpdateOne(locked).SetMetadata(md).Save(ctx); err != nil {
		return
	}
	_ = tx.Commit()
}
