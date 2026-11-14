package payments

import (
	"context"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// A desk folio payment taken by an immediate method (cash, sighted M-Pesa code, card on a PDQ)
// is recorded on the folio even when its treasury intent could not be created, so the desk is
// never blocked. Until now nothing ever sent it again: the folio counted it as paid while treasury
// never booked it (S2-20 review M-10). The intent the desk meant to create is kept on the folio
// payment under these keys and RetryPendingFolioIntents sends it again.
const (
	// MetaFolioIntentPending holds the intent request as sent (reference id, amount, method,
	// outlet, metadata); its presence is the pending flag.
	MetaFolioIntentPending = "treasury_intent_pending"
	// MetaFolioIntentAttempts counts failed retries; after maxFolioIntentAttempts the payment is
	// marked MetaFolioIntentFailed and left for staff.
	MetaFolioIntentAttempts = "treasury_intent_attempts"
	MetaFolioIntentFailed   = "treasury_intent_failed"
)

// maxFolioIntentAttempts is about an hour at the 5 minute tick, like the refund retry.
const maxFolioIntentAttempts = 12

// folioIntentRetryEvery and folioIntentRetryBatch pace the retry (variables so tests can shrink them).
var (
	folioIntentRetryEvery = 5 * time.Minute
	folioIntentRetryBatch = 50
)

// PendingFolioIntent is what a folio payment keeps of an intent that could not be created.
func PendingFolioIntent(req treasury.CreateIntentRequest) map[string]any {
	return map[string]any{
		"reference_id":   req.ReferenceID,
		"reference_type": req.ReferenceType,
		"amount":         req.Amount,
		"currency":       req.Currency,
		"payment_method": req.PaymentMethod,
		"description":    req.Description,
		"outlet_id":      req.OutletID,
		"metadata":       req.Metadata,
	}
}

// folioIntentFromMetadata rebuilds the intent request kept by PendingFolioIntent.
func folioIntentFromMetadata(md map[string]any) (treasury.CreateIntentRequest, bool) {
	raw, ok := md[MetaFolioIntentPending].(map[string]any)
	if !ok {
		return treasury.CreateIntentRequest{}, false
	}
	str := func(k string) string { v, _ := raw[k].(string); return v }
	amount, _ := raw["amount"].(float64)
	req := treasury.CreateIntentRequest{
		SourceService: "pos",
		ReferenceID:   str("reference_id"),
		ReferenceType: str("reference_type"),
		Amount:        amount,
		Currency:      str("currency"),
		PaymentMethod: str("payment_method"),
		Description:   str("description"),
		OutletID:      str("outlet_id"),
	}
	if m, ok := raw["metadata"].(map[string]any); ok {
		req.Metadata = m
	}
	if req.ReferenceID == "" || req.Amount <= 0.009 {
		return treasury.CreateIntentRequest{}, false
	}
	if req.Currency == "" {
		req.Currency = "KES"
	}
	return req, true
}

// StartFolioIntentRetry retries pending folio payment intents until ctx ends.
func (s *Service) StartFolioIntentRetry(ctx context.Context) {
	t := time.NewTicker(folioIntentRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.RetryPendingFolioIntents(ctx); n > 0 {
				s.log.Info("hotel folio payments sent to treasury on retry", zap.Int("count", n))
			}
		}
	}
}

// RetryPendingFolioIntents sends the treasury intents of completed folio payments still marked
// pending and returns how many went through. Oldest first, folioIntentRetryBatch at a time. The
// request keeps its original reference id, which is also the idempotency key, so a retry that
// races another pod (or follows a create that did reach treasury) books once. A payment treasury
// keeps refusing is tried maxFolioIntentAttempts times, then marked failed with one error log
// carrying ids only.
func (s *Service) RetryPendingFolioIntents(ctx context.Context) int {
	if s.treasuryClient == nil {
		return 0
	}
	rows, err := s.client.RoomFolioPayment.Query().
		Where(roomfoliopayment.Status("completed"), func(sel *sql.Selector) {
			sel.Where(sqljson.HasKey(sel.C(roomfoliopayment.FieldMetadata), sqljson.Path(MetaFolioIntentPending)))
		}).
		Order(ent.Asc(roomfoliopayment.FieldCreatedAt), ent.Asc(roomfoliopayment.FieldID)).
		Limit(folioIntentRetryBatch).
		All(ctx)
	if err != nil {
		s.log.Warn("folio intent retry: query failed", zap.Error(err))
		return 0
	}
	sent := 0
	for _, p := range rows {
		req, ok := folioIntentFromMetadata(p.Metadata)
		if !ok {
			s.updateFolioPaymentMeta(ctx, p, func(md map[string]any) {
				delete(md, MetaFolioIntentPending)
				delete(md, MetaFolioIntentAttempts)
			}, "")
			continue
		}
		// Treasury resolves a tenant id in place of the slug on its S2S routes.
		intent, err := s.treasuryClient.CreateIntent(ctx, p.TenantID.String(), req.ReferenceID, req)
		if err != nil {
			attempts := intAttempts(p.Metadata[MetaFolioIntentAttempts]) + 1
			s.updateFolioPaymentMeta(ctx, p, func(md map[string]any) {
				md[MetaFolioIntentAttempts] = attempts
				if attempts >= maxFolioIntentAttempts {
					delete(md, MetaFolioIntentPending)
					md[MetaFolioIntentFailed] = true
				}
			}, "")
			if attempts >= maxFolioIntentAttempts {
				s.log.Error("folio intent retry: gave up, the payment is on the folio but not in treasury",
					zap.String("tenant_id", p.TenantID.String()), zap.String("folio_payment_id", p.ID.String()),
					zap.Int("attempts", attempts), zap.Error(err))
			}
			continue
		}
		sent++
		s.updateFolioPaymentMeta(ctx, p, func(md map[string]any) {
			delete(md, MetaFolioIntentPending)
			delete(md, MetaFolioIntentAttempts)
		}, intent.ResolvedID())
	}
	return sent
}

// updateFolioPaymentMeta rewrites a folio payment's metadata (and its treasury intent id when one
// is given). Best effort: a lost write only means the payment is retried again, and the reference
// id keeps that idempotent.
func (s *Service) updateFolioPaymentMeta(ctx context.Context, p *ent.RoomFolioPayment, edit func(map[string]any), intentID string) {
	md := make(map[string]any, len(p.Metadata)+1)
	for k, v := range p.Metadata {
		md[k] = v
	}
	edit(md)
	upd := s.client.RoomFolioPayment.UpdateOneID(p.ID).SetMetadata(md)
	if intentID != "" {
		upd = upd.SetTreasuryIntentID(intentID)
	}
	if err := upd.Exec(ctx); err != nil {
		s.log.Warn("folio intent retry: metadata update failed",
			zap.String("folio_payment_id", p.ID.String()), zap.Error(err))
	}
}

// intAttempts reads an attempt counter stored as a JSON number.
func intAttempts(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}
