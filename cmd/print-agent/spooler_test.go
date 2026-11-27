package main

import (
	"testing"
	"time"
)

// A rejected pairing key must slow the agent to minutes, never the 30 s cap of ordinary errors, and
// must stop growing at an hour (plus jitter).
func TestRejectedBackoffBounds(t *testing.T) {
	if got := rejectedBackoff(time.Second); got < rejectedBackoffMin || got > rejectedBackoffMin+rejectedBackoffMin/5 {
		t.Fatalf("first wait %s, want 5m to 6m", got)
	}
	if got := rejectedBackoff(10 * time.Minute); got < 20*time.Minute || got > 24*time.Minute {
		t.Fatalf("doubled wait %s, want 20m to 24m", got)
	}
	if got := rejectedBackoff(3 * time.Hour); got < rejectedBackoffMax || got > rejectedBackoffMax+rejectedBackoffMax/5 {
		t.Fatalf("capped wait %s, want 1h to 1h12m", got)
	}
}
