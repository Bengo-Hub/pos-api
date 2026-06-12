package discovery

import (
	"net"
	"time"
)

// Minimal, dependency-free SNMP v1 GET for the printer's sysName, used only to put a human name on a
// bare port-9100 printer that doesn't advertise mDNS. It hand-encodes one GetRequest and loosely
// parses the reply; any malformed/absent answer yields "" (callers fall back to the IP).

// sysNameOID = 1.3.6.1.2.1.1.5.0, pre-encoded as BER OID value bytes (first two arcs 1.3 → 0x2B).
var sysNameOID = []byte{0x2b, 0x06, 0x01, 0x02, 0x01, 0x01, 0x05, 0x00}

// GetSysName returns the SNMP sysName (1.3.6.1.2.1.1.5.0) for ip, or "" on any failure.
func GetSysName(ip, community string, timeout time.Duration) string {
	if community == "" {
		community = "public"
	}
	pkt := buildSNMPGet(community, sysNameOID, 1)

	conn, err := net.DialTimeout("udp", net.JoinHostPort(ip, "161"), timeout)
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(pkt); err != nil {
		return ""
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		return ""
	}
	return parseSNMPSysName(buf[:n])
}

// ── tiny BER helpers (single-byte lengths only — our packets are well under 128 bytes) ──────────

func tlv(tag byte, content []byte) []byte {
	out := make([]byte, 0, len(content)+2)
	out = append(out, tag, byte(len(content)))
	return append(out, content...)
}

func berInt(v int) []byte {
	// Only used for small non-negative values (version 0, request-id, error fields).
	if v == 0 {
		return tlv(0x02, []byte{0x00})
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte(v & 0xff)}, b...)
		v >>= 8
	}
	if b[0]&0x80 != 0 { // keep it positive
		b = append([]byte{0x00}, b...)
	}
	return tlv(0x02, b)
}

// buildSNMPGet encodes an SNMP v1 GetRequest for a single OID.
func buildSNMPGet(community string, oid []byte, reqID int) []byte {
	varbind := tlv(0x30, append(tlv(0x06, oid), tlv(0x05, nil)...)) // SEQUENCE { OID, NULL }
	varbindList := tlv(0x30, varbind)

	pduBody := make([]byte, 0, 32)
	pduBody = append(pduBody, berInt(reqID)...) // request-id
	pduBody = append(pduBody, berInt(0)...)     // error-status
	pduBody = append(pduBody, berInt(0)...)     // error-index
	pduBody = append(pduBody, varbindList...)
	pdu := tlv(0xA0, pduBody) // GetRequest-PDU

	msgBody := make([]byte, 0, 48)
	msgBody = append(msgBody, berInt(0)...)                  // version: 0 (v1)
	msgBody = append(msgBody, tlv(0x04, []byte(community))...) // community
	msgBody = append(msgBody, pdu...)
	return tlv(0x30, msgBody)
}

// readTLV splits one BER element off the front of b. Supports short and long-form lengths.
func readTLV(b []byte) (tag byte, content, rest []byte, ok bool) {
	if len(b) < 2 {
		return 0, nil, nil, false
	}
	tag = b[0]
	l := int(b[1])
	hdr := 2
	if l&0x80 != 0 { // long form
		nb := l & 0x7f
		if nb == 0 || len(b) < 2+nb {
			return 0, nil, nil, false
		}
		l = 0
		for i := 0; i < nb; i++ {
			l = (l << 8) | int(b[2+i])
		}
		hdr = 2 + nb
	}
	if len(b) < hdr+l {
		return 0, nil, nil, false
	}
	return tag, b[hdr : hdr+l], b[hdr+l:], true
}

// parseSNMPSysName descends the known GetResponse structure and returns the varbind's OCTET STRING.
func parseSNMPSysName(resp []byte) string {
	_, body, _, ok := readTLV(resp) // outer SEQUENCE
	if !ok {
		return ""
	}
	_, _, rest, ok := readTLV(body) // version
	if !ok {
		return ""
	}
	_, _, rest, ok = readTLV(rest) // community
	if !ok {
		return ""
	}
	_, pdu, _, ok := readTLV(rest) // GetResponse-PDU (0xA2)
	if !ok {
		return ""
	}
	_, _, p, ok := readTLV(pdu) // request-id
	if !ok {
		return ""
	}
	_, _, p, ok = readTLV(p) // error-status
	if !ok {
		return ""
	}
	_, _, p, ok = readTLV(p) // error-index
	if !ok {
		return ""
	}
	_, vbl, _, ok := readTLV(p) // varbind list SEQUENCE
	if !ok {
		return ""
	}
	_, vb, _, ok := readTLV(vbl) // first varbind SEQUENCE
	if !ok {
		return ""
	}
	_, _, afterOID, ok := readTLV(vb) // OID
	if !ok {
		return ""
	}
	tag, val, _, ok := readTLV(afterOID) // value
	if !ok || tag != 0x04 {              // expect OCTET STRING
		return ""
	}
	return string(val)
}
