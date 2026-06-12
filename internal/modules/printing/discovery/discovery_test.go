package discovery

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// TestScanSubnetFindsOpenPort verifies the TCP scanner reports a host with an open printer port.
func TestScanSubnetFindsOpenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := ScanSubnet(ctx, "127.0.0.1/32", Options{Ports: []int{port}, Timeout: time.Second})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].IP != "127.0.0.1" || got[0].Port != port {
		t.Fatalf("expected one hit 127.0.0.1:%d, got %+v", port, got)
	}
}

// TestSNMPRoundtrip checks the SNMP GET encoder and the response parser agree on a sysName value.
func TestSNMPRoundtrip(t *testing.T) {
	// A well-formed request should at least parse as an outer SEQUENCE.
	req := buildSNMPGet("public", sysNameOID, 1)
	if tag, _, _, ok := readTLV(req); !ok || tag != 0x30 {
		t.Fatalf("request is not a valid BER SEQUENCE (tag=%#x ok=%v)", tag, ok)
	}

	const want = "FRONT-DESK-EPSON"
	varbind := tlv(0x30, append(tlv(0x06, sysNameOID), tlv(0x04, []byte(want))...))
	vbl := tlv(0x30, varbind)
	pduBody := append(append(append(berInt(1), berInt(0)...), berInt(0)...), vbl...)
	pdu := tlv(0xA2, pduBody) // GetResponse-PDU
	msgBody := append(append(berInt(0), tlv(0x04, []byte("public"))...), pdu...)
	resp := tlv(0x30, msgBody)

	if got := parseSNMPSysName(resp); got != want {
		t.Fatalf("parseSNMPSysName = %q, want %q", got, want)
	}
}

// TestInstanceLabel checks DNS-SD instance-name extraction.
func TestInstanceLabel(t *testing.T) {
	got := instanceLabel("EPSON TM-T20III._ipp._tcp.local.", "_ipp._tcp.local.")
	if got != "EPSON TM-T20III" {
		t.Fatalf("instanceLabel = %q, want %q", got, "EPSON TM-T20III")
	}
}
