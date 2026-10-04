package wifi

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// macOS has no /sys or NetworkManager: Wi-Fi hardware and link state come
// from networksetup and ifconfig. Parsers are build-tag free so they're
// tested everywhere.

type darwinPort struct {
	Name   string // "Wi-Fi", "Ethernet Adapter (en2)", ...
	Device string // "en0"
}

// parseHardwarePorts parses `networksetup -listallhardwareports`.
func parseHardwarePorts(out string) []darwinPort {
	var ports []darwinPort
	var cur darwinPort
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.TrimSpace(key) {
		case "Hardware Port":
			cur = darwinPort{Name: val}
		case "Device":
			cur.Device = val
			if cur.Name != "" && cur.Device != "" {
				ports = append(ports, cur)
			}
		}
	}
	return ports
}

func isWifiPort(p darwinPort) bool {
	n := strings.ToLower(p.Name)
	return strings.Contains(n, "wi-fi") || strings.Contains(n, "airport")
}

// isWiredPort matches real wired adapters, not Thunderbolt/bridge or
// phone-tethering ports.
func isWiredPort(p darwinPort) bool {
	n := strings.ToLower(p.Name)
	return strings.Contains(n, "ethernet") || strings.Contains(n, "lan")
}

// linkActive reports whether `ifconfig <dev>` says the interface has an
// active link. Requiring an IPv4 address here incorrectly hides connected
// Wi-Fi on networks that provide only IPv6 (or while DHCP is still pending).
func linkActive(ifconfig string) bool {
	for _, line := range strings.Split(ifconfig, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "status") && strings.EqualFold(strings.TrimSpace(val), "active") {
			return true
		}
	}
	return false
}

// parseAirportNetwork parses `networksetup -getairportnetwork`: "Current
// Wi-Fi Network: Home", or "You are not associated with an AirPort network."
func parseAirportNetwork(out string) string {
	const prefix = "Current Wi-Fi Network:"
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// parseSummarySSID parses the "SSID : name" line of `ipconfig getsummary`.
// Recent macOS redacts it without Location permission; that reads as none.
func parseSummarySSID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.TrimSpace(key) == "SSID" {
			ssid := strings.TrimSpace(val)
			if ssid == "" || strings.Contains(ssid, "redacted") {
				return ""
			}
			return ssid
		}
	}
	return ""
}

func darwinRun(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out)
}

func darwinPorts(ctx context.Context) []darwinPort {
	return parseHardwarePorts(darwinRun(ctx, "networksetup", "-listallhardwareports"))
}

func darwinWifiSupported(ctx context.Context) bool {
	for _, p := range darwinPorts(ctx) {
		if isWifiPort(p) {
			return true
		}
	}
	return false
}

// darwinConnections reports which of Wi-Fi and wired links are up, and the
// connected network's name when macOS lets us read it.
func darwinConnections(ctx context.Context) (hasWifi, hasEth bool, ssid string) {
	for _, p := range darwinPorts(ctx) {
		switch {
		case isWifiPort(p):
			if !linkActive(darwinRun(ctx, "ifconfig", p.Device)) {
				continue
			}
			hasWifi = true
			if ssid == "" {
				ssid = parseAirportNetwork(darwinRun(ctx, "networksetup", "-getairportnetwork", p.Device))
			}
			if ssid == "" {
				ssid = parseSummarySSID(darwinRun(ctx, "ipconfig", "getsummary", p.Device))
			}
			// wdutil needs root; as a daemon it can read the SSID macOS
			// redacts from unprivileged callers.
			if ssid == "" && os.Geteuid() == 0 {
				ssid = parseSummarySSID(darwinRun(ctx, "wdutil", "info"))
			}
		case isWiredPort(p):
			if linkActive(darwinRun(ctx, "ifconfig", p.Device)) {
				hasEth = true
			}
		}
	}
	return hasWifi, hasEth, ssid
}
