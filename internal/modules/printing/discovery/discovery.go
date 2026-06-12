// Package discovery finds receipt/label printers on the local network so an operator can pick one
// without typing an IP. It probes three independent ways and merges the results:
//
//   - mDNS / Bonjour (_ipp._tcp, _ipps._tcp, _printer._tcp, _pdl-datastream._tcp) — yields the
//     friendly printer NAME from the service instance + TXT records (modern network printers & AirPrint).
//   - TCP port scan of the local subnet on 9100 (HP JetDirect / raw ESC-POS), 631 (IPP), 515 (LPD).
//   - SNMP sysName (best-effort) to name a bare 9100 printer that does not advertise mDNS.
//
// IMPORTANT (deployment): this scans the CALLER's local network. It is meant to run on the POS
// terminal (via the bundled `cmd/printer-discover` CLI, or an on-prem pos-api). When pos-api runs in
// the cloud cluster it shares no L2 network with field terminals, so an HTTP endpoint built on this
// will legitimately find nothing there — that is expected, not a bug.
package discovery

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// DefaultPorts are the printer ports probed by a subnet scan.
var DefaultPorts = []int{9100, 631, 515}

// Printer is a discovered printer (or an open printer port we could not fully name).
type Printer struct {
	Name     string   `json:"name"`
	IP       string   `json:"ip"`
	Port     int      `json:"port"`
	Source   string   `json:"source"` // "mdns" | "scan" | "snmp"
	Model    string   `json:"model,omitempty"`
	Services []string `json:"services,omitempty"`
}

// Options controls a Discover run.
type Options struct {
	CIDRs       []string      // subnets to scan; empty → LocalCIDRs()
	Ports       []int         // ports to probe; empty → DefaultPorts
	Timeout     time.Duration // per-dial / per-query timeout
	Concurrency int           // max concurrent dials; <=0 → 256
	SNMP        bool          // enrich bare-9100 hits with an SNMP sysName probe
	Community   string        // SNMP community; empty → "public"
	MaxHosts    int           // safety cap on hosts per CIDR; <=0 → 4096
}

func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 4 * time.Second
	}
	return o.Timeout
}

func (o Options) concurrency() int {
	if o.Concurrency <= 0 {
		return 256
	}
	return o.Concurrency
}

func (o Options) ports() []int {
	if len(o.Ports) == 0 {
		return DefaultPorts
	}
	return o.Ports
}

func (o Options) maxHosts() int {
	if o.MaxHosts <= 0 {
		return 4096
	}
	return o.MaxHosts
}

// LocalCIDRs returns the IPv4 networks of the host's active, non-loopback interfaces (e.g.
// "192.168.0.0/24"), suitable to hand to ScanSubnet.
func LocalCIDRs() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var cidrs []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			network := ipnet.IP.Mask(ipnet.Mask)
			cidr := (&net.IPNet{IP: network, Mask: ipnet.Mask}).String()
			if _, dup := seen[cidr]; dup {
				continue
			}
			seen[cidr] = struct{}{}
			cidrs = append(cidrs, cidr)
		}
	}
	return cidrs, nil
}

