package printing

import "testing"

func TestAutoBillProfileHonorsOutletAndProfileSettings(t *testing.T) {
	profiles := []PrinterProfile{
		{
			ID:           BillProfileID,
			PrinterType:  "network",
			PrinterIP:    "192.168.0.50",
			AutoPrintSet: true,
		},
	}

	if got := AutoBillProfile(false, profiles); got != nil {
		t.Fatal("bill profile should be disabled when outlet auto_print_order is off")
	}
	if got := AutoBillProfile(true, profiles); got != nil {
		t.Fatal("bill profile should be disabled when its own auto_print toggle is off")
	}

	profiles[0].AutoPrint = true
	if got := AutoBillProfile(true, profiles); got == nil || got.ID != BillProfileID {
		t.Fatal("enabled outlet and bill profile should resolve to the bill printer")
	}
}

func TestAutoBillProfileKeepsLegacyProfilesEnabled(t *testing.T) {
	profiles := []PrinterProfile{{
		ID:          BillProfileID,
		PrinterType: "network",
		PrinterIP:   "192.168.0.50",
	}}

	if got := AutoBillProfile(true, profiles); got == nil || got.ID != BillProfileID {
		t.Fatal("legacy profile without auto_print should remain enabled")
	}
}

func TestAutoStationProfileHonorsProfileToggle(t *testing.T) {
	const stationID = "station-1"
	profiles := []PrinterProfile{{
		ID:           stationID,
		PrinterType:  "network",
		PrinterIP:    "192.168.0.51",
		AutoPrintSet: true,
	}}

	if got := AutoStationProfile(profiles, stationID); got != nil {
		t.Fatal("station printer should be disabled when its own auto_print toggle is off")
	}

	profiles[0].AutoPrint = true
	if got := AutoStationProfile(profiles, stationID); got == nil || got.ID != stationID {
		t.Fatal("enabled station profile should be returned")
	}
}

func TestProfilesFromRawPreservesAutoPrintPresence(t *testing.T) {
	profiles := ProfilesFromRaw([]map[string]any{
		{"id": "disabled", "auto_print": false},
		{"id": "legacy"},
	})

	if len(profiles) != 2 {
		t.Fatalf("got %d profiles, want 2", len(profiles))
	}
	if !profiles[0].AutoPrintSet || profiles[0].AutoPrint {
		t.Fatalf("explicit false was not preserved: %+v", profiles[0])
	}
	if profiles[1].AutoPrintSet {
		t.Fatalf("legacy profile unexpectedly recorded an auto_print value: %+v", profiles[1])
	}
}
