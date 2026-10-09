package orders

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
	"github.com/bengobox/pos-service/internal/modules/printing"
	"github.com/bengobox/pos-service/internal/modules/providerfooter"
)

// enqueueAutoPrintJobs pushes the order's kitchen/bar station tickets and (optionally) the
// customer bill onto the background print queue for the outlet's Local Print Agent, honouring the
// outlet's auto_print_kitchen / auto_print_order switches and each printer card's own Auto-print
// toggle (printing.AutoBillProfile / AutoStationProfile).
//
// It enqueues ONLY when a paired agent is currently online — otherwise the till's client-side
// silent transports (QZ / loopback agent relay) keep working exactly as before, and we avoid
// filling the queue with jobs that would only expire. Never fatal: printing must not fail orders.
//
// Dedupe keys make this idempotent per (order, ticket, printer) so a retried create/replayed
// offline sale never double-prints.
//
// stationChits is false for orders that never go to a kitchen (a shop's collect or delivery
// order): only the customer bill prints, as the counter's pick list.
func (s *Service) enqueueAutoPrintJobs(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, stationChits bool) {
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
	profiles := printing.ProfilesFromRaw(setting.PrinterProfiles)
	printChits := stationChits && setting.AutoPrintKitchen
	// The posted bill follows the same gate as the paid receipt: outlet auto_print_order AND the
	// bill printer's own Auto-print toggle (it used to ignore the toggle).
	billProfile := printing.AutoBillProfile(setting.AutoPrintOrder, profiles)
	if !printChits && billProfile == nil {
		return
	}

	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(order.ID)).
		WithModifiers().
		All(ctx)
	if err != nil || len(lines) == 0 {
		return
	}

	// Kitchen/bar station tickets — same routing as KDS tickets.
	if printChits {
		s.enqueueStationTickets(ctx, tenantID, order, profiles, lines, "", "")
	}

	// Customer bill (dine-in pro-forma) — owned here so the till can log the waiter out instantly.
	if billProfile != nil {
		outlet, _ := s.client.Outlet.Query().Where(entoutlet.ID(order.OutletID)).Only(ctx)
		servedBy := printing.ServedByFromContext(ctx)
		tenantName := ""
		if t, terr := s.client.Tenant.Query().Where(enttenant.ID(tenantID)).Only(ctx); terr == nil {
			tenantName = t.Name
		}
		rdata := printing.OrderReceiptData(order, lines, outlet, setting, "customer", "", servedBy, "", tenantName)
		rdata.ShowProviderFooter = providerfooter.Resolve(ctx, s.client, tenantID)
		payload := printing.BuildReceipt(rdata)
		s.enqueueJob(ctx, tenantID, order, "bill", billProfile, payload,
			fmt.Sprintf("%s:bill:%s", order.ID, billProfile.ID))
	}
}

// enqueueStationTickets routes the given lines to the outlet's active KDS stations and
// enqueues one kitchen/bar chit per station that received items. batchTag disambiguates
// the dedupe key: "" for the order-create pass (one chit per order+station, retry-safe),
// a stable per-batch value (e.g. the first added line's ID) for delta chits so a SECOND
// add-to-bill on the same order still prints while a replay of the SAME batch dedupes.
// banner is stamped on the chit ("*** ADDITIONAL ITEMS ***" / "*** COURSE N FIRED ***").
func (s *Service) enqueueStationTickets(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, profiles []printing.PrinterProfile, lines []*ent.POSOrderLine, batchTag, banner string) {
	router := s.kdsRouter(ctx, tenantID, order.OutletID)
	stations := router.Stations()
	if len(stations) == 0 {
		return
	}
	stationItems := routeLinesToStations(lines, router)
	// Chits print the order time in the outlet's own timezone, the same as receipts.
	loc := s.outletLocation(ctx, order.OutletID)
	for _, station := range stations {
		items := stationItems[station.ID]
		if len(items) == 0 {
			continue
		}
		// Real printer with its card's Auto-print toggle on (the till's own chit path already
		// skipped a station whose toggle is off; this server path did not).
		profile := printing.AutoStationProfile(profiles, station.ID.String())
		if profile == nil {
			continue // no real printer, or auto-print off for this station: the KDS screen covers it
		}
		jobType := "kitchen"
		if station.StationType == "bar" {
			jobType = "bar"
		}
		payload := printing.BuildReceipt(printing.StationTicketDataWithBanner(order, station.Name, items, banner, loc))
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
	if wf := s.orderWorkflow(ctx, order.OutletID, string(order.OrderSubtype)); !wf.Kitchen && !wf.Production {
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

// outletLocation is the outlet's display timezone for printed chits (printing.OutletLocation:
// outlet timezone, default Africa/Nairobi).
func (s *Service) outletLocation(ctx context.Context, outletID uuid.UUID) *time.Location {
	outlet, _ := s.client.Outlet.Query().Where(entoutlet.ID(outletID)).Only(ctx)
	return printing.OutletLocation(outlet)
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
