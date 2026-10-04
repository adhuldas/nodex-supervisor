// Package health implements Nodexa Agent's system health monitoring:
// resource utilization plus a simple three-way status (agent/runtime/
// network) that nodexactl surfaces directly in `nodexactl status` and
// `nodexactl health`.
package health

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status is a coarse health verdict.
type Status string

const (
	StatusHealthy   Status = "healthy"
	StatusDegraded  Status = "degraded"
	StatusUnhealthy Status = "unhealthy"
)

// Report is a single health snapshot.
type Report struct {
	Overall Status `json:"overall"`

	Agent   Status `json:"agent"`
	Runtime Status `json:"runtime"`
	Network Status `json:"network"`

	LoadAvg1  float64 `json:"load_avg_1m"`
	LoadAvg5  float64 `json:"load_avg_5m"`
	LoadAvg15 float64 `json:"load_avg_15m"`
	// UptimeSeconds is time since boot; 0 when unreadable.
	UptimeSeconds   uint64  `json:"uptime_seconds"`
	CPUPercent      float64 `json:"cpu_percent"`
	MemUsedPercent  float64 `json:"mem_used_percent"`
	DiskUsedPercent float64 `json:"disk_used_percent"`

	// *Bytes fields are the raw totals readMemUsage/diskUsage already
	// compute internally to derive the percentages above -- exposed
	// separately since a percentage alone can't tell a dashboard how much
	// headroom (in absolute terms) a device actually has.
	MemTotalBytes  uint64 `json:"mem_total_bytes"`
	MemUsedBytes   uint64 `json:"mem_used_bytes"`
	DiskTotalBytes uint64 `json:"disk_total_bytes"`
	DiskUsedBytes  uint64 `json:"disk_used_bytes"`

	// Swap space, nil where /proc/meminfo doesn't exist (non-Linux);
	// a total of 0 means the device has no swap.
	SwapTotalBytes *uint64 `json:"swap_total_bytes,omitempty"`
	SwapUsedBytes  *uint64 `json:"swap_used_bytes,omitempty"`

	// TemperatureC is nil, not 0, when unavailable (many VMs and every
	// non-Linux dev machine have no thermal_zone0) -- unlike the metrics
	// above, 0C is a plausible real reading, so it can't double as "unknown".
	TemperatureC *float64 `json:"temperature_c,omitempty"`

	Timestamp time.Time `json:"timestamp"`
}

// RuntimeCheckFunc reports whether the container runtime is ready.
type RuntimeCheckFunc func() bool

// NetworkCheckFunc reports whether the device has network connectivity.
type NetworkCheckFunc func() bool

// Checker produces Reports, consulting injected checks for subsystems it
// does not own itself (container runtime, network), so this package has no
// direct dependency on internal/container or interface enumeration policy.
type Checker struct {
	DiskPath     string
	RuntimeCheck RuntimeCheckFunc
	NetworkCheck NetworkCheckFunc

	// mu guards the previous /proc/stat sample used to compute CPUPercent
	// as a delta between ticks -- Check is called both from the periodic
	// health-report ticker and concurrently from API requests.
	mu           sync.Mutex
	prevCPUIdle  uint64
	prevCPUTotal uint64
	havePrevCPU  bool
}

// NewChecker creates a Checker that reports on diskPath's filesystem usage.
func NewChecker(diskPath string, runtimeCheck RuntimeCheckFunc, networkCheck NetworkCheckFunc) *Checker {
	return &Checker{DiskPath: diskPath, RuntimeCheck: runtimeCheck, NetworkCheck: networkCheck}
}

// Check produces a fresh health report.
func (c *Checker) Check() Report {
	r := Report{
		Agent:     StatusHealthy, // reaching this code at all means the agent is alive
		Timestamp: time.Now().UTC(),
	}

	r.LoadAvg1, r.LoadAvg5, r.LoadAvg15 = readLoadAvg()
	r.UptimeSeconds = readUptime()
	r.CPUPercent = c.readCPUPercent()
	r.MemUsedPercent, r.MemTotalBytes, r.MemUsedBytes = readMemUsage()
	r.SwapTotalBytes, r.SwapUsedBytes = readSwapUsage()
	r.DiskUsedPercent, r.DiskTotalBytes, r.DiskUsedBytes = diskUsage(c.DiskPath)
	r.TemperatureC = readTemperatureC()

	if c.RuntimeCheck != nil && c.RuntimeCheck() {
		r.Runtime = StatusHealthy
	} else {
		r.Runtime = StatusDegraded
	}

	if c.NetworkCheck != nil && c.NetworkCheck() {
		r.Network = StatusHealthy
	} else {
		r.Network = StatusDegraded
	}

	r.Overall = overall(r)
	return r
}

