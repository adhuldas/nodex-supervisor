package location

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
)

type mockGPS struct {
	loc *backend.Location
	err error
}

func (m *mockGPS) GetLocation(ctx context.Context) (*backend.Location, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.loc, nil
}

type mockIPv6 struct {
	hasGlobal bool
	ip        string
	loc       *backend.Location
	err       error
}

func (m *mockIPv6) HasGlobalIPv6() (bool, string, error) {
	return m.hasGlobal, m.ip, nil
}

func (m *mockIPv6) GetLocation(ctx context.Context) (*backend.Location, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.loc, nil
}

func TestResolverHierarchy(t *testing.T) {
	ctx := context.Background()

	gpsFix := &backend.Location{Latitude: 10.0, Longitude: 20.0}
	provFix := &backend.Location{Latitude: 30.0, Longitude: 40.0}
	ipv6Fix := &backend.Location{Latitude: 50.0, Longitude: 60.0}

	// 1. GPS has priority
	r1 := NewResolver(Config{
		GPSProvider:  &mockGPS{loc: gpsFix},
		IPv6Provider: &mockIPv6{hasGlobal: true, loc: ipv6Fix},
		Provisioned:  provFix,
	})
	loc := r1.Resolve(ctx)
	if loc == nil || loc.Latitude != 10.0 || loc.Longitude != 20.0 {
		t.Fatalf("expected GPS fix, got %+v", loc)
	}
	if r1.Source() != "gps" {
		t.Fatalf("expected source 'gps', got %q", r1.Source())
	}

	// 2. GPS unavailable -> Provisioned has priority
	r2 := NewResolver(Config{
		GPSProvider:  &mockGPS{err: errors.New("no modem")},
		IPv6Provider: &mockIPv6{hasGlobal: true, loc: ipv6Fix},
		Provisioned:  provFix,
	})
	loc = r2.Resolve(ctx)
	if loc == nil || loc.Latitude != 30.0 || loc.Longitude != 40.0 {
		t.Fatalf("expected provisioned fix, got %+v", loc)
	}
	if r2.Source() != "provisioned" {
		t.Fatalf("expected source 'provisioned', got %q", r2.Source())
	}

	// 3. GPS & Provisioned unavailable -> IPv6 has priority
	r3 := NewResolver(Config{
		GPSProvider:  &mockGPS{err: errors.New("no modem")},
		IPv6Provider: &mockIPv6{hasGlobal: true, loc: ipv6Fix},
		Provisioned:  nil,
	})
	loc = r3.Resolve(ctx)
	if loc == nil || loc.Latitude != 50.0 || loc.Longitude != 60.0 {
		t.Fatalf("expected IPv6 fix, got %+v", loc)
	}
	if r3.Source() != "ipv6" {
		t.Fatalf("expected source 'ipv6', got %q", r3.Source())
	}

	// 4. All unavailable -> nil (backend locates by IP)
	r4 := NewResolver(Config{
		GPSProvider:  &mockGPS{err: errors.New("no modem")},
		IPv6Provider: &mockIPv6{hasGlobal: false, err: errors.New("no ipv6")},
		Provisioned:  nil,
	})
	loc = r4.Resolve(ctx)
	if loc != nil {
		t.Fatalf("expected nil location, got %+v", loc)
	}
	if r4.Source() != "none" {
		t.Fatalf("expected source 'none', got %q", r4.Source())
	}
}

