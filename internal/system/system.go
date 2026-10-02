// Package system reports static and slowly-changing facts about the host:
// architecture, kernel, hostname, and uptime. It deliberately knows nothing
// about identity, health thresholds, or containers -- just "what machine am
// I running on".
package system

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Info is a snapshot of host system facts.
type Info struct {
	Architecture string        `json:"architecture"`
	Kernel       string        `json:"kernel"`
	Hostname     string        `json:"hostname"`
	Uptime       time.Duration `json:"uptime"`
}

var procUptimePath = "/proc/uptime"

// Collect gathers current system facts.
func Collect() Info {
	info := Info{
		Architecture: runtime.GOARCH,
	}

	if v := readKernelRelease(); v != "" {
		info.Kernel = v
	}
	if h, err := os.Hostname(); err == nil {
		info.Hostname = h
	}
	info.Uptime = readUptime()

	return info
}

// FormatUptime renders a duration as Nodexa's compact "1h 24m" style, used
// by `nodexactl status`.
func FormatUptime(d time.Duration) string {
	d = d.Round(time.Minute)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute

	switch {
	case h > 0:
		return strconv.Itoa(int(h)) + "h " + strconv.Itoa(int(m)) + "m"
	default:
		return strconv.Itoa(int(m)) + "m"
	}
}

func readUptime() time.Duration {
	b, err := os.ReadFile(procUptimePath)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// readKernelRelease reads uname's release string via /proc, avoiding a
// cgo/syscall dependency for something this simple. Falls back to "" if
// unavailable (e.g. running unit tests on a non-Linux dev machine).
func readKernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