// hostsInCIDR enumerates the usable host IPs in a CIDR (skips network & broadcast for IPv4 subnets
// smaller than /31). Returns an error if the host count exceeds max.
func hostsInCIDR(cidr string, max int) ([]net.IP, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	if ip.To4() == nil {
		return nil, fmt.Errorf("discovery: only IPv4 CIDRs supported, got %s", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	count := 1 << uint(bits-ones)
	if count > max {
		return nil, fmt.Errorf("discovery: %s has %d hosts (> max %d); pass a smaller CIDR", cidr, count, max)
	}
	var ips []net.IP
	for cur := ipnet.IP.Mask(ipnet.Mask); ipnet.Contains(cur); cur = nextIP(cur) {
		ips = append(ips, append(net.IP(nil), cur...))
	}
	// Drop network & broadcast for ordinary subnets.
	if len(ips) >= 2 && bits-ones >= 2 {
		ips = ips[1 : len(ips)-1]
	}
	return ips, nil
}

func nextIP(ip net.IP) net.IP {
	out := append(net.IP(nil), ip.To4()...)
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out
}

// ScanSubnet TCP-probes every host in the CIDR on the given ports and returns one Printer per host
// that accepted a connection (named only by IP/port here — Discover enriches with mDNS/SNMP names).
func ScanSubnet(ctx context.Context, cidr string, opts Options) ([]Printer, error) {
	hosts, err := hostsInCIDR(cidr, opts.maxHosts())
	if err != nil {
		return nil, err
	}
	ports := opts.ports()
	to := opts.timeout()

	type hit struct {
		ip   string
		port int
	}
	results := make(chan hit, 64)
	sem := make(chan struct{}, opts.concurrency())
	var wg sync.WaitGroup

	for _, ip := range hosts {
		ipStr := ip.String()
		for _, p := range ports {
			wg.Add(1)
			sem <- struct{}{}
			go func(ipStr string, port int) {
				defer wg.Done()
				defer func() { <-sem }()
				if ctx.Err() != nil {
					return
				}
				d := net.Dialer{Timeout: to}
				conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ipStr, strconv.Itoa(port)))
				if err != nil {
					return
				}
				_ = conn.Close()
				results <- hit{ip: ipStr, port: port}
			}(ipStr, p)
		}
	}
	go func() { wg.Wait(); close(results) }()

	// First open port per host wins (ports are probed in DefaultPorts order of preference).
	byIP := map[string]int{}
	for h := range results {
		if cur, ok := byIP[h.ip]; !ok || portPriority(h.port) < portPriority(cur) {
			byIP[h.ip] = h.port
		}
	}

	out := make([]Printer, 0, len(byIP))
	for ip, port := range byIP {
		out = append(out, Printer{IP: ip, Port: port, Source: "scan"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out, nil
}

// portPriority orders ports so a host's "best" port is chosen as its representative.
func portPriority(port int) int {
	switch port {
	case 9100:
		return 0
	case 631:
		return 1
	case 515:
		return 2
	default:
		return 9
	}
}

// Discover runs mDNS + subnet scan (+ optional SNMP naming) across the given (or local) CIDRs and
// returns a de-duplicated, name-enriched printer list. It never returns a hard error for a single
// failing source — partial results are still useful.
func Discover(ctx context.Context, opts Options) ([]Printer, error) {
	cidrs := opts.CIDRs
	if len(cidrs) == 0 {
		local, err := LocalCIDRs()
		if err != nil {
			return nil, fmt.Errorf("discovery: detect local subnets: %w", err)
		}
		cidrs = local
	}

	byIP := map[string]*Printer{}

	// 1) mDNS — best for names. Failures are non-fatal.
	if mdns, err := DiscoverMDNS(ctx, opts.timeout()); err == nil {
		for i := range mdns {
			m := mdns[i]
			if m.IP == "" {
				continue
			}
			cp := m
			byIP[m.IP] = &cp
		}
	}

	// 2) Subnet scan — finds printers that don't advertise mDNS.
	for _, cidr := range cidrs {
		scanned, err := ScanSubnet(ctx, cidr, opts)
		if err != nil {
			continue
		}
		for i := range scanned {
			s := scanned[i]
			if existing, ok := byIP[s.IP]; ok {
				if existing.Port == 0 {
					existing.Port = s.Port
				}
				continue // already named via mDNS
			}
			cp := s
			byIP[s.IP] = &cp
		}
	}

	// 3) Name the still-unnamed (scan-only) hosts: reverse DNS, then optional SNMP sysName.
	for _, p := range byIP {
		if p.Name != "" {
			continue
		}
		if host := reverseDNS(p.IP, opts.timeout()); host != "" {
			p.Name = host
			continue
		}
		if opts.SNMP {
			if name := GetSysName(p.IP, opts.Community, opts.timeout()); name != "" {
				p.Name = name
				p.Source = "snmp"
				continue
			}
		}
		p.Name = fmt.Sprintf("Printer @ %s:%d", p.IP, p.Port)
	}

	out := make([]Printer, 0, len(byIP))
	for _, p := range byIP {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out, nil
}

// reverseDNS returns the trimmed PTR hostname for an IP, or "".
func reverseDNS(ip string, to time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	var r net.Resolver
	names, err := r.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	name := names[0]
	for len(name) > 0 && name[len(name)-1] == '.' {
		name = name[:len(name)-1]
	}
	return name
}
