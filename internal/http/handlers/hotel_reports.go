package handlers

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// roomGuestNightInterval computes the whole calendar nights (never a fraction of one — see
// HotelOccupancyReport's doc comment) a guest's stay occupies within [windowStart, windowEnd),
// both already day-aligned. Shared by the aggregate occupancy report and the daily trend
// endpoint so the two never disagree about what counts as an occupied night.
func roomGuestNightInterval(g *ent.RoomGuest, windowStart, windowEnd time.Time, loc *time.Location) (start, end time.Time) {
	ciDate := startOfDayIn(g.CheckInDate, loc)
	var coDate time.Time
	if g.Status == entroomguest.StatusCheckedOut && g.CheckedOutAt != nil {
		// The departure calendar day itself is excluded -- the guest didn't sleep there that
		// night, same convention as the checkout nights calculation.
		coDate = startOfDayIn(*g.CheckedOutAt, loc)
	} else {
		// Still in-house: sold through tonight even if the scheduled departure date is
		// further out, but never past the scheduled checkout.
		coDate = startOfDayIn(g.CheckOutDate, loc)
		if coDate.After(windowEnd) {
			coDate = windowEnd
		}
	}
	start, end = ciDate, coDate
	if start.Before(windowStart) {
		start = windowStart
	}
	if end.After(windowEnd) {
		end = windowEnd
	}
	return start, end
}

// chargeTypeRevenue is one line of the revenue-by-charge-type breakdown a hotel folio's
// RoomFolioItem.charge_type enum already carries — see internal/ent/schema/roomfolioitem.go.
type chargeTypeRevenue struct {
	ChargeType string  `json:"charge_type"`
	Amount     float64 `json:"amount"`
}

// hotelOccupancyResult is the response body for GET /reports/hotel-occupancy.
type hotelOccupancyResult struct {
	From                string  `json:"from"`
	To                  string  `json:"to"`
	TotalRooms          int     `json:"total_rooms"`
	AvailableRoomNights float64 `json:"available_room_nights"`
	OccupiedRoomNights  float64 `json:"occupied_room_nights"`
	OccupancyRate       float64 `json:"occupancy_rate"` // 0..1
	RoomRevenue         float64 `json:"room_revenue"`
	AncillaryRevenue    float64 `json:"ancillary_revenue"`
	TotalRevenue        float64 `json:"total_revenue"`
	// ADR (Average Daily Rate) = room revenue / occupied room-nights. RevPAR (Revenue Per
	// Available Room) = room revenue / available room-nights (equivalently occupancy_rate * ADR).
	// Both standard hospitality KPIs — see https://en.wikipedia.org/wiki/RevPAR.
	ADR             float64             `json:"adr"`
	RevPAR          float64             `json:"revpar"`
	RevenueByCharge []chargeTypeRevenue `json:"revenue_by_charge_type"`
}

