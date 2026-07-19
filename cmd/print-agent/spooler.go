package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// agentConfig is the persisted pairing: which pos-api to poll and the pairing key that
// authenticates this agent (X-Agent-Key). Written by POST /pair; loaded on service start.
type agentConfig struct {
	Server string `json:"server"` // e.g. https://posapi.codevertexitsolutions.com
	Key    string `json:"key"`    // pak_… pairing key from POS Settings → Receipt & Printing
}

// configPath stores agent.json under the OS config dir (works under the Windows service account),
// falling back to the executable's directory.
func configPath() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		p := filepath.Join(dir, "codevertex-print-agent")
		if err := os.MkdirAll(p, 0o700); err == nil {
			return filepath.Join(p, "agent.json")
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "agent.json"
	}
	return filepath.Join(filepath.Dir(exe), "agent.json")
}

func loadConfig() agentConfig {
	var cfg agentConfig
	b, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg)
	return cfg
}

func saveConfig(cfg agentConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), b, 0o600)
}

// purgeConfig removes the persisted pairing from EVERY location configPath may have used
// across run contexts — the uninstaller runs as an elevated user while the service runs as
// LocalSystem, so os.UserConfigDir resolves differently between them and deleting only the
// caller's own path would leave the service-profile copy behind (a later reinstall would then
// silently resume the OLD pairing after the operator expected a clean slate). Best-effort:
// reports what it removed, never fails the uninstall.
func purgeConfig() {
	dirs := make([]string, 0, 4)
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		dirs = append(dirs, filepath.Join(dir, "codevertex-print-agent"))
	}
	// Windows service (LocalSystem) profile config dirs — both bitness views.
	if sysRoot := os.Getenv("SystemRoot"); sysRoot != "" {
		for _, sys := range []string{"System32", "SysWOW64"} {
			dirs = append(dirs, filepath.Join(sysRoot, sys, "config", "systemprofile", "AppData", "Roaming", "codevertex-print-agent"))
		}
	}
	for _, d := range dirs {
		if err := os.RemoveAll(d); err == nil {
			log.Printf("print-agent: purged config dir %s", d)
		}
	}
	// Legacy fallback location: agent.json beside the executable.
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "agent.json")
		if os.Remove(p) == nil {
			log.Printf("print-agent: purged legacy config %s", p)
		}
	}
}

// spooler is the AccuPOS-style background print loop: long-poll pos-api for queued jobs, print
// them (network 9100 or local OS printer by name), ack the outcome. Pairing can be (re)applied at
// runtime via POST /pair without restarting the service.
type spooler struct {
	mu     sync.Mutex
	cfg    agentConfig
	cancel context.CancelFunc
	client *http.Client
	// wsUp reports whether the real-time wake-up socket is currently connected. When true the
	// claim loop waits for a push (with a slow safety-net tick) instead of continuously long-polling.
	wsUp atomic.Bool
}

func newSpooler() *spooler {
	return &spooler{
		// Read timeout must exceed the server's long-poll window (10s) with margin.
		client: &http.Client{Timeout: 40 * time.Second},
	}
}

// configure applies (and persists) a pairing and (re)starts the poll loop.
func (sp *spooler) configure(cfg agentConfig, persist bool) error {
	cfg.Server = strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
	cfg.Key = strings.TrimSpace(cfg.Key)
	if persist {
		if err := saveConfig(cfg); err != nil {
			return err
		}
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.cancel != nil {
		sp.cancel()
		sp.cancel = nil
	}
	sp.cfg = cfg
	if cfg.Server == "" || cfg.Key == "" {
		return nil // unpaired: agent keeps working as the client-relay bridge only
	}
	ctx, cancel := context.WithCancel(context.Background())
	sp.cancel = cancel
	go sp.run(ctx, cfg)
	return nil
}

func (sp *spooler) status() map[string]any {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return map[string]any{
		"paired":  sp.cfg.Server != "" && sp.cfg.Key != "",
		"server":  sp.cfg.Server,
		"polling": sp.cancel != nil,
	}
}

func (sp *spooler) stop() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.cancel != nil {
		sp.cancel()
		sp.cancel = nil
	}
}

