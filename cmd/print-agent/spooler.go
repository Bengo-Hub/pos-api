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
	"time"
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
}

func newSpooler() *spooler {
	return &spooler{
		// Read timeout must exceed the server's 25s long-poll window.
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

func (sp *spooler) run(ctx context.Context, cfg agentConfig) {
	log.Printf("print-agent: spooler polling %s", cfg.Server)
	backoff := time.Second
	for ctx.Err() == nil {
		job, err := sp.nextJob(ctx, cfg)
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

		printErr := dispatchJob(job)
		if printErr != nil {
			log.Printf("print-agent: job %s (%s) failed: %v", job.ID, job.JobType, printErr)
		} else {
			log.Printf("print-agent: printed job %s (%s)", job.ID, job.JobType)
		}
		sp.ackJob(ctx, cfg, job.ID, printErr)
	}
}

func (sp *spooler) nextJob(ctx context.Context, cfg agentConfig) (*agentJob, error) {
	url := fmt.Sprintf("%s/api/v1/pos/printing/agent/jobs?wait=25&version=%s", cfg.Server, version)
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
