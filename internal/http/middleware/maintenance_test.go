package middleware

import (
	"testing"
	"time"

	"github.com/bengobox/pos-service/internal/ent"
)

func TestUnderMaintenance(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	before := now.Add(-1 * time.Hour)
	after := now.Add(1 * time.Hour)

	cases := []struct {
		name string
		t    *ent.Tenant
		want bool
	}{
		{"nil tenant", nil, false},
		{"no window set", &ent.Tenant{}, false},
		{"only starts_at set", &ent.Tenant{MaintenanceStartsAt: &before}, false},
		{"only ends_at set", &ent.Tenant{MaintenanceEndsAt: &after}, false},
		{"now before window", &ent.Tenant{MaintenanceStartsAt: &after, MaintenanceEndsAt: &after}, false},
		{"now inside window", &ent.Tenant{MaintenanceStartsAt: &before, MaintenanceEndsAt: &after}, true},
		{"now exactly at starts_at", &ent.Tenant{MaintenanceStartsAt: &now, MaintenanceEndsAt: &after}, true},
		{"now exactly at ends_at", &ent.Tenant{MaintenanceStartsAt: &before, MaintenanceEndsAt: &now}, true},
		{"now after window", &ent.Tenant{MaintenanceStartsAt: &before, MaintenanceEndsAt: &before}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UnderMaintenance(c.t, now); got != c.want {
				t.Errorf("UnderMaintenance() = %v, want %v", got, c.want)
			}
		})
	}
}