// agentJob mirrors the pos-api agent poll response.
type agentJob struct {
	ID          string `json:"id"`
	JobType     string `json:"job_type"`
	PrinterType string `json:"printer_type"`
	PrinterIP   string `json:"printer_ip"`
	PrinterPort int    `json:"printer_port"`
	PrinterName string `json:"printer_name"`
	PayloadHex  string `json:"payload_hex"`
}

// safetyNetInterval is how often the claim loop polls anyway while the real-time socket is up — a
// belt-and-braces sweep in case a wake-up was ever missed (it shouldn't be, but printing must never
// silently stall on a dropped nudge). Short poll wait, infrequent, so it's nearly free.
const safetyNetInterval = 45 * time.Second

// run is the print loop with a push-with-poll-fallback design:
//   - A background wsLoop holds a real-time wake-up socket open. When connected (wsUp), the claim
//     loop blocks on a wake-up signal (or the slow safety-net tick) and then drains all ready jobs
//     with an immediate (wait=0) claim — jobs print within milliseconds of being enqueued.
//   - When the socket is down/unavailable, the claim loop reverts to the classic short long-poll
//     (wait=10), so an older server without the /ws route, or a flaky socket, loses nothing.
func (sp *spooler) run(ctx context.Context, cfg agentConfig) {
	log.Printf("print-agent: spooler started for %s (real-time + poll fallback)", cfg.Server)

	// Wake-up signal from the WS loop (buffered depth 1 — wake-ups coalesce into "drain now").
	wake := make(chan struct{}, 1)
	go sp.wsLoop(ctx, cfg, wake)

	backoff := time.Second
	for ctx.Err() == nil {
		if sp.wsUp.Load() {
			// Real-time mode: wait for a push or the safety-net tick, then drain everything ready.
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-time.After(safetyNetInterval):
			}
			sp.drainJobs(ctx, cfg)
			continue
		}

		// Fallback mode: classic short long-poll. One job per pass, immediate re-poll on empty.
		job, err := sp.nextJob(ctx, cfg, 10)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("print-agent: poll failed: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if job == nil {
			continue // long-poll returned empty — poll again immediately
		}
		sp.printAndAck(ctx, cfg, job)
	}
}

// drainJobs claims and prints every job currently ready for this agent's outlet, using an immediate
// (wait=0) claim so a real-time wake-up empties the queue in one pass. Stops on the first empty
// response (204) or an error (the loop reverts to polling / retries on the next signal).
func (sp *spooler) drainJobs(ctx context.Context, cfg agentConfig) {
	for ctx.Err() == nil {
		job, err := sp.nextJob(ctx, cfg, 0)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("print-agent: drain claim failed: %v", err)
			}
			return
		}
		if job == nil {
			return // nothing left ready
		}
		sp.printAndAck(ctx, cfg, job)
	}
}

// printAndAck dispatches one claimed job to its printer and reports the outcome back to the server.
func (sp *spooler) printAndAck(ctx context.Context, cfg agentConfig, job *agentJob) {
	printErr := dispatchJob(job)
	if printErr != nil {
		log.Printf("print-agent: job %s (%s) failed: %v", job.ID, job.JobType, printErr)
	} else {
		log.Printf("print-agent: printed job %s (%s)", job.ID, job.JobType)
	}
	sp.ackJob(ctx, cfg, job.ID, printErr)
}

