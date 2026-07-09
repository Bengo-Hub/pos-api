package handlers

import (
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
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
	// BuildOnly returns the raw ESC/POS bytes (as hex) WITHOUT trying to dispatch. The cloud pos-api
	// cannot reach a LAN printer, so the browser relays these bytes to the on-terminal Local Print
	// Agent, which sends them to the network printer by IP:port.
	BuildOnly bool `json:"build_only"`
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

	// Load outlet settings for printer profile and receipt config
	outletSetting, _ := h.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(order.OutletID)).
		Only(r.Context())

	var profiles []printing.PrinterProfile
	if outletSetting != nil {
		profiles = printing.ProfilesFromRaw(outletSetting.PrinterProfiles)
	}

	// Resolve printer profile
	printerID := input.PrinterID
	if printerID == "" {
		printerID = "customer"
	}

	outlet, _ := h.client.Outlet.Query().Where(entoutlet.ID(order.OutletID)).Only(r.Context())
	servedBy := printing.ServedByFromContext(r.Context())
	data := printing.OrderReceiptData(order, lines, outlet, outletSetting, input.Type, "", servedBy, input.Reason)

	// Build-only: return the ESC/POS bytes (hex) for the browser to relay to the Local Print Agent.
	// This is how a network printer prints from a cloud deployment (the server can't reach the LAN).
	if input.BuildOnly {
		raw := printing.BuildReceipt(data)
		jsonOK(w, map[string]any{"method": "escpos", "escpos_hex": hex.EncodeToString(raw)})
		return
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
