package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	_ "github.com/bengobox/pos-service/internal/http/docs"

	"github.com/bengobox/pos-service/internal/app"
)

// setSoftMemoryLimit caps the Go heap at ~90% of the container's memory limit so transient
// allocation spikes — e.g. decoding many images concurrently while rendering a menu PDF —
// trigger garbage collection instead of a cgroup OOM-kill (exit 137). No-op when GOMEMLIMIT is
// already provided or no cgroup limit is detectable (e.g. local dev).
func setSoftMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return // operator-provided; the runtime already honours it
	}
	lim := cgroupMemLimitBytes()
	if lim <= 0 {
		return
	}
	soft := int64(float64(lim) * 0.9)
	debug.SetMemoryLimit(soft)
	log.Printf("soft memory limit set to %d MiB (cgroup limit %d MiB)", soft>>20, lim>>20)
}

// cgroupMemLimitBytes returns the container memory limit in bytes (cgroup v2 then v1), or 0.
func cgroupMemLimitBytes() int64 {
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil { // cgroup v2
		if s := strings.TrimSpace(string(b)); s != "max" {
			if v, perr := strconv.ParseInt(s, 10, 64); perr == nil && v > 0 {
				return v
			}
		}
	}
	if b, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil { // cgroup v1
		if v, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil && v > 0 && v < (1<<62) {
			return v
		}
	}
	return 0
}

// @title POS Service API
// @version 0.1.0
// @description HTTP API for the Codevertex POS service. Provides point-of-sale operations, order management, and cash drawer management.
// @BasePath /api/v1
// @schemes http https
// @securityDefinitions.apikey bearerAuth
// @in header
// @name Authorization
// @description JWT token from auth-service. Format: Bearer {token}
func main() {
	setSoftMemoryLimit()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx)
	if err != nil {
		log.Fatalf("failed to initialise app: %v", err)
	}
	defer a.Close()

	if err := a.Run(ctx); err != nil {
		log.Fatalf("runtime error: %v", err)
	}
}

