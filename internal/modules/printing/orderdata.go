package printing

import (
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/orderchannel"
)

// OutletLocation resolves the outlet's display timezone for every printed timestamp (schema
// default Africa/Nairobi), falling back to that on a missing or invalid value. The pods run on
// UTC, so a thermal ticket formatted without it reads three hours behind the wall clock.
func OutletLocation(outlet *ent.Outlet) *time.Location {
	tz := "Africa/Nairobi"
	if outlet != nil && outlet.Timezone != "" {
		tz = outlet.Timezone
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	if loc, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return loc
	}
	return time.UTC
}

// OrderReceiptData assembles the ESC/POS ReceiptData for an order — a thin adapter from the
// canonical ReceiptView (BuildReceiptView) to the thermal-byte shape, shared by the /print
// handler and the background print queue (never duplicate this mapping; never hand-populate
// escpos.ReceiptData directly for an order).
// outlet/setting may be nil. paymentMethod/servedBy/voidReason/tenantName may be empty — an
// empty tenantName falls back to the outlet's own name (see ReceiptView.DisplayName), matching
// pre-existing behaviour for any caller that hasn't resolved the tenant name yet.
func OrderReceiptData(order *ent.POSOrder, lines []*ent.POSOrderLine, outlet *ent.Outlet, setting *ent.OutletSetting, typ, paymentMethod, servedBy, voidReason, tenantName string) ReceiptData {
	return OrderReceiptDataOpts(order, lines, outlet, setting, ReceiptViewOpts{
		Type:          typ,
		PaymentMethod: paymentMethod,
		ServedBy:      servedBy,
		VoidReason:    voidReason,
		TenantName:    tenantName,
	})
}

// OrderReceiptDataOpts is OrderReceiptData with the full ReceiptViewOpts — callers that know the
// payment amounts/date (e.g. the auto-print-on-payment path) pass them so the thermal receipt can
// show Amount Paid / payment date / balance due like the browser one.
func OrderReceiptDataOpts(order *ent.POSOrder, lines []*ent.POSOrderLine, outlet *ent.Outlet, setting *ent.OutletSetting, opts ReceiptViewOpts) ReceiptData {
	view := BuildReceiptView(order, lines, outlet, setting, opts)
	d := receiptDataFromView(view, OutletLocation(outlet))
	d.QRRaster = qrRasterFromSetting(setting)
	return d
}

// qrRasterFromSetting reads the customer/bill printer profile's `qr_native` capability
// flag (printer_profiles JSON): false selects the GS v 0 raster QR for firmwares lacking
// GS ( k. Default (absent/true) keeps the crisp native QR.
func qrRasterFromSetting(setting *ent.OutletSetting) bool {
	if setting == nil {
		return false
	}
	for _, p := range setting.PrinterProfiles {
		id, _ := p["id"].(string)
		if id != "" && id != "customer" {
			continue
		}
		if native, ok := p["qr_native"].(bool); ok && !native {
			return true
		}
	}
	return false
}

// StationTicketData assembles the kitchen/bar chit for one station's routed items
// (the map shape produced by orders.routeLinesToStations: {sku,name,quantity}).
// Station tickets intentionally carry no prices/payment info — only routing + prep detail.
// loc is the outlet's timezone (OutletLocation) the order time prints in.
func StationTicketData(order *ent.POSOrder, stationLabel string, items []map[string]any, loc *time.Location) ReceiptData {
	ri := make([]ReceiptItem, 0, len(items))
	for _, it := range items {
		name, _ := it["name"].(string)
		qty, _ := it["quantity"].(float64)
		if qty == 0 {
			qty = 1
		}
		ri = append(ri, ReceiptItem{Name: name, Quantity: qty, Notes: stationItemNotes(it)})
	}

	tableRef := ""
	if v, ok := order.Metadata["table_number"].(string); ok && v != "" {
		tableRef = v
	} else if v, ok := order.Metadata["table_name"].(string); ok && v != "" {
		tableRef = v
	}

	orderType, details := StationOrderLabel(order)
	return ReceiptData{
		Type:          "kitchen_ticket",
		OutletName:    stationLabel,
		OrderNumber:   order.OrderNumber,
		TableRef:      tableRef,
		DateTime:      order.CreatedAt,
		Location:      loc,
		Header:        stationLabel,
		Items:         ri,
		OrderType:     orderType,
		TicketDetails: details,
	}
}

// StationOrderLabel is what a kitchen/bar chit says about the order beyond its items: the route in
// capitals ("DINE-IN", "TAKEAWAY", "DELIVERY", "ONLINE PICKUP", "ONLINE DELIVERY") and detail lines
// for the source (POS till or online store with its number), the customer to call for a counter
// handover, a promised time and the order note. It uses orderchannel, the same classification as the
// KDS board, so the screen and the paper always agree.
func StationOrderLabel(order *ent.POSOrder) (string, []string) {
	if order == nil {
		return "", nil
	}
	meta := order.Metadata
	channel := orderchannel.Of(string(order.OrderSubtype), meta)
	details := []string{}
	if orderchannel.IsOnline(meta) {
		src := "Source:  Online store"
		if no, _ := meta["online_order_no"].(string); strings.TrimSpace(no) != "" {
			src += " #" + strings.TrimSpace(no)
		}
		details = append(details, src)
	} else {
		details = append(details, "Source:  POS")
	}
	if channel.IsCounterHandover() && order.CustomerName != nil && strings.TrimSpace(*order.CustomerName) != "" {
		details = append(details, "For:     "+strings.TrimSpace(*order.CustomerName))
	}
	if at, _ := meta["scheduled_for_label"].(string); strings.TrimSpace(at) != "" {
		details = append(details, "Ready by: "+strings.TrimSpace(at))
	}
	if notes, _ := meta["order_notes"].(string); strings.TrimSpace(notes) != "" {
		details = append(details, "Note:    "+strings.TrimSpace(notes))
	}
	return strings.ToUpper(channel.Label()), details
}

