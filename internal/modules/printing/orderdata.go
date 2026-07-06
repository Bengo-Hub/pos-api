package printing

import (
	"time"

	"github.com/bengobox/pos-service/internal/ent"
)

// OrderReceiptData assembles the ESC/POS ReceiptData for an order — the single builder shared by
// the /print handler and the background print queue (never duplicate this mapping).
// setting may be nil. paymentMethod/voidReason may be empty.
func OrderReceiptData(order *ent.POSOrder, lines []*ent.POSOrderLine, setting *ent.OutletSetting, typ, paymentMethod, voidReason string) ReceiptData {
	items := make([]ReceiptItem, 0, len(lines))
	for _, l := range lines {
		items = append(items, ReceiptItem{
			Name:     l.Name,
			Quantity: l.Quantity,
			Price:    l.UnitPrice,
			Total:    l.TotalPrice,
		})
	}

	var header, footer string
	if setting != nil {
		if setting.ReceiptHeader != nil {
			header = *setting.ReceiptHeader
		}
		if setting.ReceiptFooter != nil {
			footer = *setting.ReceiptFooter
		}
	}

	tableRef := ""
	if v, ok := order.Metadata["table_number"].(string); ok && v != "" {
		tableRef = v
	} else if v, ok := order.Metadata["table_name"].(string); ok && v != "" {
		tableRef = v
	}

	currency := order.Currency
	if currency == "" {
		currency = "KES"
	}

	return ReceiptData{
		Type:          typ,
		OrderNumber:   order.OrderNumber,
		TableRef:      tableRef,
		DateTime:      time.Now(),
		Header:        header,
		Footer:        footer,
		Items:         items,
		Subtotal:      order.Subtotal,
		TaxTotal:      order.TaxTotal,
		DiscountTotal: order.DiscountTotal,
		TotalAmount:   order.TotalAmount,
		PaymentMethod: paymentMethod,
		Currency:      currency,
		VoidReason:    voidReason,
	}
}

// StationTicketData assembles the kitchen/bar chit for one station's routed items
// (the map shape produced by orders.routeLinesToStations: {sku,name,quantity}).
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
		DateTime:    time.Now(),
		Header:      stationLabel,
		Items:       ri,
	}
}
