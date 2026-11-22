package kds

import (
	"context"
	stdsql "database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Day view of the KDS board. Served and voided tickets stay on the board for the whole business
// day, so baristas and chefs can tally what left the pass against the system when they close a
// shift. The day is the tenant's calendar day (pods run in UTC), split into 24 hourly buckets.

// Outlet settings for the board, kept in OutletSetting.metadata (no schema column).
const (
	// MetaKeyDefaultView is the view the board opens on: ViewDay (default) or ViewLive.
	MetaKeyDefaultView = "kds_default_view"
	// MetaKeyKeepServed keeps served and voided tickets on the day view, crossed out (default on).
	MetaKeyKeepServed = "kds_keep_served"

	ViewDay  = "day"
	ViewLive = "live"

	// MetaKeyRefreshSeconds is how often a board screen reloads its tickets, in seconds.
	MetaKeyRefreshSeconds = "kds_refresh_seconds"
	RefreshSecondsMin     = 60
	RefreshSecondsMax     = 300
	RefreshSecondsDefault = 60
)

// NormalizeRefreshSeconds clamps a stored or submitted reload interval to 60..300 seconds
// (60 when unset or not a number).
func NormalizeRefreshSeconds(v any) int {
	n := RefreshSecondsDefault
	switch x := v.(type) {
	case float64:
		n = int(x)
	case int:
		n = x
	case int64:
		n = int(x)
	}
	if n < RefreshSecondsMin {
		return RefreshSecondsMin
	}
	if n > RefreshSecondsMax {
		return RefreshSecondsMax
	}
	return n
}

// NormalizeDefaultView maps a stored or submitted view to ViewDay or ViewLive (day when unknown).
func NormalizeDefaultView(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), ViewLive) {
		return ViewLive
	}
	return ViewDay
}

// DayWindow is one tenant-local calendar day, or one hour of it when Hour is set.
type DayWindow struct {
	Date     string // YYYY-MM-DD in the tenant zone
	Location *time.Location
	From     time.Time // inclusive
	To       time.Time // exclusive
	Hour     *int
}

// ResolveDayWindow parses ?date= (YYYY-MM-DD, empty means today in loc) and ?hour= (0..23, empty
// means the whole day). An hour on a day that skips or repeats an hour (DST) still maps to the
// wall-clock hour, because the bounds are built with time.Date in loc.
func ResolveDayWindow(date, hour string, loc *time.Location, now time.Time) (DayWindow, error) {
	if loc == nil {
		loc = time.UTC
	}
	var day time.Time
	if d := strings.TrimSpace(date); d != "" {
		parsed, err := time.ParseInLocation("2006-01-02", d, loc)
		if err != nil {
			return DayWindow{}, fmt.Errorf("date must be YYYY-MM-DD")
		}
		day = parsed
	} else {
		n := now.In(loc)
		day = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	}
	w := DayWindow{Date: day.Format("2006-01-02"), Location: loc, From: day, To: day.AddDate(0, 0, 1)}
	if h := strings.TrimSpace(hour); h != "" {
		n, err := strconv.Atoi(h)
		if err != nil || n < 0 || n > 23 {
			return DayWindow{}, fmt.Errorf("hour must be 0-23")
		}
		w.Hour = &n
		w.From = time.Date(day.Year(), day.Month(), day.Day(), n, 0, 0, 0, loc)
		w.To = w.From.Add(time.Hour)
	}
	return w, nil
}

// HourBucket is one hour of the day with its tickets and item quantities by status.
type HourBucket struct {
	Hour        int     `json:"hour"`
	Start       string  `json:"start"` // RFC3339 in the tenant zone
	Tickets     int     `json:"tickets"`
	Active      int     `json:"active"`
	Served      int     `json:"served"`
	Voided      int     `json:"voided"`
	Items       float64 `json:"items"`
	ServedItems float64 `json:"served_items"`
}

// ItemTally is one item's quantity in the window, split by ticket status, for the shift tally.
type ItemTally struct {
	Name   string  `json:"name"`
	Total  float64 `json:"total"`
	Active float64 `json:"active"`
	Served float64 `json:"served"`
	Voided float64 `json:"voided"`
}

// DaySummary is the hourly aggregation of a day plus the item tally of the selected window
// (the whole day, or the drilled-down hour).
type DaySummary struct {
	Date     string       `json:"date"`
	Timezone string       `json:"timezone"`
	Hour     *int         `json:"hour,omitempty"`
	Hours    []HourBucket `json:"hours"`
	Items    []ItemTally  `json:"items"`
	Totals   HourBucket   `json:"totals"`
}

// Querier is the part of *sql.DB the summary needs (the read replica in production).
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*stdsql.Rows, error)
}

// SummaryScope narrows the summary to an outlet (through the ticket's station, tickets carry no
// outlet of their own) and optionally one station.
type SummaryScope struct {
	TenantID  uuid.UUID
	OutletID  *uuid.UUID
	StationID *uuid.UUID
}

