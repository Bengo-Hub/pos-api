// Payment management — the All-Sales "View Payments" modal actions: detailed listing
// (tender name/type + note), limited edit (never the amount), soft VOID with treasury
// reversal, and the payment-received customer notification. Kept out of service.go per
// the modular file-size rule.
package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// StatusVoided marks a payment administratively voided — excluded from paid_total (which
// sums only StatusCompleted) but kept for audit.
const StatusVoided = "voided"

// PaymentDetail is a POSPayment enriched with its tender label + note for the View
// Payments modal (the raw row only carries tender_id and a JSON blob).
type PaymentDetail struct {
	*ent.POSPayment
	TenderName string `json:"tender_name"`
	TenderType string `json:"tender_type"`
	Note       string `json:"note,omitempty"`
	// Voidable tells the UI whether the void/edit actions apply (manual tenders only —
	// gateway-settled payments must go through the refund flow).
	Voidable bool `json:"voidable"`
}

// UpdatePaymentInput edits a payment's descriptive fields. The AMOUNT is deliberately not
// editable — changing money means voiding the row and recording a new payment, so
// paid_total and the treasury books stay consistent.
type UpdatePaymentInput struct {
	Reference  *string    `json:"reference,omitempty"`
	Note       *string    `json:"note,omitempty"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
	TenderID   *uuid.UUID `json:"tender_id,omitempty"` // switch method by pointing at another tender
}

// manualTenderTypes are the tender types recorded at the till without an online gateway
// capture — the only ones whose rows may be edited/voided here. Gateway payments (card,
// M-Pesa STK, Paystack) and structural tenders (on_account AR, loyalty points) have
// cross-service state and must go through the refund / returns flows instead.
var manualTenderTypes = map[string]bool{
	"cash": true, "manual": true, "card_manual": true, "pdq": true, "card_terminal": true,
	"cheque": true, "bank_transfer": true,
}

// paymentIsManageable reports whether the payment may be edited/voided at the till.
func paymentIsManageable(p *ent.POSPayment, tenderType string) bool {
	if p.PaymentData != nil {
		if via, _ := p.PaymentData["settled_via"].(string); via == "treasury_gateway" {
			return false
		}
	}
	return manualTenderTypes[tenderType]
}

// ListOrderPaymentsDetailed returns an order's payments with tender labels + notes.
func (s *Service) ListOrderPaymentsDetailed(ctx context.Context, tenantID, orderID uuid.UUID) ([]PaymentDetail, error) {
	rows, err := s.ListOrderPayments(ctx, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	tenderIDs := make([]uuid.UUID, 0, len(rows))
	for _, p := range rows {
		tenderIDs = append(tenderIDs, p.TenderID)
	}
	tenderByID := map[uuid.UUID]*ent.Tender{}
	if len(tenderIDs) > 0 {
		if tenders, terr := s.client.Tender.Query().Where(tender.IDIn(tenderIDs...)).All(ctx); terr == nil {
			for _, t := range tenders {
				tenderByID[t.ID] = t
			}
		}
	}
	out := make([]PaymentDetail, 0, len(rows))
	for _, p := range rows {
		d := PaymentDetail{POSPayment: p}
		if t, ok := tenderByID[p.TenderID]; ok {
			d.TenderName = t.Name
			d.TenderType = t.Type
		}
		if p.PaymentData != nil {
			d.Note, _ = p.PaymentData["note"].(string)
			// The default (nil-UUID) tender has no Tender row — fall back to the method
			// recorded at capture time so on-account / M-Pesa rows never render as blank/Cash.
			if d.TenderType == "" {
				d.TenderType, _ = p.PaymentData["method"].(string)
			}
		}
		d.Voidable = p.Status == StatusCompleted && paymentIsManageable(p, d.TenderType)
		out = append(out, d)
	}
	return out, nil
}

// UpdatePayment edits a payment's descriptive fields (reference / note / date / tender).
// Manual tenders only; voided rows are immutable.
func (s *Service) UpdatePayment(ctx context.Context, tenantID, orderID, paymentID uuid.UUID, in UpdatePaymentInput) (*ent.POSPayment, error) {
	p, tenderType, err := s.loadManagedPayment(ctx, tenantID, orderID, paymentID)
	if err != nil {
		return nil, err
	}
	if p.Status == StatusVoided {
		return nil, fmt.Errorf("payments: cannot edit a voided payment")
	}
	if !paymentIsManageable(p, tenderType) {
		return nil, fmt.Errorf("payments: only manually-recorded payments can be edited — gateway payments go through the refund flow")
	}

	upd := s.client.POSPayment.UpdateOne(p)
	if in.Reference != nil {
		upd.SetExternalReference(*in.Reference)
	}
	if in.OccurredAt != nil && !in.OccurredAt.IsZero() {
		upd.SetOccurredAt(*in.OccurredAt)
	}
	if in.TenderID != nil && *in.TenderID != uuid.Nil {
		// The replacement tender must exist for this tenant and itself be a manual type.
		t, terr := s.client.Tender.Query().
			Where(tender.ID(*in.TenderID), tender.TenantID(tenantID)).
			Only(ctx)
		if terr != nil {
			return nil, fmt.Errorf("payments: tender not found")
		}
		if !manualTenderTypes[t.Type] {
			return nil, fmt.Errorf("payments: cannot reassign a payment to a gateway tender")
		}
		upd.SetTenderID(t.ID)
	}
	if in.Note != nil {
		data := p.PaymentData
		if data == nil {
			data = map[string]any{}
		}
		data["note"] = *in.Note
		upd.SetPaymentData(data)
	}
	saved, err := upd.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: update payment: %w", err)
	}
	s.recordOrderEvent(ctx, orderID, "payment.updated", uuid.Nil, map[string]any{
		"payment_id": paymentID.String(),
	})
	return saved, nil
}

// VoidPayment soft-voids a completed manual payment: local row → voided, paid_total
// recomputed (a completed order that is no longer covered reopens to pending_payment), an
// order audit event is written, and treasury receives a best-effort refund reversal keyed
// on the payment id (idempotent) so the cash books don't drift.
func (s *Service) VoidPayment(ctx context.Context, tenantID uuid.UUID, tenantSlug string, orderID, paymentID, voidedBy uuid.UUID, reason string) (*ent.POSPayment, error) {
	p, tenderType, err := s.loadManagedPayment(ctx, tenantID, orderID, paymentID)
	if err != nil {
		return nil, err
	}
	if p.Status == StatusVoided {
		return p, nil // idempotent
	}
	if p.Status != StatusCompleted {
		return nil, fmt.Errorf("payments: only completed payments can be voided")
	}
	if !paymentIsManageable(p, tenderType) {
		return nil, fmt.Errorf("payments: only manually-recorded payments can be voided — gateway payments go through the refund flow")
	}

	data := p.PaymentData
	if data == nil {
		data = map[string]any{}
	}
	data["void_reason"] = reason
	data["voided_by"] = voidedBy.String()
	data["voided_at"] = time.Now().Format(time.RFC3339)
	voided, err := s.client.POSPayment.UpdateOne(p).
		SetStatus(StatusVoided).
		SetPaymentData(data).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: void payment: %w", err)
	}

	paid, err := s.RecomputePaidTotal(ctx, orderID)
	if err != nil {
		s.log.Warn("void payment: recompute paid_total failed", zap.Error(err))
	}

	// A completed order whose payments no longer cover the total is reopened so the
	// balance can be re-collected. This is an administrative correction, so it bypasses
	// the normal forward-only transition table (completed → pending_payment).
	order, oerr := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		Only(ctx)
	if oerr == nil && order.Status == orders.StatusCompleted && paid+0.01 < order.TotalAmount {
		if _, uerr := s.client.POSOrder.UpdateOne(order).
			SetStatus(orders.StatusPendingPayment).
			Save(ctx); uerr != nil {
			s.log.Warn("void payment: reopen order failed", zap.Error(uerr))
		}
	}

	s.recordOrderEvent(ctx, orderID, "payment.voided", voidedBy, map[string]any{
		"payment_id": paymentID.String(),
		"amount":     p.Amount,
		"reason":     reason,
	})

	// Reverse the money in treasury's books. Manual/cash POS payments were settled there as
	// immediate intents, so a local-only void would leave a Dr-Cash entry with no cash.
	// Idempotent on the payment id; failures are logged, never fatal (reconciler catches up).
	if s.treasuryClient != nil && tenantSlug != "" {
		if _, rerr := s.treasuryClient.CreateRefund(ctx, tenantSlug, paymentID.String(), treasury.RefundRequest{
			SourceService:    "pos",
			ReferenceID:      paymentID.String(),
			ReferenceType:    "pos_payment_void",
			OriginalIntentID: p.ExternalReference,
			Amount:           p.Amount,
			Currency:         p.Currency,
			Reason:           "POS payment voided: " + reason,
			RefundChannel:    "cash",
		}); rerr != nil {
			s.log.Warn("void payment: treasury reversal failed — books may need reconciliation",
				zap.String("payment_id", paymentID.String()), zap.Error(rerr))
		}
	}
	return voided, nil
}

// NotifyPaymentReceived publishes the payment-received customer notification for one
// payment (the View Payments modal action). Delivery is notifications-service's job.
func (s *Service) NotifyPaymentReceived(ctx context.Context, tenantID, orderID, paymentID uuid.UUID) error {
	p, tenderType, err := s.loadManagedPayment(ctx, tenantID, orderID, paymentID)
	if err != nil {
		return err
	}
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return fmt.Errorf("payments: order not found: %w", err)
	}
	if s.publisher == nil {
		return fmt.Errorf("payments: event publisher not configured")
	}
	name, phone := "", ""
	if order.CustomerName != nil {
		name = *order.CustomerName
	}
	if order.CustomerPhone != nil {
		phone = *order.CustomerPhone
	}
	return s.publisher.PublishPaymentReceivedNotification(ctx, tenantID, map[string]any{
		"order_id":       orderID.String(),
		"order_number":   order.OrderNumber,
		"tenant_id":      tenantID.String(),
		"outlet_id":      order.OutletID.String(),
		"payment_id":     paymentID.String(),
		"amount":         p.Amount,
		"currency":       p.Currency,
		"method":         tenderType,
		"reference":      p.ExternalReference,
		"paid_at":        p.OccurredAt,
		"total_amount":   order.TotalAmount,
		"paid_total":     order.PaidTotal,
		"customer_name":  name,
		"customer_phone": phone,
	})
}

// loadManagedPayment loads a payment scoped to (tenant, order) and resolves its tender type.
func (s *Service) loadManagedPayment(ctx context.Context, tenantID, orderID, paymentID uuid.UUID) (*ent.POSPayment, string, error) {
	exists, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		Exist(ctx)
	if err != nil || !exists {
		return nil, "", fmt.Errorf("payments: order not found")
	}
	p, err := s.client.POSPayment.Query().
		Where(pospayment.ID(paymentID), pospayment.OrderID(orderID)).
		Only(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("payments: payment not found")
	}
	tenderType := ""
	if t, terr := s.client.Tender.Query().Where(tender.ID(p.TenderID)).Only(ctx); terr == nil {
		tenderType = t.Type
	}
	return p, tenderType, nil
}

// recordOrderEvent writes a POSOrderEvent audit row; best-effort.
func (s *Service) recordOrderEvent(ctx context.Context, orderID uuid.UUID, eventType string, actorID uuid.UUID, payload map[string]any) {
	create := s.client.POSOrderEvent.Create().
		SetOrderID(orderID).
		SetEventType(eventType).
		SetPayload(payload)
	if actorID != uuid.Nil {
		create.SetActorID(actorID)
	}
	if _, err := create.Save(ctx); err != nil {
		s.log.Warn("record order event failed", zap.String("event", eventType), zap.Error(err))
	}
}
