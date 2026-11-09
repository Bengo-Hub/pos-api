package orders

import (
	"testing"
	"time"
)

// DueDatePassed decides "overdue" for both the Go settlement path and the SQL summary (which
// binds the passed dates instead of casting them), so its edge cases are pinned here.
func TestDueDatePassed(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		raw  string
		want bool
	}{
		{"2026-11-10T11:59:59Z", true},
		{"2026-11-10T12:00:00Z", false}, // exactly now is not yet overdue
		{"2026-11-10T15:00:01+03:00", false},
		{"2026-11-10T14:59:59+03:00", true},
		{"2020-06-01T10:00:00.5+03:00", true},
		{"2099-01-01T00:00:00Z", false},
		{"", false},
		{"not-a-date", false},
		{"2026-02-30T00:00:00Z", false}, // impossible day: never overdue, never an error
		{"2026-11-01", false},           // date only is not RFC3339
	}
	for _, c := range cases {
		if got := DueDatePassed(c.raw, now); got != c.want {
			t.Errorf("DueDatePassed(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestIsOrderOverdueUsesDueDatePassed(t *testing.T) {
	if !IsOrderOverdue(map[string]any{"payment_due_date": "2020-01-01T00:00:00Z"}) {
		t.Fatal("a past RFC3339 date must be overdue")
	}
	for _, v := range []any{"2026-02-30T00:00:00Z", 20200101, nil, ""} {
		if IsOrderOverdue(map[string]any{"payment_due_date": v}) {
			t.Fatalf("%v must not be overdue", v)
		}
	}
}
