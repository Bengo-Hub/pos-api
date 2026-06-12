package handlers

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bengobox/pos-service/internal/modules/printing/discovery"
)

// PrinterDiscover handles GET /{tenantID}/pos/printing/discover.
//
// It runs the Go LAN discovery (mDNS + TCP 9100/631/515 scan + best-effort SNMP sysName) and returns
// the printers it finds. This only makes sense for ON-PREM deployments where pos-api shares the
// network with the terminals/printers: when pos-api runs in the cloud cluster it cannot reach a
// field terminal's LAN, so the scan is disabled unless PRINTING_LAN_DISCOVERY_ENABLED=true (and even
// then returns whatever the SERVER's network can see). The pos-ui calls this FIRST and, when it is
// disabled or returns nothing, falls back to the local QZ Tray / WebUSB / Bluetooth bridges.
//
// Query params (optional): ?cidr=192.168.0.0/24[,...]  ?snmp=true
func PrinterDiscover(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("PRINTING_LAN_DISCOVERY_ENABLED")), "true") {
		jsonOK(w, map[string]any{
			"enabled":  false,
			"printers": []any{},
			"note":     "Server-side LAN discovery is off (a cloud deployment cannot reach the terminal's network). The terminal's local print bridge is used instead.",
		})
		return
	}

	snmp := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("snmp")), "true")
	var cidrs []string
	if c := strings.TrimSpace(r.URL.Query().Get("cidr")); c != "" {
		for _, p := range strings.Split(c, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cidrs = append(cidrs, p)
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 9*time.Second)
	defer cancel()

	printers, err := discovery.Discover(ctx, discovery.Options{
		CIDRs:   cidrs,
		Timeout: 4 * time.Second,
		SNMP:    snmp,
	})
	if err != nil {
		jsonOK(w, map[string]any{"enabled": true, "printers": []any{}, "note": "discovery error: " + err.Error()})
		return
	}

	out := make([]map[string]any, 0, len(printers))
	for _, p := range printers {
		out = append(out, map[string]any{
			"name":   p.Name,
			"ip":     p.IP,
			"port":   p.Port,
			"source": p.Source,
			"model":  p.Model,
		})
	}
	note := "Network scan found " + itoa(len(out)) + " printer(s) on the server network."
	if len(out) == 0 {
		note = "Network scan found no printers on the server network."
	}
	jsonOK(w, map[string]any{"enabled": true, "printers": out, "note": note})
}

// itoa is a tiny dependency-free int→string for the note (avoids importing strconv just for this).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
