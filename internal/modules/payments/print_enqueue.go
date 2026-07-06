package payments

import (
	"context"
	"fmt"
	"strings"

	"github.com/bengobox/pos-service/internal/ent"
	outletsettingpredicate "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/modules/printing"

	"go.uber.org/zap"
)

// enqueueReceiptPrint pushes the FINAL customer receipt (with payment method) onto the background
// print queue once the order is fully paid, when the outlet auto-prints and a Local Print Agent is
// online. Deduped per order+printer, so replayed confirmations never double-print. Never fatal.
func (s *Service) enqueueReceiptPrint(ctx context.Context, order *ent.POSOrder) {
	if s.printQueue == nil || order == nil {
		return
	}

	setting, err := s.client.OutletSetting.Query().
		Where(outletsettingpredicate.OutletID(order.OutletID)).
		Only(ctx)
	if err != nil || setting == nil || !setting.AutoPrintOrder {
		return
	}
	if !s.printQueue.AgentOnline(ctx, order.TenantID, order.OutletID) {
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

	// Payment method label: the completed tenders on the order (e.g. "Cash", "M-Pesa + Cash").
	methods := make([]string, 0, 2)
	seen := map[string]struct{}{}
	if pays, perr := s.client.POSPayment.Query().
		Where(pospayment.OrderID(order.ID), pospayment.Status(StatusCompleted)).
		All(ctx); perr == nil {
		for _, p := range pays {
			if t, tErr := s.client.Tender.Get(ctx, p.TenderID); tErr == nil && t.Name != "" {
				if _, dup := seen[t.Name]; !dup {
					seen[t.Name] = struct{}{}
					methods = append(methods, t.Name)
				}
			}
		}
	}

	payload := printing.BuildReceipt(printing.OrderReceiptData(
		order, lines, setting, "customer", strings.Join(methods, " + "), ""))
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