func overall(r Report) Status {
	if r.DiskUsedPercent >= 95 || r.MemUsedPercent >= 95 {
		return StatusUnhealthy
	}
	if r.Runtime != StatusHealthy || r.Network != StatusHealthy || r.DiskUsedPercent >= 85 || r.MemUsedPercent >= 85 {
		return StatusDegraded
	}
	return StatusHealthy
}

var procLoadAvgPath = "/proc/loadavg"

// readLoadAvg returns the 1, 5 and 15 minute load averages (0 on failure).
func readLoadAvg() (float64, float64, float64) {
	b, err := os.ReadFile(procLoadAvgPath)
	if err != nil {
		return 0, 0, 0
	}
	var v [3]float64
	for i, f := range strings.Fields(string(b)) {
		if i == len(v) {
			break
		}
		v[i], _ = strconv.ParseFloat(f, 64)
	}
	return v[0], v[1], v[2]
}

var procUptimePath = "/proc/uptime"

func readUptime() uint64 {
	b, err := os.ReadFile(procUptimePath)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0
	}
	return uint64(v)
}

var procStatPath = "/proc/stat"

// readCPUPercent computes CPU utilization as the delta between this sample
// and the previous one, since /proc/stat's counters are cumulative since
// boot and a single snapshot can't express instantaneous usage. Returns 0
// on the first call (no previous sample yet) and whenever a sample can't
// be read, consistent with this file's other best-effort readers.
func (c *Checker) readCPUPercent() float64 {
	idle, total, ok := readCPUSample()
	if !ok {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	prevIdle, prevTotal, havePrev := c.prevCPUIdle, c.prevCPUTotal, c.havePrevCPU
	c.prevCPUIdle, c.prevCPUTotal, c.havePrevCPU = idle, total, true

	if !havePrev || total <= prevTotal {
		return 0
	}
	deltaTotal := total - prevTotal
	deltaIdle := idle - prevIdle
	if deltaIdle > deltaTotal {
		return 0
	}
	return float64(deltaTotal-deltaIdle) / float64(deltaTotal) * 100
}

// readCPUSample reads the aggregate "cpu" line of /proc/stat, returning its
// idle-ticks and total-ticks counters.
func readCPUSample() (idle, total uint64, ok bool) {
	f, err := os.Open(procStatPath)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}

	var sum uint64
	for _, field := range fields[1:] {
		v, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		sum += v
	}
	idleTicks, err := strconv.ParseUint(fields[4], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return idleTicks, sum, true
}

var thermalZoneGlob = "/sys/class/thermal/thermal_zone*/temp"

// readTemperatureC reads the primary thermal zone, in millidegrees C per
// the kernel's sysfs convention. Returns nil (not 0) if unavailable.
// Scans all thermal_zone*/temp files (sorted) and returns the first
// readable value, since different boards expose their zone under different
// indices (e.g. BeagleBone AM335x uses thermal_zone1, not thermal_zone0).
func readTemperatureC() *float64 {
	matches, err := filepath.Glob(thermalZoneGlob)
	if err != nil || len(matches) == 0 {
		return nil
	}
	sort.Strings(matches)
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		milliC, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			continue
		}
		c := milliC / 1000
		return &c
	}
	return nil
}

var procMemInfoPath = "/proc/meminfo"

// readMemUsage reads /proc/meminfo (whose MemTotal:/MemAvailable: values are
// in KiB) and returns used-percent alongside the raw byte totals it's
// derived from.
func readMemUsage() (usedPercent float64, totalBytes, usedBytes uint64) {
	f, err := os.Open(procMemInfoPath)
	if err != nil {
		return 0, 0, 0
	}
	defer f.Close()

	var totalKiB, availableKiB float64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKiB = parseMemInfoValue(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availableKiB = parseMemInfoValue(line)
		}
	}
	if totalKiB == 0 {
		return 0, 0, 0
	}
	usedPercent = (totalKiB - availableKiB) / totalKiB * 100
	totalBytes = uint64(totalKiB) * 1024
	usedBytes = uint64(totalKiB-availableKiB) * 1024
	return usedPercent, totalBytes, usedBytes
}

// readSwapUsage reads SwapTotal:/SwapFree: (KiB) from /proc/meminfo; nil,
// nil when it can't be read.
func readSwapUsage() (totalBytes, usedBytes *uint64) {
	f, err := os.Open(procMemInfoPath)
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	var totalKiB, freeKiB float64
	found := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "SwapTotal:"):
			totalKiB = parseMemInfoValue(line)
			found = true
		case strings.HasPrefix(line, "SwapFree:"):
			freeKiB = parseMemInfoValue(line)
		}
	}
	if !found {
		return nil, nil
	}
	total := uint64(totalKiB) * 1024
	used := uint64(max(totalKiB-freeKiB, 0)) * 1024
	return &total, &used
}

func parseMemInfoValue(line string) float64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0
	}
	return v
}

