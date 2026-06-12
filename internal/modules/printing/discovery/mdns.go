package discovery

import (
	"context"
	"net"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// printerServices are the DNS-SD service types receipt/label/office printers commonly advertise.
var printerServices = []string{
	"_ipp._tcp.local.",
	"_ipps._tcp.local.",
	"_printer._tcp.local.",
	"_pdl-datastream._tcp.local.", // raw 9100 / JetDirect
}

const mdnsGroup = "224.0.0.251:5353"

// DiscoverMDNS sends one multicast DNS query for the printer service types and collects responses
// for `timeout`, resolving PTR → SRV → A (+ TXT) into named printers. Best-effort: it returns
// whatever it could parse. The query sets the unicast-response (QU) bit so replies come back to our
// ephemeral socket (more reliable than re-joining the multicast group).
func DiscoverMDNS(ctx context.Context, timeout time.Duration) ([]Printer, error) {
	group, err := net.ResolveUDPAddr("udp4", mdnsGroup)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	query, err := buildMDNSQuery(printerServices)
	if err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP(query, group); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	_ = conn.SetReadDeadline(deadline)

	// Accumulated records keyed by DNS name (lower-cased, trailing dot kept).
	ptr := map[string]string{}        // instance -> service
	srvTarget := map[string]string{}  // instance -> host
	srvPort := map[string]int{}       // instance -> port
	aRec := map[string]string{}       // host -> ipv4
	txtName := map[string]string{}    // instance -> friendly name (TXT ty/product)
	txtModel := map[string]string{}   // instance -> model (TXT product)

	buf := make([]byte, 9000)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // deadline reached or socket closed
		}
		parseMDNS(buf[:n], ptr, srvTarget, srvPort, aRec, txtName, txtModel)
	}

	var printers []Printer
	for instance, svc := range ptr {
		host := srvTarget[instance]
		ip := aRec[host]
		if ip == "" {
			continue // no address resolved this round; the subnet scan may still find it
		}
		name := txtName[instance]
		if name == "" {
			name = instanceLabel(instance, svc)
		}
		printers = append(printers, Printer{
			Name:     name,
			IP:       ip,
			Port:     srvPort[instance],
			Source:   "mdns",
			Model:    txtModel[instance],
			Services: []string{shortService(svc)},
		})
	}
	return printers, nil
}

// buildMDNSQuery encodes a single mDNS query with a PTR question per service type, requesting a
// unicast response (QU bit) so replies return to our socket.
func buildMDNSQuery(services []string) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	const unicastResponse = 0x8000
	for _, s := range services {
		name, err := dnsmessage.NewName(s)
		if err != nil {
			return nil, err
		}
		q := dnsmessage.Question{
			Name:  name,
			Type:  dnsmessage.TypePTR,
			Class: dnsmessage.Class(unicastResponse) | dnsmessage.ClassINET,
		}
		if err := b.Question(q); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// parseMDNS walks one response message, filling the record maps. Records may arrive across the
// Answer and Additional sections, so both are parsed; Authorities are skipped.
func parseMDNS(
	msg []byte,
	ptr, srvTarget map[string]string, srvPort map[string]int,
	aRec, txtName, txtModel map[string]string,
) {
	var p dnsmessage.Parser
	if _, err := p.Start(msg); err != nil {
		return
	}
	if err := p.SkipAllQuestions(); err != nil {
		return
	}

	consume := func(hdr dnsmessage.ResourceHeader) bool {
		name := strings.ToLower(hdr.Name.String())
		switch hdr.Type {
		case dnsmessage.TypePTR:
			r, err := p.PTRResource()
			if err != nil {
				return false
			}
			ptr[strings.ToLower(r.PTR.String())] = name
			return true
		case dnsmessage.TypeSRV:
			r, err := p.SRVResource()
			if err != nil {
				return false
			}
			srvTarget[name] = strings.ToLower(r.Target.String())
			srvPort[name] = int(r.Port)
			return true
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return false
			}
			aRec[name] = net.IP(r.A[:]).String()
			return true
		case dnsmessage.TypeTXT:
			r, err := p.TXTResource()
			if err != nil {
				return false
			}
			applyTXT(name, r.TXT, txtName, txtModel)
			return true
		default:
			return false
		}
	}

	// Answers.
	for {
		hdr, err := p.AnswerHeader()
		if err != nil {
			break
		}
		if !consume(hdr) {
			if err := p.SkipAnswer(); err != nil {
				break
			}
		}
	}
	if err := p.SkipAllAuthorities(); err != nil {
		return
	}
	// Additionals (SRV/TXT/A frequently live here).
	for {
		hdr, err := p.AdditionalHeader()
		if err != nil {
			break
		}
		if !consume(hdr) {
			if err := p.SkipAdditional(); err != nil {
				break
			}
		}
	}
}

// applyTXT pulls a friendly name (ty / product) out of DNS-SD TXT key=value strings.
func applyTXT(instance string, txt []string, txtName, txtModel map[string]string) {
	for _, kv := range txt {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToLower(kv[:eq])
		val := strings.Trim(kv[eq+1:], "()")
		if val == "" {
			continue
		}
		switch key {
		case "ty": // human-readable model, e.g. "EPSON TM-T20III"
			if txtName[instance] == "" {
				txtName[instance] = val
			}
			txtModel[instance] = val
		case "product":
			txtModel[instance] = val
			if txtName[instance] == "" {
				txtName[instance] = val
			}
		case "note": // often the location/printer name set by the admin
			if txtName[instance] == "" {
				txtName[instance] = val
			}
		}
	}
}

// instanceLabel returns the service-instance label (the friendly part) of a DNS-SD name, e.g.
// "EPSON TM-T20III._ipp._tcp.local." with service "_ipp._tcp.local." → "EPSON TM-T20III".
func instanceLabel(instance, service string) string {
	label := strings.TrimSuffix(instance, "."+service)
	label = strings.TrimSuffix(label, ".")
	if label == "" || label == instance {
		return strings.TrimSuffix(instance, ".")
	}
	return label
}

// shortService trims a DNS-SD service to a compact tag, e.g. "_ipp._tcp.local." → "ipp".
func shortService(service string) string {
	s := strings.TrimSuffix(service, ".local.")
	s = strings.TrimPrefix(s, "_")
	if i := strings.Index(s, "._"); i >= 0 {
		s = s[:i]
	}
	return s
}
