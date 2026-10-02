package location

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
)

// Resolver resolves device location following a prioritized hierarchy:
// 1. Hardware GPS (Cellular GNSS via ModemManager / NMEA)
// 2. Provisioned static location (/boot/nodexa/config.json)
// 3. Global IPv6-based geolocation
// 4. Fallback to nil (nodexa-backend locates by incoming request IP)
type Resolver struct {
	gpsProvider  GPSProvider
	ipv6Provider IPv6Provider
	provisioned  *backend.Location

	mu         sync.RWMutex
	lastSource string
	lastLoc    *backend.Location
}

// Config configures the Resolver.
type Config struct {
	GPSProvider  GPSProvider
	IPv6Provider IPv6Provider
	Provisioned  *backend.Location
}

// NewResolver initializes a Resolver with the given configuration.
func NewResolver(cfg Config) *Resolver {
	gps := cfg.GPSProvider
	if gps == nil {
		gps = DefaultModemManagerGPS()
	}
	ipv6 := cfg.IPv6Provider
	if ipv6 == nil {
		ipv6 = NewHTTPClientIPv6Provider()
	}

	return &Resolver{
		gpsProvider:  gps,
		ipv6Provider: ipv6,
		provisioned:  cfg.Provisioned,
		lastSource:   "none",
	}
}

// Resolve queries available location providers in priority order.
func (r *Resolver) Resolve(ctx context.Context) *backend.Location {
	// Priority 1: Hardware GPS
	if r.gpsProvider != nil {
		gpsCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		loc, err := r.gpsProvider.GetLocation(gpsCtx)
		cancel()
		if err == nil && loc != nil && isValidFix(loc.Latitude, loc.Longitude) {
			r.setSource("gps", loc)
			return loc
		}
	}

	// Priority 2: Static Provisioned Location
	if r.provisioned != nil && isValidFix(r.provisioned.Latitude, r.provisioned.Longitude) {
		r.setSource("provisioned", r.provisioned)
		return r.provisioned
	}

	// Priority 3: IPv6 Geolocation
	if r.ipv6Provider != nil {
		hasIPv6, _, err := r.ipv6Provider.HasGlobalIPv6()
		if err == nil && hasIPv6 {
			ipv6Ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			loc, err := r.ipv6Provider.GetLocation(ipv6Ctx)
			cancel()
			if err == nil && loc != nil && isValidFix(loc.Latitude, loc.Longitude) {
				r.setSource("ipv6", loc)
				return loc
			}
		}
	}

	// Fallback: nil (nodexa-backend locates device by incoming request IP)
	r.setSource("none", nil)
	return nil
}

// Source returns the source of the most recently resolved location ("gps", "provisioned", "ipv6", or "none").
func (r *Resolver) Source() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastSource
}

func (r *Resolver) setSource(source string, loc *backend.Location) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastSource != source {
		log.Printf("location source changed: %s -> %s", r.lastSource, source)
		r.lastSource = source
	}
	r.lastLoc = loc
}