// stationItemNotes flattens a station item's modifiers and notes into the single "*" line the
// kitchen chit prints under the item ("Oat milk, Extra shot | no sugar").
func stationItemNotes(it map[string]any) string {
	parts := []string{}
	switch mods := it["modifiers"].(type) {
	case []string:
		if len(mods) > 0 {
			parts = append(parts, strings.Join(mods, ", "))
		}
	case []any:
		labels := make([]string, 0, len(mods))
		for _, m := range mods {
			if s, ok := m.(string); ok && s != "" {
				labels = append(labels, s)
			}
		}
		if len(labels) > 0 {
			parts = append(parts, strings.Join(labels, ", "))
		}
	}
	if n, _ := it["notes"].(string); strings.TrimSpace(n) != "" {
		parts = append(parts, strings.TrimSpace(n))
	}
	return strings.Join(parts, " | ")
}

// StationTicketDataWithBanner is StationTicketData plus an attention banner (e.g.
// "*** ADDITIONAL ITEMS ***") for delta chits, items added to an already-fired order, so the
// station never re-prepares the whole bill. The order-type line still prints above it.
func StationTicketDataWithBanner(order *ent.POSOrder, stationLabel string, items []map[string]any, banner string, loc *time.Location) ReceiptData {
	d := StationTicketData(order, stationLabel, items, loc)
	if banner != "" {
		d.Banner = banner
	}
	return d
}

// receiptDataFromView maps the canonical ReceiptView onto the ESC/POS ReceiptData shape, carrying
// every field the thermal printer can render (address, served-by, VAT-rate label, charges/
// round-off, tendered/change, eTIMS, and the "HOW TO PAY" block) so an agent/background-printed
// thermal receipt is informationally identical to the browser one.
func receiptDataFromView(v ReceiptView, loc *time.Location) ReceiptData {
	if loc == nil {
		loc = time.UTC
	}
	items := make([]ReceiptItem, 0, len(v.Lines))
	for _, l := range v.Lines {
		it := ReceiptItem{Name: l.Name, Quantity: l.Quantity, Price: l.UnitPrice, Total: l.TotalPrice}
		// Show the add-time for lines rung up meaningfully after the bill was opened (add-to-bill),
		// so a happy-hour deal that depends on WHEN an item was added is auditable on the printout.
		// Same-shot lines (added at order-open) carry no note, keeping simple receipts clean.
		if l.AddedAt != nil && l.AddedAt.Sub(v.IssuedAt) >= time.Minute {
			it.Notes = fmt.Sprintf("added %s", l.AddedAt.In(loc).Format("15:04"))
		}
		items = append(items, it)
	}

	var pm *ReceiptPaymentMethods
	if v.PaymentMethods.HasAny() {
		pm = v.PaymentMethods
	}

	// The ESC/POS thermal printout uses v.DisplayName (tenant name by default, or the outlet's own
	// name when that's been turned off for this non-HQ outlet — see ReceiptView.DisplayName),
	// falling back to the true outlet name when DisplayName hasn't been resolved by the caller.
	headerName := v.DisplayName
	if headerName == "" {
		headerName = v.OutletName
	}
	return ReceiptData{
		Type:                        v.Type,
		OutletName:                  headerName,
		OutletAddress:               v.OutletAddress,
		OutletPhones:                v.OutletPhones,
		OutletEmail:                 v.OutletEmail,
		OrderNumber:                 v.OrderNumber,
		BillTo:                      v.BillTo,
		BillToLabel:                 v.BillToLabel,
		ServedBy:                    v.ServedBy,
		TableRef:                    v.TableRef,
		DateTime:                    v.DisplayDate,
		Location:                    loc,
		Header:                      v.ReceiptHeader,
		Footer:                      v.ReceiptFooter,
		Items:                       items,
		Subtotal:                    v.Subtotal,
		TaxTotal:                    v.TaxAmount,
		VatRate:                     v.VatRate,
		DiscountTotal:               v.DiscountAmount,
		ChargesTotal:                v.ChargesTotal,
		RoundOff:                    v.RoundOff,
		TotalAmount:                 v.TotalAmount,
		PaymentMethod:               v.PaymentMethod,
		PaymentDate:                 v.PaymentDate,
		AmountPaid:                  v.AmountPaid,
		BalanceDue:                  v.BalanceDue,
		CustomerAccountBalance:      v.CustomerAccountBalance,
		CustomerAccountBalanceLabel: v.CustomerAccountBalanceLabel,
		AmountTendered:              v.AmountTendered,
		ChangeDue:                   v.ChangeDue,
		Currency:                    v.Currency,
		VoidReason:                  v.VoidReason,
		EtimsInvoiceNumber:          v.EtimsInvoiceNumber,
		EtimsKraPin:                 v.EtimsKraPin,
		EtimsScuID:                  v.EtimsScuID,
		EtimsCuInvNo:                v.EtimsCuInvNo,
		EtimsRcptSign:               v.EtimsRcptSign,
		EtimsInternalData:           v.EtimsInternalData,
		EtimsQRCodeURL:              v.EtimsQRCodeURL,
		PaymentMethods:              pm,
		ProviderFooter:              v.ProviderFooter,
		ShowProviderFooter:          v.ShowProviderFooter,
		UseCase:                     v.UseCase,
		BarcodeValue:                v.FiscalBarcodeValue(),
	}
}
