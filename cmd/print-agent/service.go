package main

import (
	"context"
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
}

// newServer builds the loopback HTTP server exposing /health, /discover and /print.
func (p *program) newServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", withCORS(handleHealth))
	mux.HandleFunc("/discover", withCORS(handleDiscover(p.snmp, p.timeout)))
	mux.HandleFunc("/print", withCORS(handlePrint))
	return &http.Server{
		Addr:              p.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func (p *program) Start(s service.Service) error {
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
	if p.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.srv.Shutdown(ctx)
}