// ticketsCTE selects the window's tickets with their tenant-local hour. Every value is a bound
// parameter: $1 tenant, $2 from, $3 to, $4 zone name, then optional outlet and station.
func ticketsCTE(scope SummaryScope, w DayWindow) (string, []any) {
	args := []any{scope.TenantID, w.From, w.To, w.Location.String()}
	var b strings.Builder
	b.WriteString(`WITH t AS (
  SELECT k.id, k.status, k.items,
         EXTRACT(HOUR FROM k.received_at AT TIME ZONE $4)::int AS hr
  FROM kds_tickets k`)
	if scope.OutletID != nil {
		args = append(args, *scope.OutletID)
		b.WriteString(fmt.Sprintf("\n  JOIN kds_stations s ON s.id = k.station_id AND s.outlet_id = $%d", len(args)))
	}
	b.WriteString("\n  WHERE k.tenant_id = $1 AND k.received_at >= $2 AND k.received_at < $3")
	if scope.StationID != nil {
		args = append(args, *scope.StationID)
		b.WriteString(fmt.Sprintf(" AND k.station_id = $%d", len(args)))
	}
	b.WriteString("\n)\n")
	return b.String(), args
}

// itemQtySQL reads one ticket item's quantity. pos-api writes `quantity`; older tickets carry
// `qty`; a missing or non-numeric value counts as one plate.
const itemQtySQL = `CASE
    WHEN jsonb_typeof(e->'quantity') = 'number' THEN (e->>'quantity')::numeric
    WHEN jsonb_typeof(e->'qty') = 'number' THEN (e->>'qty')::numeric
    ELSE 1 END`

// BuildDaySummary aggregates the day in SQL (two grouped reads), so its cost tracks the number of
// hours and distinct items, not how many tickets the day held.
func BuildDaySummary(ctx context.Context, db Querier, scope SummaryScope, day DayWindow) (*DaySummary, error) {
	whole := day
	whole.Hour = nil
	whole.From = time.Date(day.From.Year(), day.From.Month(), day.From.Day(), 0, 0, 0, 0, day.Location)
	whole.To = whole.From.AddDate(0, 0, 1)

	out := &DaySummary{Date: day.Date, Timezone: day.Location.String(), Hour: day.Hour}
	out.Hours = make([]HourBucket, 24)
	for h := 0; h < 24; h++ {
		start := time.Date(whole.From.Year(), whole.From.Month(), whole.From.Day(), h, 0, 0, 0, day.Location)
		out.Hours[h] = HourBucket{Hour: h, Start: start.Format(time.RFC3339)}
	}

	cte, args := ticketsCTE(scope, whole)
	hourSQL := cte + `SELECT t.hr, t.status, COUNT(DISTINCT t.id), COALESCE(SUM(q.qty), 0)
FROM t
LEFT JOIN LATERAL (
  SELECT ` + itemQtySQL + ` AS qty FROM jsonb_array_elements(COALESCE(t.items, '[]'::jsonb)) e
) q ON true
GROUP BY t.hr, t.status`
	rows, err := db.QueryContext(ctx, hourSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("kds day hours: %w", err)
	}
	for rows.Next() {
		var hr, n int
		var status string
		var qty float64
		if err := rows.Scan(&hr, &status, &n, &qty); err != nil {
			rows.Close()
			return nil, fmt.Errorf("kds day hours scan: %w", err)
		}
		if hr < 0 || hr > 23 {
			continue
		}
		addStatus(&out.Hours[hr], status, n, qty)
		addStatus(&out.Totals, status, n, qty)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kds day hours rows: %w", err)
	}

	cte, args = ticketsCTE(scope, day)
	itemSQL := cte + `SELECT COALESCE(NULLIF(TRIM(e->>'name'), ''), 'Unnamed item') AS name, t.status, SUM(` + itemQtySQL + `)
FROM t CROSS JOIN LATERAL jsonb_array_elements(COALESCE(t.items, '[]'::jsonb)) e
GROUP BY 1, t.status`
	rows, err = db.QueryContext(ctx, itemSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("kds day items: %w", err)
	}
	defer rows.Close()
	byName := map[string]*ItemTally{}
	for rows.Next() {
		var name, status string
		var qty float64
		if err := rows.Scan(&name, &status, &qty); err != nil {
			return nil, fmt.Errorf("kds day items scan: %w", err)
		}
		it := byName[name]
		if it == nil {
			it = &ItemTally{Name: name}
			byName[name] = it
		}
		it.Total += qty
		switch status {
		case "served":
			it.Served += qty
		case "voided":
			it.Voided += qty
		default:
			it.Active += qty
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kds day items rows: %w", err)
	}
	out.Items = make([]ItemTally, 0, len(byName))
	for _, it := range byName {
		out.Items = append(out.Items, *it)
	}
	SortItemTally(out.Items)
	return out, nil
}

// SortItemTally orders the tally by quantity sold (served first), then name, so the top sellers
// lead the shift count.
func SortItemTally(items []ItemTally) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Served != items[j].Served {
			return items[i].Served > items[j].Served
		}
		if items[i].Total != items[j].Total {
			return items[i].Total > items[j].Total
		}
		return items[i].Name < items[j].Name
	})
}

func addStatus(b *HourBucket, status string, n int, qty float64) {
	b.Tickets += n
	b.Items += qty
	switch status {
	case "served":
		b.Served += n
		b.ServedItems += qty
	case "voided":
		b.Voided += n
	default:
		b.Active += n
	}
}
