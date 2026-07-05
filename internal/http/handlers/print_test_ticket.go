package handlers

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/bengobox/pos-service/internal/modules/printing"
)

// testTicketInput is the optional body for the printer-setup test print.
type testTicketInput struct {
	Station string `json:"station"` // station/printer label shown on the ticket
	Paper   string `json:"paper"`   // paper size label (e.g. "80mm (72 mm print)")
}

// TestTicket handles POST /{tenantID}/pos/printing/test-ticket.
//
// It returns the ESC/POS bytes (as hex) for a short diagnostic ticket so the pos-ui can relay them to
// the on-terminal Local Print Agent (or QZ Tray) for a SILENT background test print — no order, no
// browser print dialog. This is what the "Test print" button on a Network (IP) printer card uses so
// the ticket prints straight to the printer instead of opening the web print interface.
func TestTicket(w http.ResponseWriter, r *http.Request) {
	var in testTicketInput
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&in) // body is optional; ignore decode errors
	}
	raw := printing.BuildTestTicket(in.Station, in.Paper, time.Now())
	jsonOK(w, map[string]any{"method": "escpos", "escpos_hex": hex.EncodeToString(raw)})
}
