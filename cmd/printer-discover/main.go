// Command printer-discover finds receipt/network printers on the local network and prints their
// names. Run it ON the POS terminal (or any machine on the same LAN as the printer):
//
//	go run ./cmd/printer-discover                      # auto-detect local subnets
//	go run ./cmd/printer-discover -cidr 192.168.0.0/24 # scan a specific subnet
//	go run ./cmd/printer-discover -snmp -timeout 6s    # also SNMP-name bare 9100 printers
//	go run ./cmd/printer-discover -json                # machine-readable output
//
// It exits 0 even when nothing is found (printing a clear message), so it never hangs CI or scripts.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/pos-service/internal/modules/printing/discovery"
)

func main() {
	var (
		cidr     = flag.String("cidr", "", "subnet(s) to scan, comma-separated (default: auto-detect local interfaces)")
		portsCSV = flag.String("ports", "9100,631,515", "printer ports to probe, comma-separated")
		timeout  = flag.Duration("timeout", 4*time.Second, "overall per-source timeout")
		snmp     = flag.Bool("snmp", false, "SNMP-probe sysName for bare 9100 printers")
		comm     = flag.String("community", "public", "SNMP community string")
		asJSON   = flag.Bool("json", false, "output JSON")
		conc     = flag.Int("concurrency", 256, "max concurrent TCP dials")
	)
	flag.Parse()

	opts := discovery.Options{
		Ports:       parsePorts(*portsCSV),
		Timeout:     *timeout,
		Concurrency: *conc,
		SNMP:        *snmp,
		Community:   *comm,
	}
	if *cidr != "" {
		for _, c := range strings.Split(*cidr, ",") {
			if c = strings.TrimSpace(c); c != "" {
				opts.CIDRs = append(opts.CIDRs, c)
			}
		}
	}

	// Allow a little headroom over the per-source timeout for the whole run.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+10*time.Second)
	defer cancel()

	scanned := opts.CIDRs
	if len(scanned) == 0 {
		if local, err := discovery.LocalCIDRs(); err == nil {
			scanned = local
		}
	}
	if !*asJSON {
		fmt.Fprintf(os.Stderr, "Scanning %s (ports %s, timeout %s, snmp=%v)…\n",
			strings.Join(scanned, ", "), *portsCSV, timeout.String(), *snmp)
	}

	printers, err := discovery.Discover(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "discovery error:", err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(printers)
		return
	}

	if len(printers) == 0 {
		fmt.Printf("No printers found on %s.\n", strings.Join(scanned, ", "))
		fmt.Println("Checklist: printer powered on & on this subnet · same WiFi without AP/client isolation · " +
			"try -cidr <subnet>, a longer -timeout, or -snmp.")
		return
	}

	fmt.Printf("Found %d printer(s):\n\n", len(printers))
	fmt.Printf("%-32s %-15s %-5s %-7s %s\n", "NAME", "IP", "PORT", "SOURCE", "MODEL")
	fmt.Println(strings.Repeat("-", 80))
	for _, p := range printers {
		fmt.Printf("%-32s %-15s %-5d %-7s %s\n", truncate(p.Name, 32), p.IP, p.Port, p.Source, p.Model)
	}
}

func parsePorts(csv string) []int {
	var ports []int
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				ports = append(ports, n)
			}
		}
	}
	return ports
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