func TestModemManagerJSONParsing(t *testing.T) {
	jsonOut := []byte(`{
		"modem": {
			"location": {
				"gps-raw": {
					"latitude": "12.9716",
					"longitude": "77.5946"
				}
			}
		}
	}`)

	loc, err := parseModemManagerJSON(jsonOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc.Latitude != 12.9716 || loc.Longitude != 77.5946 {
		t.Fatalf("got wrong coords: %+v", loc)
	}
}

func TestModemManagerTextParsing(t *testing.T) {
	textOut := `
--------------------------
GPS raw | UTC time: 153028.00
        | Longitude: 76.2673
        | Latitude: 9.9312
        | Altitude: 10.000000
`
	loc, err := parseModemManagerText(textOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc.Latitude != 9.9312 || loc.Longitude != 76.2673 {
		t.Fatalf("got wrong coords: %+v", loc)
	}
}

func TestNMEAParsing(t *testing.T) {
	// $GPRMC: 4807.038,N,01131.000,E -> 48 + 7.038/60 = 48.1173, 11 + 31.000/60 = 11.516667
	rmcValid := "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A"
	loc := ParseNMEA(rmcValid)
	if loc == nil {
		t.Fatalf("expected valid fix from RMC")
	}
	if diff := loc.Latitude - 48.1173; diff < -0.001 || diff > 0.001 {
		t.Fatalf("unexpected latitude: %f", loc.Latitude)
	}
	if diff := loc.Longitude - 11.516667; diff < -0.001 || diff > 0.001 {
		t.Fatalf("unexpected longitude: %f", loc.Longitude)
	}

	// Void status in RMC
	rmcVoid := "$GPRMC,123519,V,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A"
	if loc := ParseNMEA(rmcVoid); loc != nil {
		t.Fatalf("expected nil for void RMC fix")
	}

	// $GPGGA valid fix
	ggaValid := "$GPGGA,123519,4807.038,S,01131.000,W,1,08,0.9,545.4,M,46.9,M,,*47"
	locGGA := ParseNMEA(ggaValid)
	if locGGA == nil {
		t.Fatalf("expected valid fix from GGA")
	}
	if locGGA.Latitude >= 0 {
		t.Fatalf("expected negative latitude for S: %f", locGGA.Latitude)
	}
	if locGGA.Longitude >= 0 {
		t.Fatalf("expected negative longitude for W: %f", locGGA.Longitude)
	}

	// Quality 0 (invalid) in GGA
	ggaInvalid := "$GPGGA,123519,4807.038,N,01131.000,E,0,08,0.9,545.4,M,46.9,M,,*47"
	if loc := ParseNMEA(ggaInvalid); loc != nil {
		t.Fatalf("expected nil for invalid quality GGA")
	}
}

func TestIPv6Filtering(t *testing.T) {
	tests := []struct {
		ip       string
		isGlobal bool
	}{
		{"2001:4860:4860::8888", true},
		{"2607:f8b0:4005:805::200e", true},
		{"::1", false},                             // loopback
		{"fe80::1", false},                         // link-local
		{"fc00::1", false},                         // ULA
		{"fd12:3456:789a::1", false},               // ULA
		{"2001:db8::1", false},                     // documentation
		{"192.168.1.1", false},                     // IPv4
		{"::ffff:192.168.1.1", false},              // IPv4 mapped
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("failed to parse IP: %s", tt.ip)
		}
		got := isGlobalUnicastIPv6(ip)
		if got != tt.isGlobal {
			t.Errorf("isGlobalUnicastIPv6(%s) = %v; want %v", tt.ip, got, tt.isGlobal)
		}
	}
}

func TestHTTPClientIPv6Provider(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"latitude": 37.7749,
			"longitude": -122.4194,
			"ip": "2607:f8b0:4005:805::200e"
		}`))
	}))
	defer ts.Close()

	provider := &HTTPClientIPv6Provider{
		Endpoints:     []string{ts.URL},
		CacheDuration: 10 * time.Minute,
		HTTPClient:    ts.Client(),
		InterfaceChecker: func() ([]net.IP, error) {
			return []net.IP{
				net.ParseIP("fe80::1"),
				net.ParseIP("2607:f8b0:4005:805::200e"),
			}, nil
		},
	}

	hasGlobal, ip, err := provider.HasGlobalIPv6()
	if err != nil || !hasGlobal {
		t.Fatalf("expected global IPv6, got %v (err: %v)", hasGlobal, err)
	}
	if ip != "2607:f8b0:4005:805::200e" {
		t.Fatalf("unexpected ip: %s", ip)
	}

	ctx := context.Background()
	loc, err := provider.GetLocation(ctx)
	if err != nil {
		t.Fatalf("failed to get location: %v", err)
	}
	if loc.Latitude != 37.7749 || loc.Longitude != -122.4194 {
		t.Fatalf("got unexpected location: %+v", loc)
	}

	// Test cache: shutdown server, call again -> must succeed from cache
	ts.Close()
	locCached, err := provider.GetLocation(ctx)
	if err != nil {
		t.Fatalf("failed to get cached location: %v", err)
	}
	if locCached.Latitude != 37.7749 || locCached.Longitude != -122.4194 {
		t.Fatalf("got unexpected cached location: %+v", locCached)
	}
}
