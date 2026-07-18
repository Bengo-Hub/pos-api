// Command print-agent is a tiny local companion the POS operator runs ON the terminal (like QZ Tray).
//
// Because it runs on the terminal it can do what a browser and a cloud pos-api cannot:
//   - scan the terminal's own LAN for receipt/network printers (mDNS + TCP 9100/631/515 + optional SNMP), and
//   - send raw ESC/POS bytes straight to a network printer by IP:port (e.g. a cash-drawer kick).
//
// It serves a minimal HTTP API on loopback (127.0.0.1:9330 by default). Loopback is a "potentially
// trustworthy" secure context, so the HTTPS POS web app may call it (Chrome/Edge/Firefox). Every
// response is CORS-open (reflecting the request Origin) since the API is read-only discovery plus a
// raw-print relay bound to loopback.
//
//	go run ./cmd/print-agent                 # bind 127.0.0.1:9330
//	go run ./cmd/print-agent -addr :9330     # bind all interfaces (kiosk on a trusted LAN)
//	go run ./cmd/print-agent -snmp           # SNMP-name bare 9100 printers during discovery
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/bengobox/pos-service/internal/modules/printing/discovery"
)

const version = "1.3.0"

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:9330", "listen address (loopback by default)")
		defaultSNMP = flag.Bool("snmp", false, "SNMP-probe sysName for bare 9100 printers by default")
		timeout     = flag.Duration("timeout", 4*time.Second, "per-scan timeout")
	)
	flag.Parse()

	prg := &program{addr: *addr, snmp: *defaultSNMP, timeout: *timeout}

	// Run as a background service (Windows service / systemd / launchd). When Windows starts the
	// service it invokes the exe with the "run" argument (see svcConfig.Arguments).
	svcConfig := &service.Config{
		Name:        "CodevertexPrintAgent",
		DisplayName: "Codevertex POS Print Agent",
		Description: "Local bridge so the Codevertex POS can discover and print to LAN/receipt printers.",
		Arguments:   []string{"run"},
	}
	svc, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatalf("print-agent: service init: %v", err)
	}

	// Subcommand: install | uninstall | start | stop | restart | status | purge-config | run (default).
	if cmd := flag.Arg(0); cmd != "" && cmd != "run" {
		if cmd == "status" {
			st, sErr := svc.Status()
			if sErr != nil {
				log.Fatalf("print-agent: status: %v", sErr)
			}
			log.Printf("print-agent service status: %s", statusText(st))
			return
		}
		// purge-config wipes the persisted pairing from every location it may live in —
		// run by the UNINSTALLER only (never on upgrade: an upgrade must keep its pairing
		// so the reinstalled agent resumes the SAME server identity instead of forcing a
		// re-pair that would mint a fresh print_agents row).
		if cmd == "purge-config" {
			purgeConfig()
			log.Printf("print-agent: purge-config ok")
			return
		}
		if ctlErr := service.Control(svc, cmd); ctlErr != nil {
			log.Fatalf("print-agent: %s failed: %v", cmd, ctlErr)
		}
		log.Printf("print-agent: %s ok", cmd)
		return
	}

	// Default (and the service entrypoint): run under the service manager, or in the foreground when
	// launched interactively (service.Interactive()).
	if err := svc.Run(); err != nil {
		log.Fatalf("print-agent: run: %v", err)
	}
}

func statusText(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// withCORS reflects the request Origin and answers preflight, so the HTTPS POS page can call the
// loopback agent. The API exposes only LAN printer discovery and a raw-print relay — no credentials.
//
// CRITICAL: an HTTPS page (public network) calling http://127.0.0.1 (loopback = "local" network) is
// subject to Chrome's Private Network Access. The preflight carries Access-Control-Request-Private-
// Network: true and the response MUST answer Access-Control-Allow-Private-Network: true or Chrome
// blocks the request — which is exactly why a running agent reads as "not detected". We always send it.
func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		// Private Network Access: grant the loopback call from the HTTPS PWA.
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "agent": "pos-print-agent"})
}

