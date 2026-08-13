package handlers

import (
	"net/http"
	"time"
)

// parseCreatedAtRange parses optional ?from=&to= (YYYY-MM-DD) query params for plain operational
// list endpoints (Layaway, Staff Credit, Bills, Commissions, Packages, Loyalty, Shipments,
// Discounts, …) that filter on CreatedAt — unlike the reports package's effective-date/business-
// date machinery (order_date_range.go), these are simple CRUD lists with no backdating concept, so
// a plain UTC calendar-day range is the right granularity. `to` is end-of-day inclusive. ok is
// false when neither param is present (caller applies no date filter at all).
func parseCreatedAtRange(r *http.Request) (from, to time.Time, ok bool) {
	fromStr, toStr := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if fromStr == "" && toStr == "" {
		return time.Time{}, time.Time{}, false
	}
	if fromStr != "" {
		if t, err := time.Parse("2006-01-02", fromStr); err == nil {
			from = t
		}
	}
	if toStr != "" {
		if t, err := time.Parse("2006-01-02", toStr); err == nil {
			to = t.Add(24*time.Hour - time.Second)
		}
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from, to, true
}
