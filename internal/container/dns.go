package container

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// DNSServer is a lightweight RFC 1035 UDP DNS responder that resolves local
// container names on a specific network and forwards other queries to upstream DNS.
type DNSServer struct {
	mu          sync.Mutex
	listenAddr  string
	networkName string
	resolver    func(netName, hostname string) (net.IP, bool)
	upstreams   []string
	conn        *net.UDPConn
	closed      bool
	closeChan   chan struct{}
}

// NewDNSServer creates and starts a DNS server on listenAddr (e.g. "172.17.0.1:53")
// for the given container network.
func NewDNSServer(listenAddr, networkName string, resolver func(string, string) (net.IP, bool), upstreams []string) (*DNSServer, error) {
	if upstreams == nil {
		upstreams = getHostNameservers()
	}

	uAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolving DNS listen address %s: %w", listenAddr, err)
	}

	conn, err := net.ListenUDP("udp", uAddr)
	if err != nil {
		return nil, fmt.Errorf("listening on DNS address %s: %w", listenAddr, err)
	}

	s := &DNSServer{
		listenAddr:  listenAddr,
		networkName: networkName,
		resolver:    resolver,
		upstreams:   upstreams,
		conn:        conn,
		closeChan:   make(chan struct{}),
	}

	go s.serve()
	return s, nil
}

// Close stops the DNS server.
func (s *DNSServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closeChan)
	conn := s.conn
	s.mu.Unlock()

	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (s *DNSServer) serve() {
	buf := make([]byte, 1024)
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		n, remoteAddr, err := s.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-s.closeChan:
				return
			default:
				continue
			}
		}

		if n < 12 {
			continue // DNS header is minimum 12 bytes
		}

		queryData := make([]byte, n)
		copy(queryData, buf[:n])

		go s.handleQuery(queryData, remoteAddr)
	}
}

func (s *DNSServer) handleQuery(query []byte, remote net.Addr) {
	id, qname, qtype, _, _, err := parseDNSQuery(query)
	if err != nil {
		return
	}

	// Clean name for lookup (strip trailing dot, lowercase)
	lookupName := strings.ToLower(strings.TrimSuffix(qname, "."))

	// Check if this matches a local container on this network
	if s.resolver != nil && (qtype == 1 /* A */ || qtype == 255 /* ANY */) {
		if ip, ok := s.resolver(s.networkName, lookupName); ok && ip != nil {
			resp := buildDNSResponse(query, id, qname, ip, 5)
			_, _ = s.conn.WriteTo(resp, remote)
			return
		}
	}

	// For AAAA queries of local containers, return NoError with 0 answers (no IPv6 address)
	if s.resolver != nil && qtype == 28 /* AAAA */ {
		if _, ok := s.resolver(s.networkName, lookupName); ok {
			resp := buildEmptyResponse(query, id)
			_, _ = s.conn.WriteTo(resp, remote)
			return
		}
	}

	// Forward non-local or public domains to upstream
	if len(s.upstreams) > 0 {
		resp, err := forwardQuery(query, s.upstreams)
		if err == nil && len(resp) >= 12 {
			_, _ = s.conn.WriteTo(resp, remote)
			return
		}
	}

	// Fallback to NXDOMAIN
	resp := buildNXDomainResponse(query, id)
	_, _ = s.conn.WriteTo(resp, remote)
}

func parseDNSQuery(buf []byte) (id uint16, qname string, qtype, qclass uint16, qend int, err error) {
	if len(buf) < 12 {
		return 0, "", 0, 0, 0, fmt.Errorf("truncated DNS header")
	}

	id = binary.BigEndian.Uint16(buf[0:2])
	qdcount := binary.BigEndian.Uint16(buf[4:6])
	if qdcount == 0 {
		return id, "", 0, 0, 12, fmt.Errorf("no questions")
	}

	offset := 12
	var labels []string
	for offset < len(buf) {
		length := int(buf[offset])
		offset++
		if length == 0 {
			break
		}
		if offset+length > len(buf) {
			return id, "", 0, 0, 0, fmt.Errorf("label length overflow")
		}
		labels = append(labels, string(buf[offset:offset+length]))
		offset += length
	}

	if offset+4 > len(buf) {
		return id, "", 0, 0, 0, fmt.Errorf("truncated question footer")
	}

	qtype = binary.BigEndian.Uint16(buf[offset : offset+2])
	qclass = binary.BigEndian.Uint16(buf[offset+2 : offset+4])
	qend = offset + 4
	qname = strings.Join(labels, ".")
	return id, qname, qtype, qclass, qend, nil
}

