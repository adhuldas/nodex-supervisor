package location

import (
	"strconv"
	"strings"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
)

// ParseNMEA parses an NMEA sentence (e.g. $GPRMC, $GNRMC, $GPGGA, $GNGGA)
// and returns a *backend.Location if a valid fix is found.
func ParseNMEA(sentence string) *backend.Location {
	sentence = strings.TrimSpace(sentence)
	// Strip checksum if present
	if idx := strings.Index(sentence, "*"); idx != -1 {
		sentence = sentence[:idx]
	}
	parts := strings.Split(sentence, ",")
	if len(parts) < 7 {
		return nil
	}

	talker := strings.ToUpper(parts[0])
	// Support GPS (GP), GLONASS (GL), Galileo (GA), BeiDou (GB), Multi-GNSS (GN)
	if strings.HasSuffix(talker, "RMC") {
		// Format: $--RMC,time,status,lat,lat_dir,lon,lon_dir,...
		// status: 'A' = active, 'V' = void
		if parts[2] != "A" {
			return nil
		}
		lat, ok1 := parseNMEACoord(parts[3], parts[4], 2)
		lon, ok2 := parseNMEACoord(parts[5], parts[6], 3)
		if ok1 && ok2 && isValidFix(lat, lon) {
			return &backend.Location{Latitude: lat, Longitude: lon}
		}
	} else if strings.HasSuffix(talker, "GGA") {
		// Format: $--GGA,time,lat,lat_dir,lon,lon_dir,quality,...
		// quality: 0 = invalid
		if len(parts) > 6 && parts[6] == "0" {
			return nil
		}
		lat, ok1 := parseNMEACoord(parts[2], parts[3], 2)
		lon, ok2 := parseNMEACoord(parts[4], parts[5], 3)
		if ok1 && ok2 && isValidFix(lat, lon) {
			return &backend.Location{Latitude: lat, Longitude: lon}
		}
	}

	return nil
}

// parseNMEACoord parses DDMM.MMMM (degDigits=2) or DDDMM.MMMM (degDigits=3)
func parseNMEACoord(val, dir string, degDigits int) (float64, bool) {
	val = strings.TrimSpace(val)
	dir = strings.ToUpper(strings.TrimSpace(dir))
	dotIdx := strings.Index(val, ".")
	if dotIdx < degDigits {
		return 0, false
	}

	degStr := val[:dotIdx-2]
	minStr := val[dotIdx-2:]

	deg, err1 := strconv.ParseFloat(degStr, 64)
	min, err2 := strconv.ParseFloat(minStr, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}

	result := deg + (min / 60.0)
	if dir == "S" || dir == "W" {
		result = -result
	}
	return result, true
}
