package location

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
)

// GPSProvider obtains position fixes from hardware GPS.
type GPSProvider interface {
	GetLocation(ctx context.Context) (*backend.Location, error)
}

// ModemManagerGPS queries cellular modem GNSS via mmcli.
type ModemManagerGPS struct {
	// BinPath is the path to mmcli (defaults to "/usr/bin/mmcli").
	BinPath string
	// Timeout per invocation (defaults to 3 seconds).
	Timeout time.Duration
	// CommandRunner executes commands, allowing unit test injection.
	CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// DefaultModemManagerGPS returns a ModemManagerGPS with standard defaults.
func DefaultModemManagerGPS() *ModemManagerGPS {
	return &ModemManagerGPS{
		BinPath: "/usr/bin/mmcli",
		Timeout: 3 * time.Second,
		CommandRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			return cmd.CombinedOutput()
		},
	}
}

var (
	reLatitude  = regexp.MustCompile(`(?i)(?:Latitude|lat)\s*[:=]\s*([+-]?\d+(?:\.\d+)?)`)
	reLongitude = regexp.MustCompile(`(?i)(?:Longitude|lon)\s*[:=]\s*([+-]?\d+(?:\.\d+)?)`)
)

type mmcliLocationJSON struct {
	Modem struct {
		Location struct {
			GPSRaw struct {
				Latitude  string `json:"latitude"`
				Longitude string `json:"longitude"`
			} `json:"gps-raw"`
			GPSNmea []string `json:"gps-nmea"`
		} `json:"location"`
	} `json:"modem"`
}

// GetLocation tries to fetch raw GPS coordinates or NMEA fixes from ModemManager.
func (m *ModemManagerGPS) GetLocation(ctx context.Context) (*backend.Location, error) {
	runner := m.CommandRunner
	if runner == nil {
		runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			return cmd.CombinedOutput()
		}
	}
	binPath := m.BinPath
	if binPath == "" {
		binPath = "/usr/bin/mmcli"
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 1. Try with JSON output first: mmcli -m any --location-get -J
	out, err := runner(execCtx, binPath, "-m", "any", "--location-get", "-J")
	if err == nil && len(out) > 0 {
		loc, parseErr := parseModemManagerJSON(out)
		if parseErr == nil && loc != nil {
			return loc, nil
		}
	}

	// 2. Fallback to standard text output: mmcli -m any --location-get
	outText, errText := runner(execCtx, binPath, "-m", "any", "--location-get")
	if errText == nil && len(outText) > 0 {
		loc, parseErr := parseModemManagerText(string(outText))
		if parseErr == nil && loc != nil {
			return loc, nil
		}
	}

	return nil, fmt.Errorf("modemmanager: no valid GPS fix available")
}

func parseModemManagerJSON(data []byte) (*backend.Location, error) {
	var resp mmcliLocationJSON
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	raw := resp.Modem.Location.GPSRaw
	if raw.Latitude != "" && raw.Longitude != "" {
		lat, err1 := strconv.ParseFloat(strings.TrimSpace(raw.Latitude), 64)
		lon, err2 := strconv.ParseFloat(strings.TrimSpace(raw.Longitude), 64)
		if err1 == nil && err2 == nil && isValidFix(lat, lon) {
			return &backend.Location{Latitude: lat, Longitude: lon}, nil
		}
	}

	// If raw GPS isn't available, check if NMEA sentences are available
	for _, sentence := range resp.Modem.Location.GPSNmea {
		if loc := ParseNMEA(sentence); loc != nil {
			return loc, nil
		}
	}

	return nil, fmt.Errorf("no valid coordinates in JSON output")
}

func parseModemManagerText(text string) (*backend.Location, error) {
	// Check for raw lat/long
	latMatch := reLatitude.FindStringSubmatch(text)
	lonMatch := reLongitude.FindStringSubmatch(text)
	if len(latMatch) >= 2 && len(lonMatch) >= 2 {
		lat, err1 := strconv.ParseFloat(latMatch[1], 64)
		lon, err2 := strconv.ParseFloat(lonMatch[1], 64)
		if err1 == nil && err2 == nil && isValidFix(lat, lon) {
			return &backend.Location{Latitude: lat, Longitude: lon}, nil
		}
	}

	// Check for any embedded NMEA strings
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "$") {
			if loc := ParseNMEA(line); loc != nil {
				return loc, nil
			}
		}
	}

	return nil, fmt.Errorf("no valid coordinates in text output")
}

func isValidFix(lat, lon float64) bool {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return false
	}
	// 0,0 is "Null Island" — typical indicator of uninitialized GPS data
	if lat == 0.0 && lon == 0.0 {
		return false
	}
	return true
}
