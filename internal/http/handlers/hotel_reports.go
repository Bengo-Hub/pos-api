package handlers

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	entroom "github.com/bengobox/pos-service/internal/ent/room"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// chargeTypeRevenue is one line of the revenue-by-charge-type breakdown a hotel folio's
// RoomFolioItem.charge_type enum already carries — see internal/ent/schema/roomfolioitem.go.
type chargeTypeRevenue struct {
	ChargeType string  `json:"charge_type"`
	Amount     float64 `json:"amount"`
}

// hotelOccupancyResult is the response body for GET /reports/hotel-occupancy.
type hotelOccupancyResult struct {
	From                string              `json:"from"`
	To                  string              `json:"to"`
	TotalRooms          int                 `json:"total_rooms"`
	AvailableRoomNights float64             `json:"available_room_nights"`
	OccupiedRoomNights  float64             `json:"occupied_room_nights"`
	OccupancyRate       float64             `json:"occupancy_rate"` // 0..1
	RoomRevenue         float64             `json:"room_revenue"`
	AncillaryRevenue    float64             `json:"ancillary_revenue"`
	TotalRevenue        float64             `json:"total_revenue"`
	// ADR (Average Daily Rate) = room revenue / occupied room-nights. RevPAR (Revenue Per
	// Available Room) = room revenue / available room-nights (equivalently occupancy_rate * ADR).
	// Both standard hospitality KPIs — see https://en.wikipedia.org/wiki/RevPAR.
	ADR              float64             `json:"adr"`
	RevPAR           float64             `json:"revpar"`
	RevenueByCharge  []chargeTypeRevenue `json:"revenue_by_charge_type"`
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
// Occupied room-nights are computed by overlapping each RoomGuest stay ([check_in_date,
// checked_out_at-or-check_out_date)) with the requested window, clipped to the window's bounds —
// a stay spanning the window boundary contributes only the nights that actually fall inside it.
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

	periodDays := to.Sub(from).Hours() / 24
	availableRoomNights := float64(totalRooms) * periodDays

	var occupiedRoomNights float64
	if len(roomIDs) > 0 {
		guests, gerr := h.db.RoomGuest.Query().
			Where(
				entroomguest.TenantID(tid),
				entroomguest.RoomIDIn(roomIDs...),
				entroomguest.CheckInDateLT(to),
			).
			All(r.Context())
		if gerr != nil {
			h.log.Error("hotel occupancy: room guest query failed", zap.Error(gerr))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, g := range guests {
			stayEnd := g.CheckOutDate
			if g.Status == entroomguest.StatusCheckedOut && g.CheckedOutAt != nil {
				stayEnd = *g.CheckedOutAt
			}
			start, end := g.CheckInDate, stayEnd
			if start.Before(from) {
				start = from
			}
			if end.After(to) {
				end = to
			}
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
