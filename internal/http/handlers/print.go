package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/modules/printing"
)

// PrintHandler handles receipt printing requests.
type PrintHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewPrintHandler(log *zap.Logger, client *ent.Client) *PrintHandler {
	return &PrintHandler{log: log, client: client}
}

type printReceiptInput struct {
	PrinterID string `json:"printer_id"` // matches PrinterProfile.ID; empty = customer default
	Type      string `json:"type"`       // "customer" | "kitchen_ticket" | "waiter_copy" | "void"
	Reason    string `json:"reason"`     // void reason (optional)
}

// PrintReceipt handles POST /{tenantID}/pos/orders/{orderID}/print
func (h *PrintHandler) PrintReceipt(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order id", http.StatusBadRequest)
		return
	}

	var input printReceiptInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Type == "" {
		input.Type = "customer"
	}

	// Load order
	order, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	// Load order lines
	lines, _ := h.client.POSOrderLine.Query().
		Where(entposorderline.OrderID(orderID)).
		All(r.Context())

	// Build receipt items
	items := make([]printing.ReceiptItem, 0, len(lines))
	for _, l := range lines {
		items = append(items, printing.ReceiptItem{
			Name:     l.Name,
			Quantity: l.Quantity,
			Price:    l.UnitPrice,
			Total:    l.TotalPrice,
		})
	}

	// Load outlet settings for printer profile and receipt config
	outletSetting, _ := h.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(order.OutletID)).
		Only(r.Context())

	var profiles []printing.PrinterProfile
	if outletSetting != nil {
		for _, raw := range outletSetting.PrinterProfiles {
			p := printing.PrinterProfile{}
			if v, ok := raw["id"].(string); ok {
				p.ID = v
			}
			if v, ok := raw["label"].(string); ok {
				p.Label = v
			}
			if v, ok := raw["printer_type"].(string); ok {
				p.PrinterType = v
			}
			if v, ok := raw["printer_ip"].(string); ok {
				p.PrinterIP = v
			}
			if v, ok := raw["paper_width"].(string); ok {
				p.PaperWidth = v
			}
			profiles = append(profiles, p)
		}
	}

	// Resolve printer profile
	printerID := input.PrinterID
	if printerID == "" {
		printerID = "customer"
	}

	var header, footer string
	if outletSetting != nil {
		if outletSetting.ReceiptHeader != nil {
			header = *outletSetting.ReceiptHeader
		}
		if outletSetting.ReceiptFooter != nil {
			footer = *outletSetting.ReceiptFooter
		}
	}

	tableRef := ""
	if v, ok := order.Metadata["table_number"].(string); ok {
		tableRef = v
	}
	if tableRef == "" {
		if v, ok := order.Metadata["table_name"].(string); ok {
			tableRef = v
		}
	}

	data := printing.ReceiptData{
		Type:          input.Type,
		OrderNumber:   order.OrderNumber,
		TableRef:      tableRef,
		Header:        header,
		Footer:        footer,
		Items:         items,
		Subtotal:      order.Subtotal,
		TaxTotal:      order.TaxTotal,
		DiscountTotal: order.DiscountTotal,
		TotalAmount:   order.TotalAmount,
		Currency:      "KES",
		VoidReason:    input.Reason,
	}

	profile := printing.FindProfileByID(profiles, printerID)

	// Browser print fallback — return HTML receipt template
	if profile == nil || profile.PrinterType == "browser" || profile.PrinterType == "" {
		w.Header().Set("Content-Type", "application/json")
		jsonOK(w, map[string]any{
			"method":       "browser",
			"receipt_data": data,
		})
		return
	}

	if profile.PrinterType == "network" && profile.PrinterIP != "" {
		rawData := printing.BuildReceipt(data)
		np := printing.NewNetworkPrinter(profile.PrinterIP)
		if err := np.Print(rawData); err != nil {
			h.log.Warn("network print failed", zap.String("printer_ip", profile.PrinterIP), zap.Error(err))
			jsonError(w, "print failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		jsonOK(w, map[string]any{"status": "printed", "method": "network", "printer_ip": profile.PrinterIP})
		return
	}

	// For unsupported printer types, return ESC/POS bytes for client-side dispatch
	rawData := printing.BuildReceipt(data)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Printer-Type", profile.PrinterType)
	_, _ = w.Write(rawData)
}
