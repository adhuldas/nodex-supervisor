package location

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
)

// IPv6Provider checks for global IPv6 connectivity and resolves location over IPv6.
type IPv6Provider interface {
	HasGlobalIPv6() (bool, string, error)
	GetLocation(ctx context.Context) (*backend.Location, error)
}

// HTTPClientIPv6Provider inspects local interfaces for global IPv6 and queries
// geolocation endpoints strictly over IPv6.
type HTTPClientIPv6Provider struct {
	// Endpoints to query, in order of preference.
	Endpoints []string
	// CacheDuration is how long a resolved location remains valid.
	CacheDuration time.Duration
	// HTTPClient to use. If nil, a client restricted to tcp6 dialing is used.
	HTTPClient *http.Client
	// InterfaceChecker retrieves local interface addresses. If nil, net.Interfaces is used.
	InterfaceChecker func() ([]net.IP, error)

	mu            sync.Mutex
	cachedLoc     *backend.Location
	cachedIP      string
	cachedAt      time.Time
}

// DefaultEndpoints are public geolocation APIs that support HTTPS and IPv6.
// If an endpoint contains %s, it will be formatted with the detected global IPv6 address.
var DefaultEndpoints = []string{
	"https://ipwho.is/%s",
	"https://ipapi.co/%s/json/",
}

// NewHTTPClientIPv6Provider returns an initialized HTTPClientIPv6Provider.
func NewHTTPClientIPv6Provider() *HTTPClientIPv6Provider {
	dialer := &net.Dialer{
		Timeout: 4 * time.Second,
	}

	// Restricting the dialer to "tcp6" forces outgoing connections to use IPv6
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp6", addr)
		},
		ResponseHeaderTimeout: 5 * time.Second,
	}

	return &HTTPClientIPv6Provider{
		Endpoints:     DefaultEndpoints,
		CacheDuration: 1 * time.Hour,
		HTTPClient: &http.Client{
			Transport: transport,
			Timeout:   6 * time.Second,
		},
	}
}

// HasGlobalIPv6 checks whether any active, non-loopback network interface
// has a public global unicast IPv6 address.
func (p *HTTPClientIPv6Provider) HasGlobalIPv6() (bool, string, error) {
	var ips []net.IP
	if p.InterfaceChecker != nil {
		var err error
		ips, err = p.InterfaceChecker()
		if err != nil {
			return false, "", err
		}
	} else {
		var err error
		ips, err = defaultInterfaceIPs()
		if err != nil {
			return false, "", err
		}
	}

	for _, ip := range ips {
		if isGlobalUnicastIPv6(ip) {
			return true, ip.String(), nil
		}
	}
	return false, "", nil
}

func defaultInterfaceIPs() ([]net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, iface := range ifaces {
		// Ignore interfaces that are down or loopback
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	return ips, nil
}

// isGlobalUnicastIPv6 returns true if ip is a globally routable IPv6 address.
func isGlobalUnicastIPv6(ip net.IP) bool {
	// Must be IPv6 (not IPv4 or IPv4-mapped)
	if ip.To4() != nil || len(ip) != net.IPv6len {
		return false
	}
	// Must be global unicast
	if !ip.IsGlobalUnicast() {
		return false
	}
	// Exclude Unique Local Addresses (fc00::/7)
	if (ip[0] & 0xfe) == 0xfc {
		return false
	}
	// Exclude Link-Local (fe80::/10)
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	// Exclude Documentation prefix (2001:db8::/32)
	if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return false
	}
	return true
}

type geoIPResponse struct {
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	IP        string   `json:"ip"`
	Error     bool     `json:"error"`
	Success   *bool    `json:"success"`
}

// GetLocation resolves location via IPv6. If cached and valid, returns cached coordinates.
func (p *HTTPClientIPv6Provider) GetLocation(ctx context.Context) (*backend.Location, error) {
	hasIPv6, currentIP, err := p.HasGlobalIPv6()
	if err != nil || !hasIPv6 {
		return nil, fmt.Errorf("no global IPv6 address detected")
	}

	p.mu.Lock()
	if p.cachedLoc != nil && p.cachedIP == currentIP && time.Since(p.cachedAt) < p.CacheDuration {
		loc := *p.cachedLoc
		p.mu.Unlock()
		return &loc, nil
	}
	p.mu.Unlock()

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	endpoints := p.Endpoints
	if len(endpoints) == 0 {
		endpoints = DefaultEndpoints
	}

	var lastErr error
	for _, ep := range endpoints {
		urlStr := ep
		if strings.Contains(ep, "%s") {
			urlStr = fmt.Sprintf(ep, currentIP)
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if reqErr != nil {
			lastErr = reqErr
			continue
		}
		req.Header.Set("User-Agent", "nodexa-agent/1.0")

		resp, respErr := client.Do(req)
		if respErr != nil {
			lastErr = respErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("endpoint %s returned status %d", ep, resp.StatusCode)
			continue
		}

		var geo geoIPResponse
		decErr := json.NewDecoder(resp.Body).Decode(&geo)
		resp.Body.Close()
		if decErr != nil {
			lastErr = decErr
			continue
		}

		if geo.Error || (geo.Success != nil && !*geo.Success) {
			lastErr = fmt.Errorf("geolocation API error from %s", ep)
			continue
		}

		if geo.Latitude != nil && geo.Longitude != nil && isValidFix(*geo.Latitude, *geo.Longitude) {
			loc := &backend.Location{
				Latitude:  *geo.Latitude,
				Longitude: *geo.Longitude,
			}
			p.mu.Lock()
			p.cachedLoc = loc
			p.cachedIP = currentIP
			p.cachedAt = time.Now()
			p.mu.Unlock()
			return loc, nil
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("ipv6 geolocation failed: %w", lastErr)
	}
	return nil, fmt.Errorf("ipv6 geolocation returned no valid coordinates")
}
