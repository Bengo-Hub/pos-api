package returns

import (
	"context"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// A completed return of a fiscalised sale must reach KRA as a credit note. When neither the refund
// call nor the credit note call got through (treasury down, a timeout), CompleteReturn marks the
// return's metadata fiscal_credit_note "pending", and this job raises it later. Treasury keys the
// credit note on the return id, so a retry, or two pods retrying at once, yields one credit note.
const (
	mdFiscalCreditNote   = "fiscal_credit_note"
	creditNotePending    = "pending"
	creditNoteDone       = "done"
	creditNoteNotNeeded  = "not_needed"
	creditNoteRetryEvery = 5 * time.Minute
	creditNoteRetryBatch = 50
)

// StartCreditNoteRetry retries pending return credit notes until ctx ends.
func (s *Service) StartCreditNoteRetry(ctx context.Context) {
	t := time.NewTicker(creditNoteRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.RetryPendingCreditNotes(ctx); n > 0 {
				s.log.Info("return credit notes raised on retry", zap.Int("count", n))
			}
		}
	}
}

// RetryPendingCreditNotes raises the credit note for completed returns still marked pending and
// returns how many went through.
func (s *Service) RetryPendingCreditNotes(ctx context.Context) int {
	if s.treasuryClient == nil {
		return 0
	}
	rets, err := s.client.POSReturn.Query().
		Where(posreturn.StatusEQ(posreturn.StatusCompleted), func(sel *sql.Selector) {
			sel.Where(sqljson.ValueEQ(sel.C(posreturn.FieldMetadata), creditNotePending, sqljson.Path(mdFiscalCreditNote)))
		}).
		WithLines().
		Limit(creditNoteRetryBatch).
		All(ctx)
	if err != nil {
		s.log.Warn("return credit note retry: query failed", zap.Error(err))
		return 0
	}
	raised := 0
	for _, ret := range rets {
		// Treasury resolves a tenant id in place of the slug on its S2S routes.
		tenantRef := ret.TenantID.String()
		f := s.fiscalRefundFor(ctx, tenantRef, ret, ret.Edges.Lines)
		if len(f.lines) == 0 {
			s.markFiscalCreditNote(ctx, ret.ID, creditNoteNotNeeded)
			continue
		}
		if s.raiseFiscalCreditNote(ctx, tenantRef, ret, f) {
			s.markFiscalCreditNote(ctx, ret.ID, creditNoteDone)
			raised++
		}
	}
	return raised
}

func (s *Service) markFiscalCreditNote(ctx context.Context, returnID uuid.UUID, state string) {
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
	md[mdFiscalCreditNote] = state
	if _, err := tx.POSReturn.UpdateOne(locked).SetMetadata(md).Save(ctx); err != nil {
		return
	}
	_ = tx.Commit()
}
