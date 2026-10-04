package health

import (
	"regexp"
	"strconv"
	"strings"
)

// Parsers for the macOS tools health_darwin.go runs; kept build-tag free
// so they're tested everywhere.

// parseVMStat returns the page size and memory in use from `vm_stat`:
// active + wired + compressed pages, as Activity Monitor counts "Memory
// Used" (cached/inactive pages are reclaimable, so not counted).
func parseVMStat(out string) (usedBytes uint64, ok bool) {
	pageSize := uint64(4096)
	if m := regexp.MustCompile(`page size of (\d+) bytes`).FindStringSubmatch(out); m != nil {
		pageSize, _ = strconv.ParseUint(m[1], 10, 64)
	}
	pages := map[string]uint64{}
	for _, line := range strings.Split(out, "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(val), "."), 10, 64)
		if err == nil {
			pages[strings.TrimSpace(key)] = n
		}
	}
	active, okA := pages["Pages active"]
	wired, okW := pages["Pages wired down"]
	if !okA || !okW {
		return 0, false
	}
	return (active + wired + pages["Pages occupied by compressor"]) * pageSize, true
}

// parseSwapUsage parses `sysctl -n vm.swapusage`:
// "total = 2048.00M  used = 1024.50M  free = 1023.50M  (encrypted)".
func parseSwapUsage(out string) (total, used uint64, ok bool) {
	m := regexp.MustCompile(`total = ([\d.]+)([KMGT])\s+used = ([\d.]+)([KMGT])`).FindStringSubmatch(out)
	if m == nil {
		return 0, 0, false
	}
	return sizeBytes(m[1], m[2]), sizeBytes(m[3], m[4]), true
}

func sizeBytes(num, unit string) uint64 {
	v, _ := strconv.ParseFloat(num, 64)
	shift := map[string]uint{"K": 10, "M": 20, "G": 30, "T": 40}[unit]
	return uint64(v * float64(uint64(1)<<shift))
}

// parseLoadAvg parses `sysctl -n vm.loadavg`: "{ 1.23 1.45 1.67 }".
func parseLoadAvg(out string) (l1, l5, l15 float64, ok bool) {
	f := strings.Fields(strings.Trim(strings.TrimSpace(out), "{}"))
	if len(f) < 3 {
		return 0, 0, 0, false
	}
	var v [3]float64
	for i := range v {
		var err error
		if v[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return 0, 0, 0, false
		}
	}
	return v[0], v[1], v[2], true
}

// parseBootTime parses `sysctl -n kern.boottime`: "{ sec = 1696000000, ...".
func parseBootTime(out string) (unixSec int64, ok bool) {
	m := regexp.MustCompile(`sec = (\d+)`).FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	return v, err == nil
}

// parseGPUCores parses `system_profiler SPDisplaysDataType`, returning the
// first GPU's model and core count ("Total Number of Cores" is only listed
// for Apple Silicon; 0 otherwise).
func parseGPUCores(out string) (name string, cores int) {
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "Chipset Model":
			if name == "" {
				name = val
			}
		case "Total Number of Cores":
			if cores == 0 {
				cores, _ = strconv.Atoi(val)
			}
		}
	}
	return name, cores
}

// parseGPUUtilization returns the busiest GPU's "Device Utilization %" from
// `ioreg -r -c IOAccelerator`; ok is false when none is listed.
func parseGPUUtilization(out string) (percent float64, ok bool) {
	for _, m := range regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`).FindAllStringSubmatch(out, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			percent, ok = max(percent, v), true
		}
	}
	return min(percent, 100), ok
}

// parseTopCPU returns CPU use from the last "CPU usage: ... X% idle" line of
// `top -l 2`; the first sample is an average since boot.
func parseTopCPU(out string) (percent float64, ok bool) {
	ms := regexp.MustCompile(`CPU usage:.*?([\d.]+)% idle`).FindAllStringSubmatch(out, -1)
	if len(ms) == 0 {
		return 0, false
	}
	idle, err := strconv.ParseFloat(ms[len(ms)-1][1], 64)
	if err != nil {
		return 0, false
	}
	return max(0, min(100, 100-idle)), true
}