// handleDiscover scans the terminal's LAN and returns the printers found.
// Query params: ?cidr=192.168.0.0/24[,...]  ?snmp=true
func handleDiscover(defaultSNMP bool, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snmp := defaultSNMP
		if v := strings.TrimSpace(r.URL.Query().Get("snmp")); v != "" {
			snmp = strings.EqualFold(v, "true")
		}
		var cidrs []string
		if c := strings.TrimSpace(r.URL.Query().Get("cidr")); c != "" {
			for _, p := range strings.Split(c, ",") {
				if p = strings.TrimSpace(p); p != "" {
					cidrs = append(cidrs, p)
				}
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), timeout+6*time.Second)
		defer cancel()

		printers, err := discovery.Discover(ctx, discovery.Options{CIDRs: cidrs, Timeout: timeout, SNMP: snmp})
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"printers": []any{}, "note": "discovery error: " + err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(printers))
		for _, p := range printers {
			out = append(out, map[string]any{"name": p.Name, "ip": p.IP, "port": p.Port, "source": p.Source, "model": p.Model})
		}
		writeJSON(w, http.StatusOK, map[string]any{"printers": out})
	}
}

// handlePing checks whether a raw (JetDirect/9100) network printer is reachable — a TCP connect to
// ip:port with a short timeout, then close. This backs the "Ping printer" button in printer setup so
// the operator can confirm a network printer is on the LAN before assigning/printing to it. It writes
// no data to the printer.
//
//	GET  /ping?ip=192.168.8.108&port=9100
//	POST /ping   {"ip":"192.168.8.108","port":9100}
func handlePing(w http.ResponseWriter, r *http.Request) {
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	port := 0
	if v := strings.TrimSpace(r.URL.Query().Get("port")); v != "" {
		port, _ = strconv.Atoi(v)
	}
	// Also accept a JSON body (POST) so the client can use either shape.
	if r.Method == http.MethodPost && r.Body != nil {
		var body struct {
			IP   string `json:"ip"`
			Port int    `json:"port"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body) == nil {
			if body.IP != "" {
				ip = strings.TrimSpace(body.IP)
			}
			if body.Port != 0 {
				port = body.Port
			}
		}
	}
	if net.ParseIP(ip) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid ip"})
		return
	}
	if port <= 0 || port > 65535 {
		port = 9100
	}

	start := time.Now()
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "ip": ip, "port": port, "error": err.Error()})
		return
	}
	_ = conn.Close()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ip": ip, "port": port, "ms": time.Since(start).Milliseconds()})
}

type printRequest struct {
	IP     string `json:"ip"`
	Port   int    `json:"port"`
	Name   string `json:"name"`   // locally-installed printer name (Windows spooler / CUPS) — USB printers
	Format string `json:"format"` // "rawhex" (hex string) | "raw"/"text" (utf-8)
	Data   string `json:"data"`
}

// handlePrint relays raw ESC/POS bytes to a printer: a network printer by IP:port (default 9100),
// or — when "name" is given instead — a locally-installed (USB) printer via the OS spooler. Used
// for receipts/tickets/cash-drawer kicks when there is no QZ Tray bridge on the terminal, so USB
// printing never has to fall back to the browser print dialog.
func handlePrint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST only"})
		return
	}
	var req printRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid body"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	hasIP := net.ParseIP(strings.TrimSpace(req.IP)) != nil
	if !hasIP && req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "ip or name required"})
		return
	}
	port := req.Port
	if port <= 0 || port > 65535 {
		port = 9100
	}

	var payload []byte
	switch strings.ToLower(req.Format) {
	case "rawhex", "hex":
		b, err := hex.DecodeString(strings.ReplaceAll(req.Data, " ", ""))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad hex data"})
			return
		}
		payload = b
	default: // raw / text
		payload = []byte(req.Data)
	}
	if len(payload) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "empty data"})
		return
	}

	var err error
	if hasIP {
		err = sendRaw(req.IP, port, payload)
	} else {
		err = printLocal(req.Name, payload)
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// sendRaw opens a short-lived TCP connection to a raw (JetDirect/9100) printer and writes the bytes.
func sendRaw(ip string, port int, data []byte) error {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 4*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(6 * time.Second))
	_, err = conn.Write(data)
	return err
}
