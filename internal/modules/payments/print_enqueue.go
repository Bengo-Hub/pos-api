package payments

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	outletsettingpredicate "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/modules/printing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// enqueueReceiptPrint pushes the FINAL customer receipt (with payment method) onto the background
// print queue once the order is fully paid, when the outlet auto-prints and a Local Print Agent is
// online. Deduped per order+printer, so replayed confirmations never double-print. Never fatal.
func (s *Service) enqueueReceiptPrint(ctx context.Context, order *ent.POSOrder) {
	if s.printQueue == nil || order == nil {
		return
	}

	// Cheapest gate first (one index-backed EXISTS) — most outlets have no paired agent.
	if !s.printQueue.AgentOnline(ctx, order.TenantID, order.OutletID) {
		return
	}

	setting, err := s.client.OutletSetting.Query().
		Where(outletsettingpredicate.OutletID(order.OutletID)).
		Only(ctx)
	if err != nil || setting == nil || !setting.AutoPrintOrder {
		return
	}

	profiles := printing.ProfilesFromRaw(setting.PrinterProfiles)
	profile := printing.ResolveBillProfile(profiles)
	if profile == nil {
		return
	}

	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(order.ID)).
		All(ctx)
	if err != nil || len(lines) == 0 {
		return
	}

	// Payment method label: the completed tenders on the order (e.g. "Cash", "M-Pesa + Cash"),
	// plus the total settled amount and last settle time so the printed receipt can show
	// Amount Paid / "Cash (14-07-2026)" / balance due like the browser one.
	methods := make([]string, 0, 2)
	seen := map[string]struct{}{}
	var amountPaid float64
	var paymentDate *time.Time
	if pays, perr := s.client.POSPayment.Query().
		Where(pospayment.OrderID(order.ID), pospayment.Status(StatusCompleted)).
		All(ctx); perr == nil {
		// Preload the referenced tenders in ONE query (was a Tender.Get per payment — N+1).
		tenderName := make(map[uuid.UUID]string, len(pays))
		ids := make([]uuid.UUID, 0, len(pays))
		for _, p := range pays {
			ids = append(ids, p.TenderID)
		}
		if len(ids) > 0 {
			if ts, terr := s.client.Tender.Query().Where(tender.IDIn(ids...)).All(ctx); terr == nil {
				for _, t := range ts {
					tenderName[t.ID] = t.Name
				}
			}
		}
		for _, p := range pays {
			amountPaid += p.Amount
			if paymentDate == nil || p.OccurredAt.After(*paymentDate) {
				occurred := p.OccurredAt
				paymentDate = &occurred
			}
			if name := tenderName[p.TenderID]; name != "" {
				if _, dup := seen[name]; !dup {
					seen[name] = struct{}{}
					methods = append(methods, name)
				}
			}
		}
	}

	outlet, _ := s.client.Outlet.Query().Where(entoutlet.ID(order.OutletID)).Only(ctx)
	servedBy := printing.ServedByFromContext(ctx)
	payload := printing.BuildReceipt(printing.OrderReceiptDataOpts(
		order, lines, outlet, setting, printing.ReceiptViewOpts{
			Type:          "customer",
			PaymentMethod: strings.Join(methods, " + "),
			ServedBy:      servedBy,
			AmountPaid:    amountPaid,
			PaymentDate:   paymentDate,
		}))
	_, err = s.printQueue.Enqueue(ctx, printing.EnqueueInput{
		TenantID:  order.TenantID,
		OutletID:  order.OutletID,
		OrderID:   &order.ID,
		JobType:   "receipt",
		Target:    printing.TargetFromProfile(profile),
		Payload:   payload,
		DedupeKey: fmt.Sprintf("%s:receipt:%s", order.ID, profile.ID),
	})
	if err != nil {
		s.log.Warn("payments: receipt print enqueue failed",
			zap.String("order_id", order.ID.String()), zap.Error(err))
	}
}
