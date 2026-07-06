package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/kardianos/service"
)

// program implements service.Interface so the print-agent runs as a Windows service (or systemd/
// launchd). Start must be non-blocking (kick off the HTTP server in a goroutine); Stop shuts it down.
type program struct {
	addr    string
	snmp    bool
	timeout time.Duration
	srv     *http.Server
	spool   *spooler
}

// newServer builds the loopback HTTP server exposing /health, /discover, /ping, /print, plus the
// spooler endpoints: /printers (local OS printers), /pair (apply pairing), /status.
func (p *program) newServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", withCORS(handleHealth))
	mux.HandleFunc("/discover", withCORS(handleDiscover(p.snmp, p.timeout)))
	mux.HandleFunc("/ping", withCORS(handlePing))
	mux.HandleFunc("/print", withCORS(handlePrint))
	mux.HandleFunc("/printers", withCORS(handlePrinters))
	mux.HandleFunc("/pair", withCORS(p.handlePair))
	mux.HandleFunc("/status", withCORS(p.handleStatus))
	return &http.Server{
		Addr:              p.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func (p *program) Start(s service.Service) error {
	p.spool = newSpooler()
	// Resume a saved pairing so the spooler starts printing queued jobs immediately on boot.
	if cfg := loadConfig(); cfg.Server != "" && cfg.Key != "" {
		if err := p.spool.configure(cfg, false); err != nil {
			log.Printf("print-agent: resume pairing failed: %v", err)
		}
	}
	p.srv = p.newServer()
	go func() {
		log.Printf("pos print-agent %s listening on http://%s", version, p.addr)
		if err := p.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("print-agent: server error: %v", err)
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.spool != nil {
		p.spool.stop()
	}
	if p.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.srv.Shutdown(ctx)
}

// handlePair applies a pairing issued by POS Settings → Receipt & Printing ("Pair print agent"):
// {"server":"https://posapi…","key":"pak_…"}. Persists it and (re)starts the job spooler, so the
// browser can hand the one-time key straight to the loopback agent — no manual config editing.
func (p *program) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST only"})
		return
	}
	var cfg agentConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid body"})
		return
	}
	if cfg.Server == "" || cfg.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "server and key required"})
		return
	}
	if err := p.spool.configure(cfg, true); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleStatus reports pairing/polling state for the settings UI badge.
func (p *program) handleStatus(w http.ResponseWriter, _ *http.Request) {
	st := p.spool.status()
	st["ok"] = true
	st["version"] = version
	writeJSON(w, http.StatusOK, st)
}

// handlePrinters lists the printers installed on THIS machine (Windows spooler / CUPS) so the
// settings UI can bind a USB profile to an exact OS printer name.
func handlePrinters(w http.ResponseWriter, _ *http.Request) {
	names, err := localPrinters()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"printers": []string{}, "note": err.Error()})
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"printers": names})
}
