package handlers

import (
	"testing"
	"time"
)

func TestScheduledForLater(t *testing.T) {
	if scheduledForLater(map[string]any{}) {
		t.Fatal("an order with no promised time goes to the kitchen when accepted")
	}
	soon := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	if scheduledForLater(map[string]any{"scheduled_for": soon}) {
		t.Fatal("an order due within the prep window is released on acceptance")
	}
	later := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	if !scheduledForLater(map[string]any{"scheduled_for": later}) {
		t.Fatal("an order for later stays off the board until its prep window")
	}
	if scheduledForLater(map[string]any{"scheduled_for": "not a time"}) {
		t.Fatal("an unreadable time must not hold the order back")
	}
}