func buildDNSResponse(query []byte, id uint16, qname string, ip net.IP, ttl uint32) []byte {
	ip4 := ip.To4()
	if ip4 == nil {
		return buildEmptyResponse(query, id)
	}

	// Find end of question in query
	_, _, _, _, qend, err := parseDNSQuery(query)
	if err != nil || qend > len(query) {
		qend = len(query)
	}

	resp := make([]byte, qend+16)
	copy(resp[:qend], query[:qend])

	// Header: QR=1 (response), AA=1 (authoritative), RA=1, RCODE=0
	resp[0] = byte(id >> 8)
	resp[1] = byte(id)
	resp[2] = 0x85 // QR=1, AA=1, RD=1
	resp[3] = 0x80 // RA=1, RCODE=0 (NoError)
	resp[4] = 0x00 // QDCOUNT high
	resp[5] = 0x01 // QDCOUNT low
	resp[6] = 0x00 // ANCOUNT high
	resp[7] = 0x01 // ANCOUNT low (1 answer)
	resp[8] = 0x00 // NSCOUNT
	resp[9] = 0x00
	resp[10] = 0x00 // ARCOUNT
	resp[11] = 0x00

	// Answer RR:
	// NAME: pointer to offset 12 (0xc00c)
	resp[qend] = 0xc0
	resp[qend+1] = 0x0c
	// TYPE: A (1)
	binary.BigEndian.PutUint16(resp[qend+2:qend+4], 1)
	// CLASS: IN (1)
	binary.BigEndian.PutUint16(resp[qend+4:qend+6], 1)
	// TTL
	binary.BigEndian.PutUint32(resp[qend+6:qend+10], ttl)
	// RDLENGTH: 4
	binary.BigEndian.PutUint16(resp[qend+10:qend+12], 4)
	// RDATA: 4 bytes IP
	copy(resp[qend+12:qend+16], ip4)

	return resp
}

func buildEmptyResponse(query []byte, id uint16) []byte {
	_, _, _, _, qend, err := parseDNSQuery(query)
	if err != nil || qend > len(query) {
		qend = len(query)
	}

	resp := make([]byte, qend)
	copy(resp, query[:qend])
	resp[0] = byte(id >> 8)
	resp[1] = byte(id)
	resp[2] = 0x85 // QR=1, AA=1, RD=1
	resp[3] = 0x80 // RA=1, RCODE=0 (NoError)
	resp[6] = 0x00 // ANCOUNT = 0
	resp[7] = 0x00
	return resp
}

func buildNXDomainResponse(query []byte, id uint16) []byte {
	_, _, _, _, qend, err := parseDNSQuery(query)
	if err != nil || qend > len(query) {
		qend = len(query)
	}

	resp := make([]byte, qend)
	copy(resp, query[:qend])
	resp[0] = byte(id >> 8)
	resp[1] = byte(id)
	resp[2] = 0x81 // QR=1, RD=1
	resp[3] = 0x83 // RA=1, RCODE=3 (NXDOMAIN)
	resp[6] = 0x00 // ANCOUNT = 0
	resp[7] = 0x00
	return resp
}

func forwardQuery(query []byte, upstreams []string) ([]byte, error) {
	for _, up := range upstreams {
		if !strings.Contains(up, ":") {
			up = up + ":53"
		}
		c, err := net.DialTimeout("udp", up, 1500*time.Millisecond)
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write(query); err != nil {
			_ = c.Close()
			continue
		}
		resp := make([]byte, 1024)
		n, err := c.Read(resp)
		_ = c.Close()
		if err == nil && n >= 12 {
			return resp[:n], nil
		}
	}
	return nil, fmt.Errorf("all upstreams failed")
}

// getHostNameservers reads nameservers from host resolv.conf, filtering out
// loopback, link-local, and bridge gateway addresses.
func getHostNameservers() []string {
	var results []string

	paths := []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "nameserver") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					ns := fields[1]
					// Filter out loopback (127.0.0.1, 127.0.0.53) and IPv6 link-local
					if !strings.HasPrefix(ns, "127.") && !strings.HasPrefix(ns, "::") && !strings.HasPrefix(ns, "fe80:") {
						results = append(results, ns+":53")
					}
				}
			}
		}
		_ = f.Close()
		if len(results) > 0 {
			break
		}
	}

	if len(results) == 0 {
		// Fallbacks: Cloudflare & Google public DNS
		results = []string{"1.1.1.1:53", "8.8.8.8:53"}
	}

	return results
}
