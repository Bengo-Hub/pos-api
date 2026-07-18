package orders

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsstation"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/modules/printing"
)

// enqueueAutoPrintJobs pushes the order's kitchen/bar station tickets and (optionally) the
// customer bill onto the background print queue for the outlet's Local Print Agent, honouring the
// outlet's auto_print_kitchen / auto_print_order settings.
//
// It enqueues ONLY when a paired agent is currently online — otherwise the till's client-side
// silent transports (QZ / loopback agent relay) keep working exactly as before, and we avoid
// filling the queue with jobs that would only expire. Never fatal: printing must not fail orders.
//
// Dedupe keys make this idempotent per (order, ticket, printer) so a retried create/replayed
// offline sale never double-prints.
func (s *Service) enqueueAutoPrintJobs(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder) {
	if s.printQueue == nil || order == nil {
		return
	}

	// Cheapest gate first: most outlets have no paired agent, so the common case costs exactly
	// one index-backed EXISTS per order — settings/lines/stations load only when spooling is on.
	if !s.printQueue.AgentOnline(ctx, tenantID, order.OutletID) {
		return
	}

	setting, err := s.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(order.OutletID)).
		Only(ctx)
	if err != nil || setting == nil {
		return
	}
	if !setting.AutoPrintKitchen && !setting.AutoPrintOrder {
		return
	}

	profiles := printing.ProfilesFromRaw(setting.PrinterProfiles)
	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(order.ID)).
		All(ctx)
	if err != nil || len(lines) == 0 {
		return
	}

	// Kitchen/bar station tickets — same routing as KDS tickets.
	if setting.AutoPrintKitchen {
		s.enqueueStationTickets(ctx, tenantID, order, profiles, lines, "", "")
	}

	// Customer bill (dine-in pro-forma) — owned here so the till can log the waiter out instantly.
	if setting.AutoPrintOrder {
		if profile := printing.ResolveBillProfile(profiles); profile != nil {
			outlet, _ := s.client.Outlet.Query().Where(entoutlet.ID(order.OutletID)).Only(ctx)
			servedBy := printing.ServedByFromContext(ctx)
			payload := printing.BuildReceipt(printing.OrderReceiptData(order, lines, outlet, setting, "customer", "", servedBy, ""))
			s.enqueueJob(ctx, tenantID, order, "bill", profile, payload,
				fmt.Sprintf("%s:bill:%s", order.ID, profile.ID))
		}
	}
}

// enqueueStationTickets routes the given lines to the outlet's active KDS stations and
// enqueues one kitchen/bar chit per station that received items. batchTag disambiguates
// the dedupe key: "" for the order-create pass (one chit per order+station, retry-safe),
// a stable per-batch value (e.g. the first added line's ID) for delta chits so a SECOND
// add-to-bill on the same order still prints while a replay of the SAME batch dedupes.
// banner is stamped on the chit ("*** ADDITIONAL ITEMS ***" / "*** COURSE N FIRED ***").
func (s *Service) enqueueStationTickets(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, profiles []printing.PrinterProfile, lines []*ent.POSOrderLine, batchTag, banner string) {
	stations, _ := s.client.KDSStation.Query().
		Where(
			kdsstation.TenantID(tenantID),
			kdsstation.OutletID(order.OutletID),
			kdsstation.IsActive(true),
		).
		All(ctx)
	if len(stations) == 0 {
		return
	}
	stationItems := routeLinesToStations(lines, stations)
	for _, station := range stations {
		items := stationItems[station.ID]
		if len(items) == 0 {
			continue
		}
		profile := printing.ProfileForStation(profiles, station.ID.String())
		if profile == nil {
			continue // station has no real printer assigned — KDS screen (or client path) covers it
		}
		jobType := "kitchen"
		if station.StationType == "bar" {
			jobType = "bar"
		}
		payload := printing.BuildReceipt(printing.StationTicketDataWithBanner(order, station.Name, items, banner))
		dedupe := fmt.Sprintf("%s:%s:%s", order.ID, jobType, station.ID)
		if batchTag != "" {
			dedupe += ":" + batchTag
		}
		s.enqueueJob(ctx, tenantID, order, jobType, profile, payload, dedupe)
	}
}

// enqueueStationTicketsForLines pushes DELTA kitchen/bar chits for ONLY the given lines
// (add-to-bill / course fire) onto the background print queue — the fix for the "no new
// docket when items are added to an open bill" incident: without a delta chit, waiters
// reprinted the FULL bill and stations re-prepared the whole order. Same gates as
// enqueueAutoPrintJobs (agent online + auto_print_kitchen); the customer bill is never
// reprinted here. Never fatal: printing must not fail order mutations.
func (s *Service) enqueueStationTicketsForLines(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, lines []*ent.POSOrderLine, batchTag, banner string) {
	if s.printQueue == nil || order == nil || len(lines) == 0 {
		return
	}
	if !s.printQueue.AgentOnline(ctx, tenantID, order.OutletID) {
		return
	}
	setting, err := s.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(order.OutletID)).
		Only(ctx)
	if err != nil || setting == nil || !setting.AutoPrintKitchen {
		return
	}
	s.enqueueStationTickets(ctx, tenantID, order, printing.ProfilesFromRaw(setting.PrinterProfiles), lines, batchTag, banner)
}

// enqueueJob enqueues one job, logging (never propagating) failures.
func (s *Service) enqueueJob(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, jobType string, profile *printing.PrinterProfile, payload []byte, dedupe string) {
	_, err := s.printQueue.Enqueue(ctx, printing.EnqueueInput{
		TenantID:  tenantID,
		OutletID:  order.OutletID,
		OrderID:   &order.ID,
		JobType:   jobType,
		Target:    printing.TargetFromProfile(profile),
		Payload:   payload,
		DedupeKey: dedupe,
	})
	if err != nil {
		s.log.Warn("orders: print job enqueue failed",
			zap.String("order_id", order.ID.String()),
			zap.String("job_type", jobType),
			zap.Error(err))
	}
}