// wsLoop maintains the real-time wake-up socket, reconnecting with backoff. While connected it sets
// wsUp=true and signals `wake` on every server push (and once on connect, to drain anything enqueued
// while it was reconnecting). A server without the /ws route (older pos-api) fails the dial and the
// loop keeps retrying slowly while the poll fallback carries printing — nothing is lost.
func (sp *spooler) wsLoop(ctx context.Context, cfg agentConfig, wake chan<- struct{}) {
	wsURL := toWebSocketURL(cfg.Server) + "/api/v1/pos/printing/agent/ws?version=" + version
	backoff := time.Second
	for ctx.Err() == nil {
		conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"X-Agent-Key": {cfg.Key}},
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Quietly retry — an old server (no /ws) or a transient network blip both land here, and
			// the poll fallback is already covering printing. Cap the backoff so reconnection stays
			// responsive once the endpoint becomes available.
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}

		backoff = time.Second
		sp.wsUp.Store(true)
		log.Printf("print-agent: real-time wake-up socket connected")
		signalWake(wake) // drain anything queued while we were (re)connecting

		// Read loop — any inbound frame (job_available push or the connect ping) means "claim now".
		for ctx.Err() == nil {
			_, _, rerr := conn.Read(ctx)
			if rerr != nil {
				break
			}
			signalWake(wake)
		}

		sp.wsUp.Store(false)
		_ = conn.Close(websocket.StatusNormalClosure, "")
		if ctx.Err() != nil {
			return
		}
		log.Printf("print-agent: real-time socket dropped — reverting to polling, will reconnect")
	}
}

// signalWake does a non-blocking send so overlapping wake-ups coalesce into a single "drain" pass.
func signalWake(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// toWebSocketURL rewrites an http(s) base URL to its ws(s) equivalent for the wake-up socket.
func toWebSocketURL(server string) string {
	s := strings.TrimRight(strings.TrimSpace(server), "/")
	switch {
	case strings.HasPrefix(s, "https://"):
		return "wss://" + strings.TrimPrefix(s, "https://")
	case strings.HasPrefix(s, "http://"):
		return "ws://" + strings.TrimPrefix(s, "http://")
	default:
		return s // already ws(s):// or bare host — pass through
	}
}

// nextJob long-polls (wait seconds; 0 = return immediately) for the next claimable job. version is
// reported so the server can track agent versions on the liveness record.
func (sp *spooler) nextJob(ctx context.Context, cfg agentConfig, wait int) (*agentJob, error) {
	url := fmt.Sprintf("%s/api/v1/pos/printing/agent/jobs?wait=%d&version=%s", cfg.Server, wait, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Agent-Key", cfg.Key)
	resp, err := sp.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var job agentJob
		if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
			return nil, err
		}
		return &job, nil
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("pairing key rejected (re-pair from POS Settings)")
	default:
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
}

func (sp *spooler) ackJob(ctx context.Context, cfg agentConfig, jobID string, printErr error) {
	body := map[string]any{"printed": printErr == nil}
	if printErr != nil {
		body["error"] = printErr.Error()
	}
	b, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/api/v1/pos/printing/agent/jobs/%s/ack", cfg.Server, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("X-Agent-Key", cfg.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := sp.client.Do(req)
	if err != nil {
		log.Printf("print-agent: ack %s failed: %v", jobID, err)
		return
	}
	_ = resp.Body.Close()
}

// dispatchJob prints one claimed job: network target by IP:port, else local OS printer by name.
func dispatchJob(job *agentJob) error {
	payload, err := hex.DecodeString(strings.ReplaceAll(job.PayloadHex, " ", ""))
	if err != nil {
		return fmt.Errorf("bad payload hex: %w", err)
	}
	if len(payload) == 0 {
		return fmt.Errorf("empty payload")
	}
	if job.PrinterIP != "" {
		port := job.PrinterPort
		if port <= 0 || port > 65535 {
			port = 9100
		}
		return sendRaw(job.PrinterIP, port, payload)
	}
	if job.PrinterName != "" {
		return printLocal(job.PrinterName, payload)
	}
	return fmt.Errorf("job has no printer target")
}