// HotelOccupancyReport handles GET /{tenantID}/pos/reports/hotel-occupancy — occupancy %, ADR,
// RevPAR, and a room-vs-ancillary revenue split for the requested date range (default: last 30
// days, same as every other analytics report — see parseDateRange) and outlet (parseOutletFilter;
// omitting outlet_id/X-Outlet-ID reports across every outlet the tenant runs hotel rooms in).
//
// Route-gated the same as the rest of the hotel module (RequireUseCase("hospitality") +
// RequireFeature(FeatureHotelModule) — see router.go's /hotel group), so this only ever runs for
// tenants actually entitled to and running the hotel module.
//
// Occupied room-nights are computed by overlapping each RoomGuest stay's calendar nights
// ([check_in_date, checked_out_at-or-check_out_date), truncated to whole days) with the
// requested window (also truncated to whole days, inclusive of the window's own end date) —
// a stay spanning the window boundary contributes only the nights that actually fall inside it,
// and a guest still checked in as of the window's end date always counts as occupying that
// final night in full, regardless of what time of day the report happens to run.
// Revenue is recognized by RoomFolioItem.created_at falling inside the window (the same folio
// items GL posting will eventually itemize by charge_type — see
// D:\Projects\Codevertex\.claude\plans\boi-multi-use-case-subscription-and-hospitality-audit-2026-08-18.md).
func (h *ReportsHandler) HotelOccupancyReport(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	loc := requestTenantLocation(r, h.db)
	from, to := parseDateRange(r, loc)
	if !to.After(from) {
		jsonError(w, "to must be after from", http.StatusBadRequest)
		return
	}

	roomQuery := h.db.Room.Query().Where(entroom.TenantID(tid))
	if outletFilter := parseOutletFilter(r); outletFilter != uuid.Nil {
		roomQuery = roomQuery.Where(entroom.OutletID(outletFilter))
	}
	rooms, err := roomQuery.All(r.Context())
	if err != nil {
		h.log.Error("hotel occupancy: room query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	totalRooms := len(rooms)
	roomIDs := make([]uuid.UUID, totalRooms)
	for i, rm := range rooms {
		roomIDs[i] = rm.ID
	}

	// Room-nights are whole calendar nights, never a fraction of one -- mirrors how "nights" is
	// computed everywhere else in the hotel module (the folio's Nights field, the frontend's
	// calendarDaysBetween, RoomNightlyBillingScheduler's elapsedDays+1). Using raw wall-clock
	// hours/24 here previously counted a guest who checked in minutes ago as ~0.002 "nights"
	// occupied even though a full night's charge had already posted -- with real folio revenue
	// on top, ADR (= room_revenue / occupied_room_nights) could explode into the millions. The
	// window is clamped to day boundaries (windowEnd includes the report's own "to" date in full,
	// i.e. "through tonight") so a guest who is still in-house as of the report's end date always
	// counts as occupying tonight's room-night, regardless of what hour "now" happens to be.
	windowStart := startOfDayIn(from, loc)
	windowEnd := startOfDayIn(to, loc).AddDate(0, 0, 1)
	periodDays := windowEnd.Sub(windowStart).Hours() / 24
	availableRoomNights := float64(totalRooms) * periodDays

	var occupiedRoomNights float64
	if len(roomIDs) > 0 {
		guests, gerr := h.db.RoomGuest.Query().
			Where(
				entroomguest.TenantID(tid),
				entroomguest.RoomIDIn(roomIDs...),
				entroomguest.CheckInDateLT(windowEnd),
			).
			All(r.Context())
		if gerr != nil {
			h.log.Error("hotel occupancy: room guest query failed", zap.Error(gerr))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, g := range guests {
			start, end := roomGuestNightInterval(g, windowStart, windowEnd, loc)
			if end.After(start) {
				occupiedRoomNights += end.Sub(start).Hours() / 24
			}
		}
	}

	revenueByType := map[string]float64{}
	if len(roomIDs) > 0 {
		items, ferr := h.db.RoomFolioItem.Query().
			Where(
				entroomfolioitem.TenantID(tid),
				entroomfolioitem.RoomIDIn(roomIDs...),
				entroomfolioitem.CreatedAtGTE(from),
				entroomfolioitem.CreatedAtLT(to),
			).
			All(r.Context())
		if ferr != nil {
			h.log.Error("hotel occupancy: folio item query failed", zap.Error(ferr))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, it := range items {
			revenueByType[string(it.ChargeType)] += it.Amount
		}
	}

	roomRevenue := revenueByType[string(entroomfolioitem.ChargeTypeRoomCharge)]
	var totalRevenue float64
	breakdown := make([]chargeTypeRevenue, 0, len(revenueByType))
	for ct, amt := range revenueByType {
		totalRevenue += amt
		breakdown = append(breakdown, chargeTypeRevenue{ChargeType: ct, Amount: amt})
	}
	ancillaryRevenue := totalRevenue - roomRevenue

	result := hotelOccupancyResult{
		From:                from.Format(time.RFC3339),
		To:                  to.Format(time.RFC3339),
		TotalRooms:          totalRooms,
		AvailableRoomNights: availableRoomNights,
		OccupiedRoomNights:  occupiedRoomNights,
		RoomRevenue:         roomRevenue,
		AncillaryRevenue:    ancillaryRevenue,
		TotalRevenue:        totalRevenue,
		RevenueByCharge:     breakdown,
	}
	if availableRoomNights > 0 {
		result.OccupancyRate = occupiedRoomNights / availableRoomNights
		result.RevPAR = roomRevenue / availableRoomNights
	}
	if occupiedRoomNights > 0 {
		result.ADR = roomRevenue / occupiedRoomNights
	}

	jsonOK(w, result)
}

// hotelTrendBucket is one calendar day of the occupancy/revenue trend chart.
type hotelTrendBucket struct {
	Date                string  `json:"date"` // YYYY-MM-DD, tenant-local
	AvailableRoomNights float64 `json:"available_room_nights"`
	OccupiedRoomNights  float64 `json:"occupied_room_nights"`
	OccupancyRate       float64 `json:"occupancy_rate"`
	RoomRevenue         float64 `json:"room_revenue"`
	AncillaryRevenue    float64 `json:"ancillary_revenue"`
	ADR                 float64 `json:"adr"`
}

// roomTypePerformance is one room type's occupancy + revenue for the requested window.
type roomTypePerformance struct {
	RoomType            string  `json:"room_type"`
	RoomCount           int     `json:"room_count"`
	AvailableRoomNights float64 `json:"available_room_nights"`
	OccupiedRoomNights  float64 `json:"occupied_room_nights"`
	OccupancyRate       float64 `json:"occupancy_rate"`
	Revenue             float64 `json:"revenue"`
}

// bookingSourceBreakdown is how many stays (and how much they billed) originated from each
// RoomGuest.source — staff walk-in/front-desk vs the online self-service widget vs API.
type bookingSourceBreakdown struct {
	Source   string  `json:"source"`
	Bookings int     `json:"bookings"`
	Revenue  float64 `json:"revenue"`
}

// hotelTrendResult is the response body for GET /reports/hotel-occupancy/trend.
type hotelTrendResult struct {
	From              string                   `json:"from"`
	To                string                   `json:"to"`
	Buckets           []hotelTrendBucket       `json:"buckets"`
	RoomTypeBreakdown []roomTypePerformance    `json:"room_type_breakdown"`
	BookingSources    []bookingSourceBreakdown `json:"booking_source_breakdown"`
}

// HotelOccupancyTrend handles GET /{tenantID}/pos/reports/hotel-occupancy/trend — the
// day-by-day occupancy/ADR/revenue series behind the Hotel Reports trend chart, plus two
// cross-sections of the same window: performance by room type (which room types actually earn
// their keep) and bookings by source (staff vs the self-service widget vs API). Shares
// roomGuestNightInterval with HotelOccupancyReport so the aggregate and the trend can never
// disagree, and reuses the same tenant/outlet/date-range resolution (parseTenantUUID,
// parseOutletFilter, parseDateRange, requestTenantLocation) as every other report handler.
func (h *ReportsHandler) HotelOccupancyTrend(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	loc := requestTenantLocation(r, h.db)
	from, to := parseDateRange(r, loc)
	if !to.After(from) {
		jsonError(w, "to must be after from", http.StatusBadRequest)
		return
	}

	roomQuery := h.db.Room.Query().Where(entroom.TenantID(tid))
	if outletFilter := parseOutletFilter(r); outletFilter != uuid.Nil {
		roomQuery = roomQuery.Where(entroom.OutletID(outletFilter))
	}
	rooms, err := roomQuery.All(r.Context())
	if err != nil {
		h.log.Error("hotel occupancy trend: room query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	totalRooms := len(rooms)
	roomIDs := make([]uuid.UUID, totalRooms)
	roomType := make(map[uuid.UUID]string, totalRooms)
	roomsByType := map[string]int{}
	for i, rm := range rooms {
		roomIDs[i] = rm.ID
		roomType[rm.ID] = string(rm.RoomType)
		roomsByType[string(rm.RoomType)]++
	}

	windowStart := startOfDayIn(from, loc)
	windowEnd := startOfDayIn(to, loc).AddDate(0, 0, 1)
	totalDays := int(windowEnd.Sub(windowStart).Hours() / 24)
	if totalDays < 1 {
		totalDays = 1
	}

	var guests []*ent.RoomGuest
	if len(roomIDs) > 0 {
		guests, err = h.db.RoomGuest.Query().
			Where(
				entroomguest.TenantID(tid),
				entroomguest.RoomIDIn(roomIDs...),
				entroomguest.CheckInDateLT(windowEnd),
			).
			All(r.Context())
		if err != nil {
			h.log.Error("hotel occupancy trend: room guest query failed", zap.Error(err))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	var folioItems []*ent.RoomFolioItem
	if len(roomIDs) > 0 {
		folioItems, err = h.db.RoomFolioItem.Query().
			Where(
				entroomfolioitem.TenantID(tid),
				entroomfolioitem.RoomIDIn(roomIDs...),
				entroomfolioitem.CreatedAtGTE(windowStart),
				entroomfolioitem.CreatedAtLT(windowEnd),
			).
			All(r.Context())
		if err != nil {
			h.log.Error("hotel occupancy trend: folio item query failed", zap.Error(err))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	// Pre-compute each guest's clipped night interval once (O(guests)), then test day
	// membership per bucket (O(days x guests)) instead of re-deriving it per day — cheap at
	// hotel scale (tens to low hundreds of stays per report window).
	type guestInterval struct {
		start, end time.Time
		roomID     uuid.UUID
	}
	intervals := make([]guestInterval, 0, len(guests))
	for _, g := range guests {
		start, end := roomGuestNightInterval(g, windowStart, windowEnd, loc)
		if end.After(start) {
			intervals = append(intervals, guestInterval{start: start, end: end, roomID: g.RoomID})
		}
	}

	buckets := make([]hotelTrendBucket, totalDays)
	roomTypeOccupied := map[string]float64{}
	roomTypeRevenue := map[string]float64{}
	for d := 0; d < totalDays; d++ {
		dayStart := windowStart.AddDate(0, 0, d)

		var occupied float64
		for _, iv := range intervals {
			if !dayStart.Before(iv.start) && dayStart.Before(iv.end) {
				occupied++
				roomTypeOccupied[roomType[iv.roomID]]++
			}
		}

		var roomRev, ancillaryRev float64
		for _, it := range folioItems {
			itDay := startOfDayIn(it.CreatedAt, loc)
			if !itDay.Equal(dayStart) {
				continue
			}
			if it.ChargeType == entroomfolioitem.ChargeTypeRoomCharge {
				roomRev += it.Amount
			} else {
				ancillaryRev += it.Amount
			}
			roomTypeRevenue[roomType[it.RoomID]] += it.Amount
		}

		bucket := hotelTrendBucket{
			Date:                dayStart.Format("2006-01-02"),
			AvailableRoomNights: float64(totalRooms),
			OccupiedRoomNights:  occupied,
			RoomRevenue:         roomRev,
			AncillaryRevenue:    ancillaryRev,
		}
		if totalRooms > 0 {
			bucket.OccupancyRate = occupied / float64(totalRooms)
		}
		if occupied > 0 {
			bucket.ADR = roomRev / occupied
		}
		buckets[d] = bucket
	}

	roomTypeBreakdown := make([]roomTypePerformance, 0, len(roomsByType))
	for rt, count := range roomsByType {
		perf := roomTypePerformance{
			RoomType:            rt,
			RoomCount:           count,
			AvailableRoomNights: float64(count) * float64(totalDays),
			OccupiedRoomNights:  roomTypeOccupied[rt],
			Revenue:             roomTypeRevenue[rt],
		}
		if perf.AvailableRoomNights > 0 {
			perf.OccupancyRate = perf.OccupiedRoomNights / perf.AvailableRoomNights
		}
		roomTypeBreakdown = append(roomTypeBreakdown, perf)
	}

	sourceCounts := map[string]int{}
	sourceRevenue := map[string]float64{}
	for _, g := range guests {
		ciDate := startOfDayIn(g.CheckInDate, loc)
		if ciDate.Before(windowStart) || !ciDate.Before(windowEnd) {
			continue // only bookings that actually checked in during this window
		}
		src := string(g.Source)
		sourceCounts[src]++
		sourceRevenue[src] += g.TotalRoomCharge
	}
	bookingSources := make([]bookingSourceBreakdown, 0, len(sourceCounts))
	for src, count := range sourceCounts {
		bookingSources = append(bookingSources, bookingSourceBreakdown{Source: src, Bookings: count, Revenue: sourceRevenue[src]})
	}

	jsonOK(w, hotelTrendResult{
		From:              from.Format(time.RFC3339),
		To:                to.Format(time.RFC3339),
		Buckets:           buckets,
		RoomTypeBreakdown: roomTypeBreakdown,
		BookingSources:    bookingSources,
	})
}
