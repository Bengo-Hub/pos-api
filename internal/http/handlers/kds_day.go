package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	kdsmod "github.com/bengobox/pos-service/internal/modules/kds"
)

// Day view of the KDS board: served and voided tickets stay listed for the tenant's whole day,
// grouped by hour, so baristas and chefs can tally what left the pass at shift close. The outlet
// decides whether the board opens on this view (service settings kds_default_view, kds_keep_served).

const (
	kdsDayTicketDefaultLimit = 500
	kdsDayTicketMaxLimit     = 2000
)

// SetSummaryDB wires the database the day summary aggregates on (the read replica in production,
// the primary when none is configured).
func (h *KDSHandler) SetSummaryDB(db kdsmod.Querier) { h.summaryDB = db }

// dayView reports whether the ticket list asks for a day or hour window instead of the live board.
func dayView(r *http.Request) bool {
	q := r.URL.Query()
	return q.Get("view") == "day" || q.Get("date") != "" || q.Get("hour") != ""
}

// kdsDayTicketLimit is the page size of a day listing (?limit=, capped). The hourly summary is
// computed in SQL over the whole window, so a capped card list never skews the tally.
func kdsDayTicketLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return kdsDayTicketDefaultLimit
	}
	if n > kdsDayTicketMaxLimit {
		return kdsDayTicketMaxLimit
	}
	return n
}

// DaySummary handles GET /{tenantID}/pos/kds/tickets/day-summary
// ?date=YYYY-MM-DD (tenant zone, default today), ?hour=0..23 (narrows the item tally), ?station_id=.
// Returns all 24 hourly buckets of the day and the item tally of the selected window.
func (h *KDSHandler) DaySummary(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	if h.summaryDB == nil {
		h.log.Error("kds day summary: no summary database wired (SetSummaryDB)")
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	win, err := kdsmod.ResolveDayWindow(r.URL.Query().Get("date"), r.URL.Query().Get("hour"),
		tenantLocation(r.Context(), h.client, tid), time.Now())
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	scope := kdsmod.SummaryScope{TenantID: tid}
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			scope.OutletID = &oid
		}
	}
	if s := r.URL.Query().Get("station_id"); s != "" {
		sid, perr := uuid.Parse(s)
		if perr != nil {
			jsonError(w, "invalid station_id", http.StatusBadRequest)
			return
		}
		scope.StationID = &sid
	}
	summary, err := kdsmod.BuildDaySummary(r.Context(), h.summaryDB, scope, win)
	if err != nil {
		h.log.Error("kds day summary failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, summary)
}
