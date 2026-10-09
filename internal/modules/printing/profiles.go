package printing

// Fixed printer-profile ids shared with pos-ui (lib/pos/printer-stations.ts).
const (
	BillProfileID   = "customer"
	WaiterProfileID = "waiter"
)

// ProfilesFromRaw decodes the OutletSetting.printer_profiles JSON array ([]map[string]any) into
// typed profiles. It is the single decoder — handlers and the print queue must not hand-roll this.
func ProfilesFromRaw(raw []map[string]any) []PrinterProfile {
	profiles := make([]PrinterProfile, 0, len(raw))
	for _, m := range raw {
		p := PrinterProfile{}
		str := func(k string) string {
			v, _ := m[k].(string)
			return v
		}
		p.ID = str("id")
		p.Label = str("label")
		p.PrinterType = str("printer_type")
		p.PrinterIP = str("printer_ip")
		p.PrinterName = str("printer_name")
		p.PaperSize = str("paper_size")
		p.PaperWidth = str("paper_width")
		p.StationID = str("station_id")
		p.StationType = str("station_type")
		if v, ok := m["printer_port"].(float64); ok {
			p.PrinterPort = int(v)
		}
		if v, ok := m["auto_print"].(bool); ok {
			p.AutoPrint = v
			p.AutoPrintSet = true
		}
		if cats, ok := m["categories"].([]any); ok {
			for _, c := range cats {
				if s, ok := c.(string); ok {
					p.Categories = append(p.Categories, s)
				}
			}
		}
		profiles = append(profiles, p)
	}
	return profiles
}

// Paper returns the effective paper size, preferring the new paper_size field.
func (p PrinterProfile) Paper() string {
	if p.PaperSize != "" {
		return p.PaperSize
	}
	return p.PaperWidth
}

// HasRealPrinter mirrors pos-ui hasRealPrinter: the profile points at actual hardware — a named
// OS/USB/Bluetooth printer or a network printer with an IP. "browser"/"none" profiles do not count.
func (p PrinterProfile) HasRealPrinter() bool {
	switch p.PrinterType {
	case "browser", "none", "":
		return false
	case "network", "thermal":
		return p.PrinterIP != "" || (p.PrinterName != "" && p.PrinterName != "browser")
	default: // usb | os | bluetooth
		return p.PrinterName != "" && p.PrinterName != "browser"
	}
}

// ResolveBillProfile mirrors pos-ui resolveBillProfile: customer → waiter → any real printer.
// Returns nil when the outlet has no profile with real hardware.
func ResolveBillProfile(profiles []PrinterProfile) *PrinterProfile {
	if p := FindProfileByID(profiles, BillProfileID); p != nil && p.HasRealPrinter() {
		return p
	}
	if p := FindProfileByID(profiles, WaiterProfileID); p != nil && p.HasRealPrinter() {
		return p
	}
	for i := range profiles {
		if profiles[i].HasRealPrinter() {
			return &profiles[i]
		}
	}
	return nil
}

// AutoPrintEnabled reports whether this printer's own Auto-print toggle allows automatic jobs. The
// toggle is per printer card in Settings > Receipt & Printing; a profile saved without it (older
// configs) counts as on. Same rule as pos-ui printer-stations.ts autoPrintsOn.
func (p PrinterProfile) AutoPrintEnabled() bool {
	return !p.AutoPrintSet || p.AutoPrint
}

// AutoBillProfile is the single gate for every automatic customer copy (the bill printed when an
// order is posted and the receipt printed when it is paid): the outlet's auto_print_order switch
// must be on, the resolved bill printer must exist, and that printer's own Auto-print toggle must
// not be off. Returns nil when nothing should print automatically. Manual prints (Print Bill,
// Print Receipt) use ResolveBillProfile and ignore both switches.
func AutoBillProfile(autoPrintOrder bool, profiles []PrinterProfile) *PrinterProfile {
	if !autoPrintOrder {
		return nil
	}
	if p := ResolveBillProfile(profiles); p != nil && p.AutoPrintEnabled() {
		return p
	}
	return nil
}

// AutoStationProfile is ProfileForStation for automatic chits: nil when the station has no real
// printer or its card's Auto-print toggle is off. The outlet's auto_print_kitchen switch is
// checked by the caller.
func AutoStationProfile(profiles []PrinterProfile, stationID string) *PrinterProfile {
	if p := ProfileForStation(profiles, stationID); p != nil && p.AutoPrintEnabled() {
		return p
	}
	return nil
}

// ProfileForStation returns the profile assigned to a KDS station (profile id == station UUID, or
// the explicit station_id field), provided it has real hardware.
func ProfileForStation(profiles []PrinterProfile, stationID string) *PrinterProfile {
	for i := range profiles {
		p := &profiles[i]
		if (p.ID == stationID || p.StationID == stationID) && p.HasRealPrinter() {
			return p
		}
	}
	return nil
}
