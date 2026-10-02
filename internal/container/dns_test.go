package container

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func makeTestDNSQuery(id uint16, name string, qtype uint16) []byte {
	var buf []byte
	// Header
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[0:2], id)
	binary.BigEndian.PutUint16(hdr[2:4], 0x0100) // RD=1
	binary.BigEndian.PutUint16(hdr[4:6], 1)      // QDCOUNT=1
	buf = append(buf, hdr...)

	// Question QNAME
	parts := strings.Split(name, ".")
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		buf = append(buf, byte(len(p)))
		buf = append(buf, []byte(p)...)
	}
	buf = append(buf, 0) // zero length root

	// QTYPE & QCLASS
	qfooter := make([]byte, 4)
	binary.BigEndian.PutUint16(qfooter[0:2], qtype)
	binary.BigEndian.PutUint16(qfooter[2:4], 1) // IN
	buf = append(buf, qfooter...)
	return buf
}

func TestDNSServerLocalResolution(t *testing.T) {
	registry := map[string]net.IP{
		"smart-printer-firmware": net.ParseIP("172.17.0.3"),
		"nginx":                  net.ParseIP("172.17.0.2"),
		"printer-alias":          net.ParseIP("172.17.0.3"),
	}

	resolver := func(netName, hostname string) (net.IP, bool) {
		if netName != "bridge" {
			return nil, false
		}
		ip, ok := registry[hostname]
		return ip, ok
	}

	server, err := NewDNSServer("127.0.0.1:0", "bridge", resolver, nil)
	if err != nil {
		t.Fatalf("NewDNSServer: %v", err)
	}
	defer server.Close()

	serverAddr := server.conn.LocalAddr().String()

	// 1. Resolve smart-printer-firmware
	client, err := net.Dial("udp", serverAddr)
	if err != nil {
		t.Fatalf("dial DNS: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))

	query := makeTestDNSQuery(0x1234, "smart-printer-firmware", 1 /* A */)
	if _, err := client.Write(query); err != nil {
		t.Fatalf("write query: %v", err)
	}

	resp := make([]byte, 512)
	n, err := client.Read(resp)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp = resp[:n]

	if len(resp) < 16 {
		t.Fatalf("response too short: %d", len(resp))
	}
	// Check ID matches
	if binary.BigEndian.Uint16(resp[0:2]) != 0x1234 {
		t.Errorf("expected ID 0x1234, got 0x%x", binary.BigEndian.Uint16(resp[0:2]))
	}
	// Check ANCOUNT == 1
	ancount := binary.BigEndian.Uint16(resp[6:8])
	if ancount != 1 {
		t.Fatalf("expected 1 answer, got %d", ancount)
	}
	// Check answer IP at end is 172.17.0.3
	answerIP := net.IP(resp[n-4 : n])
	if answerIP.String() != "172.17.0.3" {
		t.Errorf("expected 172.17.0.3, got %s", answerIP)
	}

	// 2. Resolve printer-alias
	queryAlias := makeTestDNSQuery(0x5678, "printer-alias", 1 /* A */)
	if _, err := client.Write(queryAlias); err != nil {
		t.Fatalf("write query: %v", err)
	}
	n, err = client.Read(resp)
	if err != nil {
		t.Fatalf("read alias response: %v", err)
	}
	resp = resp[:n]
	answerIP = net.IP(resp[n-4 : n])
	if answerIP.String() != "172.17.0.3" {
		t.Errorf("expected 172.17.0.3 for alias, got %s", answerIP)
	}

	// 3. Resolve unknown name returns NXDOMAIN or forwarded
	queryUnknown := makeTestDNSQuery(0x9999, "unknown-service-nodexa-test-never-exists", 1 /* A */)
	if _, err := client.Write(queryUnknown); err != nil {
		t.Fatalf("write query: %v", err)
	}
	n, err = client.Read(resp)
	if err != nil {
		t.Fatalf("read unknown response: %v", err)
	}
	resp = resp[:n]
	rcode := resp[3] & 0x0f
	if rcode != 3 /* NXDOMAIN */ && rcode != 0 {
		t.Errorf("expected NXDOMAIN (3) or NoError, got rcode %d", rcode)
	}
}

func TestDNSServerNetworkIsolation(t *testing.T) {
	// smart-printer-firmware registered on "net-a", should not resolve on "net-b"
	resolver := func(netName, hostname string) (net.IP, bool) {
		if netName == "net-a" && hostname == "smart-printer-firmware" {
			return net.ParseIP("172.18.0.2"), true
		}
		return nil, false
	}

	// Server bound to net-b
	server, err := NewDNSServer("127.0.0.1:0", "net-b", resolver, []string{})
	if err != nil {
		t.Fatalf("NewDNSServer: %v", err)
	}
	defer server.Close()

	client, err := net.Dial("udp", server.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))

	query := makeTestDNSQuery(0x1111, "smart-printer-firmware", 1)
	_, _ = client.Write(query)

	resp := make([]byte, 512)
	n, err := client.Read(resp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	resp = resp[:n]
	rcode := resp[3] & 0x0f
	if rcode != 3 /* NXDOMAIN */ {
		t.Errorf("expected NXDOMAIN due to network isolation, got rcode %d", rcode)
	}
}

func TestDNSServerExternalForwarding(t *testing.T) {
	// Upstream pointing to Google public DNS
	server, err := NewDNSServer("127.0.0.1:0", "bridge", nil, []string{"8.8.8.8:53"})
	if err != nil {
		t.Fatalf("NewDNSServer: %v", err)
	}
	defer server.Close()

	client, err := net.Dial("udp", server.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))

	query := makeTestDNSQuery(0x7777, "google.com", 1)
	if _, err := client.Write(query); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp := make([]byte, 512)
	n, err := client.Read(resp)
	if err != nil {
		t.Skipf("skipping external forwarding test if network unavailable: %v", err)
		return
	}
	resp = resp[:n]
	if len(resp) < 12 {
		t.Fatalf("response too short")
	}
	rcode := resp[3] & 0x0f
	if rcode != 0 {
		t.Errorf("expected NoError for google.com, got rcode %d", rcode)
	}
	ancount := binary.BigEndian.Uint16(resp[6:8])
	if ancount == 0 {
		t.Errorf("expected at least 1 answer for google.com")
	}
}
