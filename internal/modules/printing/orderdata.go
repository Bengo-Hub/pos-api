package printing

import (
	"github.com/bengobox/pos-service/internal/ent"
)

// OrderReceiptData assembles the ESC/POS ReceiptData for an order — a thin adapter from the
// canonical ReceiptView (BuildReceiptView) to the thermal-byte shape, shared by the /print
// handler and the background print queue (never duplicate this mapping; never hand-populate
// escpos.ReceiptData directly for an order).
// outlet/setting may be nil. paymentMethod/servedBy/voidReason may be empty.
func OrderReceiptData(order *ent.POSOrder, lines []*ent.POSOrderLine, outlet *ent.Outlet, setting *ent.OutletSetting, typ, paymentMethod, servedBy, voidReason string) ReceiptData {
	view := BuildReceiptView(order, lines, outlet, setting, ReceiptViewOpts{
		Type:          typ,
		PaymentMethod: paymentMethod,
		ServedBy:      servedBy,
		VoidReason:    voidReason,
	})
	return receiptDataFromView(view)
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
func receiptDataFromView(v ReceiptView) ReceiptData {
	items := make([]ReceiptItem, 0, len(v.Lines))
	for _, l := range v.Lines {
		items = append(items, ReceiptItem{Name: l.Name, Quantity: l.Quantity, Price: l.UnitPrice, Total: l.TotalPrice})
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
		AmountTendered:     v.AmountTendered,
		ChangeDue:          v.ChangeDue,
		Currency:           v.Currency,
		VoidReason:         v.VoidReason,
		EtimsInvoiceNumber: v.EtimsInvoiceNumber,
		PaymentMethods:     pm,
		ProviderFooter:     v.ProviderFooter,
	}
}
