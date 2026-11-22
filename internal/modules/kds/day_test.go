package kds

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestResolveDayWindow(t *testing.T) {
	nbi, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Skip("tz data unavailable")
	}
	// 22:30 UTC on 21 Nov is 01:30 on 22 Nov in Nairobi: "today" must be the tenant's day.
	now := time.Date(2026, 11, 21, 22, 30, 0, 0, time.UTC)

	w, err := ResolveDayWindow("", "", nbi, now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Date != "2026-11-22" || w.Hour != nil {
		t.Fatalf("today = %s hour=%v, want 2026-11-22 whole day", w.Date, w.Hour)
	}
	if got := w.From.UTC(); !got.Equal(time.Date(2026, 11, 21, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("from = %s", got)
	}
	if w.To.Sub(w.From) != 24*time.Hour {
		t.Fatalf("day length = %s", w.To.Sub(w.From))
	}

	h, err := ResolveDayWindow("2026-11-20", "13", nbi, now)
	if err != nil {
		t.Fatal(err)
	}
	if h.Hour == nil || *h.Hour != 13 || h.From.In(nbi).Hour() != 13 || h.To.Sub(h.From) != time.Hour {
		t.Fatalf("hour window = %+v", h)
	}

	for _, bad := range [][2]string{{"20-11-2026", ""}, {"", "24"}, {"", "-1"}, {"", "x"}} {
		if _, err := ResolveDayWindow(bad[0], bad[1], nbi, now); err == nil {
			t.Fatalf("want error for %v", bad)
		}
	}
}

func TestTicketsCTEBindsEveryValue(t *testing.T) {
	oid, sid := uuid.New(), uuid.New()
	w, _ := ResolveDayWindow("2026-11-22", "", time.UTC, time.Now())
	sql, args := ticketsCTE(SummaryScope{TenantID: uuid.New(), OutletID: &oid, StationID: &sid}, w)
	if len(args) != 6 {
		t.Fatalf("args = %d, want 6", len(args))
	}
	if !strings.Contains(sql, "s.outlet_id = $5") || !strings.Contains(sql, "k.station_id = $6") {
		t.Fatalf("placeholders wrong:\n%s", sql)
	}
	if strings.Contains(sql, oid.String()) || strings.Contains(sql, sid.String()) {
		t.Fatal("ids must travel as parameters, never in the SQL text")
	}
}

func TestNormalizeDefaultView(t *testing.T) {
	for in, want := range map[string]string{"": ViewDay, "day": ViewDay, "LIVE": ViewLive, " live ": ViewLive, "junk": ViewDay} {
		if got := NormalizeDefaultView(in); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeRefreshSeconds(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{{nil, 60}, {"x", 60}, {float64(5), 60}, {float64(120), 120}, {900, 300}, {int64(300), 300}}
	for _, c := range cases {
		if got := NormalizeRefreshSeconds(c.in); got != c.want {
			t.Fatalf("%v -> %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSortItemTally(t *testing.T) {
	items := []ItemTally{{Name: "b", Served: 1, Total: 3}, {Name: "a", Served: 5, Total: 5}, {Name: "c", Served: 1, Total: 3}}
	SortItemTally(items)
	if items[0].Name != "a" || items[1].Name != "b" || items[2].Name != "c" {
		t.Fatalf("order = %v", items)
	}
}
