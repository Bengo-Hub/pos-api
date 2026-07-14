package printing

import (
	"fmt"
	"time"

	"github.com/bengobox/pos-service/internal/ent"
)

// receiptLocation resolves the outlet's display timezone for receipt timestamps (schema default
// Africa/Nairobi), falling back to that on any missing/invalid value so a line's add-time is shown
// in the same wall-clock the rest of the receipt uses.
func receiptLocation(outlet *ent.Outlet) *time.Location {
	tz := "Africa/Nairobi"
	if outlet != nil && outlet.Timezone != "" {
		tz = outlet.Timezone
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}

// OrderReceiptData assembles the ESC/POS ReceiptData for an order — a thin adapter from the
// canonical ReceiptView (BuildReceiptView) to the thermal-byte shape, shared by the /print
// handler and the background print queue (never duplicate this mapping; never hand-populate
// escpos.ReceiptData directly for an order).
// outlet/setting may be nil. paymentMethod/servedBy/voidReason may be empty.
func OrderReceiptData(order *ent.POSOrder, lines []*ent.POSOrderLine, outlet *ent.Outlet, setting *ent.OutletSetting, typ, paymentMethod, servedBy, voidReason string) ReceiptData {
	return OrderReceiptDataOpts(order, lines, outlet, setting, ReceiptViewOpts{
		Type:          typ,
		PaymentMethod: paymentMethod,
		ServedBy:      servedBy,
		VoidReason:    voidReason,
	})
}

// OrderReceiptDataOpts is OrderReceiptData with the full ReceiptViewOpts — callers that know the
// payment amounts/date (e.g. the auto-print-on-payment path) pass them so the thermal receipt can
// show Amount Paid / payment date / balance due like the browser one.
func OrderReceiptDataOpts(order *ent.POSOrder, lines []*ent.POSOrderLine, outlet *ent.Outlet, setting *ent.OutletSetting, opts ReceiptViewOpts) ReceiptData {
	view := BuildReceiptView(order, lines, outlet, setting, opts)
	return receiptDataFromView(view, receiptLocation(outlet))
}

// StationTicketData assembles the kitchen/bar chit for one station's routed items
// (the map shape produced by orders.routeLinesToStations: {sku,name,quantity}).
// Station tickets intentionally carry no prices/payment info — only routing + prep detail.
func StationTicketData(order *ent.POSOrder, stationLabel string, items []map[string]any) ReceiptData {
	ri := make([]ReceiptItem, 0, len(items))
	for _, it := range items {
		name, _ := it["name"].(string)
		qty, _ := it["quantity"].(float64)
		if qty == 0 {
			qty = 1
		}
		ri = append(ri, ReceiptItem{Name: name, Quantity: qty})
	}

	tableRef := ""
	if v, ok := order.Metadata["table_number"].(string); ok && v != "" {
		tableRef = v
	} else if v, ok := order.Metadata["table_name"].(string); ok && v != "" {
		tableRef = v
	}

	return ReceiptData{
		Type:        "kitchen_ticket",
		OutletName:  stationLabel,
		OrderNumber: order.OrderNumber,
		TableRef:    tableRef,
		DateTime:    order.CreatedAt,
		Header:      stationLabel,
		Items:       ri,
	}
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

	return ReceiptData{
		Type:               v.Type,
		OutletName:         v.OutletName,
		OutletAddress:      v.OutletAddress,
		OrderNumber:        v.OrderNumber,
		BillTo:             v.BillTo,
		BillToLabel:        v.BillToLabel,
		ServedBy:           v.ServedBy,
		TableRef:           v.TableRef,
		DateTime:           v.IssuedAt,
		Header:             v.ReceiptHeader,
		Footer:             v.ReceiptFooter,
		Items:              items,
		Subtotal:           v.Subtotal,
		TaxTotal:           v.TaxAmount,
		VatRate:            v.VatRate,
		DiscountTotal:      v.DiscountAmount,
		ChargesTotal:       v.ChargesTotal,
		RoundOff:           v.RoundOff,
		TotalAmount:        v.TotalAmount,
		PaymentMethod:      v.PaymentMethod,
		PaymentDate:        v.PaymentDate,
		AmountPaid:         v.AmountPaid,
		BalanceDue:         v.BalanceDue,
		AmountTendered:     v.AmountTendered,
		ChangeDue:          v.ChangeDue,
		Currency:           v.Currency,
		VoidReason:         v.VoidReason,
		EtimsInvoiceNumber: v.EtimsInvoiceNumber,
		PaymentMethods:     pm,
		ProviderFooter:     v.ProviderFooter,
		UseCase:            v.UseCase,
	}
}
